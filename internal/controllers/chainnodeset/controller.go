package chainnodeset

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/chainutils"
	"github.com/voluzi/cosmopilot/v2/internal/chainutils/sdkcmd"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
)

// Reconciler reconciles a ChainNode object
type Reconciler struct {
	client.Client
	ClientSet  *kubernetes.Clientset
	RestConfig *rest.Config
	Scheme     *runtime.Scheme
	recorder   record.EventRecorder
	opts       *controllers.ControllerRunOptions
}

func New(mgr ctrl.Manager, clientSet *kubernetes.Clientset, opts *controllers.ControllerRunOptions) (*Reconciler, error) {
	r := &Reconciler{
		Client:     mgr.GetClient(),
		ClientSet:  clientSet,
		RestConfig: mgr.GetConfig(),
		Scheme:     mgr.GetScheme(),
		recorder:   mgr.GetEventRecorderFor("chainnodeset-controller"),
		opts:       opts,
	}
	if err := r.setupWithManager(mgr); err != nil {
		return nil, err
	}
	return r, nil
}

//+kubebuilder:rbac:groups=cosmopilot.voluzi.com,resources=chainnodesets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=cosmopilot.voluzi.com,resources=chainnodesets/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=cosmopilot.voluzi.com,resources=chainnodesets/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=serviceaccounts,verbs=get;list;watch
//+kubebuilder:rbac:groups=networking.k8s.io,resources=ingresses,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch
//+kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=grpcroutes,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=tcproutes,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	nodeSet := &appsv1.ChainNodeSet{}
	if err := r.Get(ctx, req.NamespacedName, nodeSet); err != nil {
		if errors.IsNotFound(err) {
			// Request object not found, could have been deleted after reconcile request.
			return ctrl.Result{}, nil
		}
		logger.Error(err, "unable to fetch chainnodeset")
		return ctrl.Result{}, err
	}

	// Check if namespace is being terminated - if so, skip reconcile to avoid errors
	ns := &corev1.Namespace{}
	if err := r.Get(ctx, client.ObjectKey{Name: req.Namespace}, ns); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if ns.DeletionTimestamp != nil {
		logger.V(1).Info("namespace is being terminated, skipping reconcile")
		return ctrl.Result{}, nil
	}

	if nodeSet.Labels[controllers.LabelWorkerName] != r.opts.WorkerName {
		logger.V(1).Info("skipping chainnodeset due to worker-name mismatch.")
		return ctrl.Result{}, nil
	}

	if _, err := r.initializeLegacySignerServiceNames(ctx, nodeSet); err != nil {
		return ctrl.Result{}, err
	}

	if r.opts.DisableWebhooks {
		warnings, err := validateForReconcile(nodeSet)
		if err != nil {
			logger.Error(err, "spec is invalid")
			r.recorder.Eventf(nodeSet,
				corev1.EventTypeWarning,
				appsv1.ReasonInvalid,
				"spec is invalid: %v",
				err,
			)
			return ctrl.Result{}, err
		}
		if len(warnings) > 0 {
			logger.Error(nil, "validation warnings", "warnings", warnings)
		}
	}

	// Clearly log beginning and end of reconcile cycle
	logger.Info("starting reconcile")
	defer logger.Info("finishing reconcile")

	app, err := chainutils.NewApp(r.ClientSet, r.Scheme, r.RestConfig, nodeSet,
		nodeSet.Spec.App.GetSdkVersion(),
		[]sdkcmd.Option{sdkcmd.WithGenesisSubcommand(nodeSet.Spec.App.UseGenesisSubcommand())},
		chainutils.WithImage(nodeSet.Spec.App.GetImage()),
		chainutils.WithImagePullPolicy(nodeSet.Spec.App.ImagePullPolicy),
		chainutils.WithBinary(nodeSet.Spec.App.App),
		chainutils.WithPriorityClass(r.opts.GetDefaultPriorityClassName()),
	)
	if err != nil {
		return ctrl.Result{}, err
	}

	if nodeSet.Status.ChainID == "" {
		if err := r.updatePhase(ctx, nodeSet, appsv1.PhaseChainNodeSetInitializing); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Preflight every desired signer before deleting a stale one. A replacement that cannot deploy must
	// leave the old signing path intact instead of deleting it and then failing before children retarget.
	if err := r.preflightCosmosigners(ctx, nodeSet); err != nil {
		return ctrl.Result{}, err
	}
	if err := r.preflightRemovedSignerFallbacks(ctx, nodeSet); err != nil {
		return ctrl.Result{}, err
	}

	chainIDKnownAtStart := nodeSet.Status.ChainID != ""

	// Record replacement signer locks and complete any immediately-runnable Vault import while the
	// existing signing path is still intact. A failed replacement import must not delete the old signer.
	if recorded, err := r.initCosmosignerLocks(ctx, nodeSet); err != nil {
		return ctrl.Result{}, err
	} else if recorded {
		return ctrl.Result{Requeue: true}, nil
	}
	blockedSignerTargets, ready, err := r.prepareCosmosignerImports(ctx, nodeSet)
	if err != nil {
		return ctrl.Result{}, err
	} else if !ready {
		return ctrl.Result{RequeueAfter: appsv1.DefaultReconcilePeriod}, nil
	}

	// Tear down any managed signer the spec no longer desires before children are reconciled, and wait
	// for completion: a child switching back to its local/tmKMS signing path while old signer pods are
	// still terminating would put two signers on the same consensus key.
	if tornDown, err := r.reconcileSignerTeardown(ctx, nodeSet); err != nil {
		return ctrl.Result{}, err
	} else if !tornDown {
		logger.Info("waiting for cosmosigner teardown before reconciling children")
		return ctrl.Result{RequeueAfter: appsv1.DefaultReconcilePeriod}, nil
	}

	// Validators that initialize a new genesis must run before ensureGenesis: they produce the
	// genesis (and its ConfigMap) that the ChainNodeSet and every other node consume.
	if nodeSet.ShouldInitGenesis() {
		if err := r.ensureValidatorWithBlockedSignerTargets(ctx, nodeSet, blockedSignerTargets); err != nil {
			return ctrl.Result{}, err
		}
	}

	if err := r.ensureGenesis(ctx, app, nodeSet); err != nil {
		return ctrl.Result{}, err
	}

	// Record whether this chain's genesis was generated by an init validator the first time genesis
	// exists, so the no-webhook reconcile path can reject adding init validators to an external-genesis
	// chain even when no validators are recorded. Persisted HERE — before the discovery requeue below —
	// so a no-webhook edit on the next pass cannot exploit a still-nil marker (which
	// validateNoWebhookGenesisInitState would read, alongside empty validators, as an unknown legacy
	// chain and admit an init validator on an imported genesis). Nil-guarded: captured once, never flipped.
	if nodeSet.Status.ChainID != "" && nodeSet.Status.GenesisInitGenerated == nil {
		genInit := nodeSet.ShouldInitGenesis()
		nodeSet.Status.GenesisInitGenerated = &genInit
		if err := r.Status().Update(ctx, nodeSet); err != nil {
			return ctrl.Result{}, err
		}
	}

	// If the chain ID was only just discovered this pass (an external genesis, or an init validator that
	// produced it above), requeue — ensureGenesis has persisted the chain ID and GenesisInitGenerated is
	// recorded above — so the next reconcile records the per-signer locks (a no-op above while the chain
	// ID was empty) BEFORE ensureValidator/ensureNodes retarget child validators to the signer.
	if !chainIDKnownAtStart && nodeSet.Status.ChainID != "" && len(nodeSet.ResolveCosmosigners()) > 0 {
		return ctrl.Result{Requeue: true}, nil
	}

	// Once a genesis is available (chainID known), reconcile validators that consume an external
	// genesis. This also runs the validator cleanup, so it must execute even when no validator is
	// currently desired (e.g. the last validator was just removed from the spec) to delete the
	// stale validator ChainNodes. Doing it here—rather than gating on phase Running—also ensures
	// validator-only groups are created on the first reconcile, without depending on an owned
	// ChainNode event to trigger the requeue.
	if !nodeSet.ShouldInitGenesis() && nodeSet.Status.ChainID != "" {
		if err := r.ensureValidatorWithBlockedSignerTargets(ctx, nodeSet, blockedSignerTargets); err != nil {
			return ctrl.Result{}, err
		}
	}

	if err := r.ensureNodesWithBlockedSignerTargets(ctx, nodeSet, blockedSignerTargets); err != nil {
		return ctrl.Result{}, err
	}

	// Deploy (or tear down) the managed cosmosigner remote signer. This runs after ensureNodes so
	// the chain ID and target group nodes exist.
	if err := r.ensureCosmosigner(ctx, nodeSet); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.ensureSeedNodes(ctx, nodeSet); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.ensureUpgrades(ctx, nodeSet); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.ensureServices(ctx, nodeSet); err != nil {
		return ctrl.Result{}, err
	}

	// Reconcile gateway routes BEFORE legacy ingresses so we know whether the
	// replacement routes were actually applied. If Gateway API CRDs are missing,
	// ensureIngresses must preserve any Ingress whose name is now covered by a
	// gatewayRoutes entry, to avoid an exposure gap during migration.
	gatewayApplied, err := r.ensureGatewayRoutes(ctx, nodeSet)
	if err != nil {
		return ctrl.Result{}, err
	}

	if err := r.ensureIngresses(ctx, nodeSet, gatewayApplied); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.ensurePodDisruptionBudgets(ctx, nodeSet); err != nil {
		return ctrl.Result{}, err
	}

	if nodeSet.Status.Phase != appsv1.PhaseChainNodeSetRunning || nodeSet.GetLastUpgradeVersion() != nodeSet.Status.AppVersion {
		log.FromContext(ctx).Info("updating .status.appVersion", "version", nodeSet.GetLastUpgradeVersion())
		nodeSet.Status.AppVersion = nodeSet.GetLastUpgradeVersion()
		return ctrl.Result{}, r.updatePhase(ctx, nodeSet, appsv1.PhaseChainNodeSetRunning)
	}
	return ctrl.Result{RequeueAfter: appsv1.DefaultReconcilePeriod}, nil
}

