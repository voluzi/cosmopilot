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

	// Validators that initialize a new genesis must run before ensureGenesis: they produce the
	// genesis (and its ConfigMap) that the ChainNodeSet and every other node consume.
	if nodeSet.ShouldInitGenesis() {
		if err := r.ensureValidator(ctx, nodeSet); err != nil {
			return ctrl.Result{}, err
		}
	}

	if err := r.ensureGenesis(ctx, app, nodeSet); err != nil {
		return ctrl.Result{}, err
	}

	// Once a genesis is available (chainID known), reconcile validators that consume an external
	// genesis. This also runs the validator cleanup, so it must execute even when no validator is
	// currently desired (e.g. the last validator was just removed from the spec) to delete the
	// stale validator ChainNodes. Doing it here—rather than gating on phase Running—also ensures
	// validator-only groups are created on the first reconcile, without depending on an owned
	// ChainNode event to trigger the requeue.
	if !nodeSet.ShouldInitGenesis() && nodeSet.Status.ChainID != "" {
		if err := r.ensureValidator(ctx, nodeSet); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Record whether this chain's genesis was generated by an init validator the first time genesis
	// exists, so the no-webhook reconcile path can reject adding init validators to an external-genesis
	// chain even when no validators are recorded. Nil-guarded: captured once and never flipped.
	if nodeSet.Status.ChainID != "" && nodeSet.Status.GenesisInitGenerated == nil {
		genInit := nodeSet.ShouldInitGenesis()
		nodeSet.Status.GenesisInitGenerated = &genInit
		if err := r.Status().Update(ctx, nodeSet); err != nil {
			return ctrl.Result{}, err
		}
	}

	if err := r.ensureNodes(ctx, nodeSet); err != nil {
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
	if nodeSet.Status.ChainID != "" {
		if err := validateNoWebhookGenesisInitState(nodeSet); err != nil {
			return nil, err
		}
	}
	return nodeSet.Validate(nil)
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
		for i := 0; i < instances; i++ {
			name := validatorNodeName(nodeSet, group.Name, i)
			cfg := deriveGroupValidatorConfig(nodeSet, group.Name, i, instances, group.Validator)
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