// validateForReconcile validates a ChainNodeSet on the controller's no-webhook path. Reconcile
// only ever sees the current persisted object, never the previous spec, so it is validated with a
// nil old: Validate then enforces structural and status-gated invariants using the object's own
// status (notably Status.ChainID — i.e. that genesis already exists) without diffing the spec.
//
// The object is deliberately not validated against a copy of itself: doing so would make the
// old/new spec-diff guards compare the spec to itself and silently pass, giving false confidence
// that dangerous changes were rejected. Those diff guards require a genuine previous spec, which is
// unavailable here. To keep no-webhook clusters safe after genesis, this path adds conservative
// status-gated checks for the immutable genesis validator set: once a ChainNodeSet has a chainID,
// the genesis-initializing validators recorded in status (flagged Init, with a fingerprint of their
// signing material) must still be desired in the same groups with unchanged signing material, and no
// new genesis-initializing validator may appear — so removals, conversions, signing-material changes
// and additions are all rejected without a previous spec to diff against.
// The status-gated waiver in Validate stays conservative without an old spec — it only drops the
// .spec.genesis requirement when the current spec still describes an active genesis-initializing
// validator, so a running configuration with no derivable genesis is rejected rather than accepted.
func validateForReconcile(nodeSet *appsv1.ChainNodeSet) (admission.Warnings, error) {
	// The reserved-name rule normally runs on the admission create path; on the no-webhook path it
	// applies only while the object has never been reconciled (no chainID recorded), so legacy
	// names keep working.
	if err := appsv1.ValidateCosmosignerReservedNameNoWebhook(nodeSet.GetName(), nodeSet.Status.ChainID != ""); err != nil {
		return nil, err
	}
	if nodeSet.Status.ChainID != "" {
		if err := validateNoWebhookGenesisInitState(nodeSet); err != nil {
			return nil, err
		}
	}
	if err := validateNoWebhookCosmosignerState(nodeSet); err != nil {
		return nil, err
	}
	return nodeSet.Validate(nil)
}

// validateNoWebhookCosmosignerState reconstructs the parts of the admission guard that can be judged
// from status alone. Reconcile only ever sees the current persisted object, never the previous spec,
// so it enforces exactly the invariants that do NOT need an old/new diff:
//
//   - Modifying a still-present signer that already rolled out (recorded digest, but the current
//     digest differs) is rejected — changing the Vault key, replicas, or target set of a live signer
//     would make the validator sign with a key not in the on-chain set. A same-key config keeps the
//     digest identical and is allowed.
//
//   - The raft replica count is immutable once the cluster formed with it (re-rendering a bootstrap
//     list does not migrate the recorded membership). Enforced from the persisted replica count so it
//     also covers sentry signers, which record no signing digest.
//
//   - ADDING a validator-targeted signer with an unverifiable (pre-provisioned Vault/GCP) key AFTER
//     establishment is rejected, using the write-once at-establishment identity marker to tell it
//     apart from the establishing signer's own first rollout (which must be admitted — the digest is
//     only recorded after rollout, so keying on "chainID set + empty digest" would deadlock it).
//
//   - REMOVING a validator-targeted signer is fail-closed until rollout records its serving identity.
//     Afterwards, removal is admitted only when the validator's own path still resolves that identity.
func validateNoWebhookCosmosignerState(nodeSet *appsv1.ChainNodeSet) error {
	if nodeSet.Status.ChainID == "" {
		return nil
	}

	desired := map[string]appsv1.ResolvedSigner{}
	for _, s := range nodeSet.ResolveCosmosigners() {
		desired[s.Name] = s
	}

	// REMOVED signers: pre-rollout validator targets fail closed; rolled-out signers can only be removed
	// when the same validator still resolves the recorded serving identity through its own path.
	for i := range nodeSet.Status.Cosmosigners {
		st := &nodeSet.Status.Cosmosigners[i]
		if _, ok := desired[st.Name]; ok {
			continue
		}
		if st.ServingIdentity != "" && nodeSet.ServedValidatorHasMultipleInstances(st.ServingGroup) {
			return fmt.Errorf("cosmosigner %q cannot be removed from multi-instance validator group %q (webhooks disabled): the group currently represents one signer-held validator identity, but removing the signer would restore per-instance validator identities", st.Name, st.ServingGroup)
		}
		if st.ServingIdentity != "" &&
			!nodeSet.ServedValidatorResolvesIdentity(st.ServingGroup, st.ServingIdentity) {
			return fmt.Errorf("cosmosigner %q cannot be removed (webhooks disabled): the validator it served would fall back to a local key different from the on-chain consensus key — restore the signer, or migrate the validator's own signing path to the same key first", st.Name)
		}
		if st.ServingIdentity == "" && st.SigningDigest != "" {
			return fmt.Errorf("cosmosigner %q cannot be removed (webhooks disabled): it served a validator but its recorded identity predates this version and cannot be verified — restore the signer so the controller can record it, or remove it with webhooks enabled", st.Name)
		}
		if st.ServingIdentity == "" && st.SigningDigest == "" {
			if st.ServingGroup != "" {
				return fmt.Errorf("cosmosigner %q cannot be removed (webhooks disabled): its validator rollout identity has not been recorded yet, so the controller cannot prove the resulting local signing path is safe — restore the signer until rollout completes, or perform the removal with webhooks enabled", st.Name)
			}
			if st.AtEstablishment == nil {
				return fmt.Errorf("cosmosigner %q cannot be removed (webhooks disabled): its pre-rollout target kind predates the status marker and cannot be verified — restore the signer with webhooks enabled so the controller can record whether it serves a validator", st.Name)
			}
		}
		// A SENTRY signer whose key was registered in the immutable genesis validator set at
		// establishment (AtEstablishment holds its identity; validator-targeted signers are handled by
		// the ServingIdentity check above) cannot be removed: the genesis validator would be left with no
		// signing path. A non-genesis sentry records an empty marker and is freely removable.
		if st.ServingIdentity == "" && st.AtEstablishment != nil && *st.AtEstablishment != "" {
			return fmt.Errorf("cosmosigner %q cannot be removed (webhooks disabled): its key is registered in the immutable genesis validator set, so removing it would leave that genesis validator without a signer — remove it with webhooks enabled, or keep the signer", st.Name)
		}
	}

	// PRESENT signers: modification and post-establishment-addition guards.
	for _, s := range nodeSet.ResolveCosmosigners() {
		st := nodeSet.GetCosmosignerStatus(s.Name)

		if st != nil {
			// The raft replica count is immutable once the cluster formed with it (re-rendering a
			// bootstrap list does not migrate the recorded membership). Enforced from the persisted count
			// so it also covers sentry signers, which record no signing digest.
			if st.Replicas != nil && *st.Replicas != s.Spec.GetReplicas() {
				return fmt.Errorf("cosmosigner %q replicas are immutable after deployment (webhooks disabled): changing them does not migrate the raft membership and can break quorum", s.Name)
			}
			// The PVC template is immutable too: StatefulSet volumeClaimTemplates cannot be updated and
			// existing claims stay at their old size/class, so a change would be silently ignored.
			if st.StateStorageSize != "" &&
				!appsv1.CosmosignerStateStorageEqual(st.StateStorageSize, st.StateStorageClassName, s.Spec.GetStateStorageSize(), s.Spec.StorageClassName) {
				return fmt.Errorf("cosmosigner %q state storage (size/class) is immutable after deployment (webhooks disabled): its raft state PVCs cannot be resized or moved — remove the signer and re-add it", s.Name)
			}
			// A signer that was responsible for an on-chain consensus key at establishment (non-empty
			// AtEstablishment) but has NOT yet rolled out (no SigningDigest) must keep serving that exact
			// identity until it does. The digest/serving guards below only take over once a digest is
			// recorded, and sentries never record one, so without this the pre-digest window admits:
			//   - demoting the served validator to a sentry / dropping .spec.validator while a pre-provisioned
			//     Vault/GCP identity is unchanged — ensureNodes then stops targeting the validator and its
			//     on-chain key loses its signing path; and
			//   - rotating a genesis-registered software sentry key.
			// "Still serving" is proven by a validator target resolving the recorded identity, OR by the
			// signer still being the SAME genesis-registered software sentry (its key still in
			// init.genesisValidators). A non-genesis sentry records "" and is unaffected; a post-establishment
			// validator addition keeps a nil marker and is judged by the addition guard below instead.
			if st.SigningDigest == "" && st.AtEstablishment != nil && *st.AtEstablishment != "" {
				// The signer must still serve the recorded identity through the SAME group it was
				// pinned to at establishment. The served group MUST have been recorded (ServingGroup
				// non-empty): a signer targeting multiple groups could otherwise move validator-ness to a
				// sibling group with the same backend identity, and even a SINGLE-target top-level signer can
				// retarget .spec.cosmosigner.nodeGroups from [a] to [b] while keeping the same status entry,
				// cardinality and identity — so cardinality alone cannot tell a retarget from an unchanged
				// config. A legacy marker with no ServingGroup therefore cannot be verified for validator-ness
				// and is rejected (repair with webhooks enabled); it only occurs on an intermediate
				// pre-release status, never after a release, since SetEstablishedChainID records the served
				// group with the marker. A genesis sentry (no served group) is still admitted below.
				stillValidator := s.TargetsValidator() &&
					st.ServingGroup != "" &&
					s.ValidatorGroup == st.ServingGroup &&
					s.ValidatorTargetedIdentity() == *st.AtEstablishment
				stillGenesisSentry := nodeSet.GenesisSentryEstablishmentIdentity(s) == *st.AtEstablishment
				if !stillValidator && !stillGenesisSentry {
					return fmt.Errorf("cosmosigner %q was responsible for an on-chain consensus key at establishment but has not recorded a rollout digest (webhooks disabled): it must keep serving that exact validator/genesis key until the digest is recorded — a demotion, retarget, sibling-group swap, or key change here would leave the on-chain key without its signing path; repair with webhooks enabled", s.Name)
				}
			}
			if st.SigningDigest != "" {
				// Modifying a still-present signer that already rolled out (recorded digest, current digest
				// differs) is rejected — changing the Vault key, replicas, or target set of a live signer
				// would make the validator sign with a key not in the on-chain set. A same-key config keeps
				// the digest identical and is allowed.
				if s.Digest() != st.SigningDigest {
					return fmt.Errorf("cosmosigner %q signing configuration is immutable after the chain is established (webhooks disabled): the targeted validator's key is fixed on-chain", s.Name)
				}
				// The digest hashes the backend identity, replicas and target-group NAMES — not whether the
				// served group still contains the validator. Converting the served group into a regular node
				// group keeps a Vault/GCP digest identical while removing the validator, so additionally
				// require the signer to still resolve the recorded serving identity — and through the SAME
				// group it served: swapping validator-ness between two targeted groups keeps the
				// identity and digest intact while the original on-chain validator loses its signing path.
				if st.ServingIdentity != "" {
					if s.ValidatorTargetedIdentity() != st.ServingIdentity {
						return fmt.Errorf("cosmosigner %q: the validator it was serving can no longer be resolved (webhooks disabled) — removing or converting the served validator would leave its on-chain key without its signing path", s.Name)
					}
					if s.ValidatorGroup != st.ServingGroup {
						return fmt.Errorf("cosmosigner %q served the validator in group %q (webhooks disabled) — moving validator-ness elsewhere would leave that validator's on-chain key without its signing path", s.Name, st.ServingGroup)
					}
				} else if !s.TargetsValidator() || len(s.TargetGroups) > 1 {
					// A LEGACY digest (recorded before the serving fields existed) carries no served
					// group, so the group demotion check above cannot run. The digest
					// hashes the backend identity, replica count and target-group NAMES — not WHICH targeted
					// group is the validator — so it stays identical when validator-ness moves between the
					// targeted groups. Two cases are therefore unverifiable from status alone and rejected:
					//   - the signer no longer targets a validator at all (the served group was demoted); or
					//   - it still targets a validator but among MULTIPLE groups, so a no-webhook edit could
					//     have demoted the originally-served group and promoted a sibling (e.g. served group
					//     a → b) with the digest unchanged; the next reconcile would then backfill the WRONG
					//     serving group, permanently losing the original validator's signing path.
					// A single-target validator signer is safe: it has no sibling to swap with, and a plain
					// demotion falls into the first case. Keep such signers targeting exactly the validator
					// so the next reconcile can backfill the serving fields, after which the precise check
					// applies; repair anything else with webhooks enabled.
					return fmt.Errorf("cosmosigner %q: its recorded identity predates this version and its validator target cannot be verified from status alone (webhooks disabled) — the served validator may have been demoted or swapped with a sibling group; reduce it to the single validator it serves so the controller can record the serving identity, or repair the configuration with webhooks enabled", s.Name)
				}
				// A matching digest proves this exact signer identity rolled out and served, so the
				// at-establishment guard below must not re-judge it.
				continue
			}
		}

		// ADDING a validator-targeted signer AFTER establishment is rejected unless its key source is
		// provably the registered one. A signer present at establishment has a status entry whose
		// write-once AtEstablishment marker equals its identity (recorded atomically with the chain
		// ID); a post-establishment addition has no entry (never reconciled), an entry with a NIL
		// marker (its first reconcile ran after establishment — SetEstablishedChainID never runs
		// again, so the marker stays nil), or an entry whose marker differs. The waiver requires the
		// backend to reference/import the validator's own key (software or vault.uploadGenerated)
		// AND that key's secret to be status-pinned — only true for a genesis-init target, whose
		// recorded genesis fingerprint includes privateKeySecret. An external-genesis or
		// create-validator target has no such pin, so the same no-webhook edit could swap
		// privateKeySecret alongside the signer, deploying a key not in the validator set.
		if s.TargetsValidator() {
			addedAfterEstablishment := st == nil ||
				st.AtEstablishment == nil ||
				s.ValidatorTargetedIdentity() != *st.AtEstablishment
			if addedAfterEstablishment {
				c := s.Spec
				importsRegisteredKey := signerTargetInitializesGenesis(nodeSet, s) &&
					(c.UsesSoftwareBackend() || (c.UsesVaultBackend() && c.VaultUploadsGenerated(true)))
				if !importsRegisteredKey {
					return fmt.Errorf("cosmosigner %q: a validator-targeted signer cannot be added to an established chain with webhooks disabled — its key cannot be verified against the on-chain validator key from status alone; perform the migration with webhooks enabled", s.Name)
				}
			}
		}
	}

	return nil
}

func validateNoWebhookGenesisInitState(nodeSet *appsv1.ChainNodeSet) error {
	type desiredInit struct {
		group  string
		digest string
	}
	desired := map[string]desiredInit{}

	if nodeSet.Spec.Validator != nil && nodeSet.Spec.Validator.Init != nil {
		name := fmt.Sprintf("%s-validator", nodeSet.GetName())
		desired[name] = desiredInit{
			group:  appsv1.ReservedValidatorGroupName,
			digest: nodeSet.Spec.Validator.GenesisSigningFingerprint(fmt.Sprintf("%s-priv-key", name)),
		}
	}
	for _, group := range nodeSet.Spec.Nodes {
		if group.Validator == nil || group.Validator.Init == nil || group.GetInstances() == 0 {
			continue
		}
		instances := group.GetInstances()
		// A cosmosigner-targeted group holds ONE consensus identity: only instance 0 is a genesis
		// validator (the other instances are redundant signing endpoints and are never recorded as
		// init validators), so only its fingerprint belongs in the desired init set.
		if nodeSet.IsCosmosignerTargetGroup(group.Name) {
			instances = 1
		}
		for i := 0; i < instances; i++ {
			name := validatorNodeName(nodeSet, group.Name, i)
			cfg := deriveGroupValidatorConfig(nodeSet, group.Name, i, group.GetInstances(), group.Validator)
			desired[name] = desiredInit{
				group:  group.Name,
				digest: cfg.GenesisSigningFingerprint(fmt.Sprintf("%s-priv-key", name)),
			}
		}
	}

	// No genesis validators recorded yet. With no desired init validators there is nothing to enforce.
	// Otherwise reject adding init validators when the genesis is known to have been imported from an
	// external source (an init validator would regenerate a fresh genesis for a running chain); allow
	// when it was init-generated, or when the source is unknown — a pre-marker legacy chain whose
	// .status.validators ensureValidator will backfill on this reconcile (rejecting it would strand a
	// running no-webhook chain). The full checks apply on subsequent reconciles once the slice exists.
	if len(nodeSet.Status.Validators) == 0 {
		if len(desired) > 0 && nodeSet.Status.GenesisInitGenerated != nil && !*nodeSet.Status.GenesisInitGenerated {
			for name, d := range desired {
				return fmt.Errorf("genesis-initializing validator %q (group %q) cannot be added with webhooks disabled to a ChainNodeSet that imported an external genesis", name, d.group)
			}
		}
		return nil
	}

	// Walk the recorded genesis validator set (entries flagged Init). Each must still be desired as a
	// genesis-initializing validator, in the same group, with unchanged signing material — its
	// consensus key and membership are baked into the immutable genesis. Non-init validators
	// (e.g. createValidator) are not genesis-protected and are ignored.
	seen := map[string]struct{}{}
	for _, validator := range nodeSet.Status.Validators {
		if !validator.Init {
			continue
		}
		want, ok := desired[validator.Name]
		if !ok {
			return fmt.Errorf("genesis-initializing validator %q cannot be removed or converted with webhooks disabled after genesis has been created: it is part of the immutable genesis validator set", validator.Name)
		}
		if validator.Group != want.group {
			return fmt.Errorf("genesis-initializing validator %q is recorded in group %q but the spec now places it in group %q", validator.Name, validator.Group, want.group)
		}
		if validator.SigningKeyDigest != "" && validator.SigningKeyDigest != want.digest {
			return fmt.Errorf("signing material or genesis parameters of genesis-initializing validator %q cannot be changed with webhooks disabled after genesis has been created: they are part of the immutable genesis validator set", validator.Name)
		}
		seen[validator.Name] = struct{}{}
	}

	// .status.validators is populated (the empty/legacy case returned above), so it reflects the genesis
	// validator set. Any desired init validator not already recorded is being added to an immutable
	// genesis that does not include it — reject it. This also covers adding init to an external-genesis
	// chain whose recorded validators are all non-init (createValidator).
	for name, d := range desired {
		if _, ok := seen[name]; !ok {
			return fmt.Errorf("genesis-initializing validator %q (group %q) cannot be added with webhooks disabled after genesis has been created: it is not part of the immutable genesis validator set", name, d.group)
		}
	}

	return nil
}

func (r *Reconciler) updatePhase(ctx context.Context, nodeSet *appsv1.ChainNodeSet, phase appsv1.ChainNodeSetPhase) error {
	if nodeSet.Status.Phase == phase {
		return nil
	}
	log.FromContext(ctx).Info("updating .status.phase", "phase", phase)
	nodeSet.Status.Phase = phase
	return r.Status().Update(ctx, nodeSet)
}

// setupWithManager sets up the controller with the Manager.
func (r *Reconciler) setupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1.ChainNodeSet{}).
		Owns(&appsv1.ChainNode{}).
		WithEventFilter(GenerationChangedPredicate{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: r.opts.WorkerCount}).
		Complete(r)
}
