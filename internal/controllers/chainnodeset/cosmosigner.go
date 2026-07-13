package chainnodeset

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
	"github.com/voluzi/cosmopilot/v2/internal/cosmosigner"
	"github.com/voluzi/cosmopilot/v2/pkg/utils"
)

// reconcileSignerTeardown removes the managed signers a ChainNodeSet no longer desires — those with a
// recorded status entry but no longer produced by ResolveCosmosigners (the top-level or a per-group
// block was dropped, or a validator group shrank). It runs EARLY in Reconcile, before children switch
// back to their local/tmKMS signing path, and reports whether every removed signer's teardown is
// COMPLETE (StatefulSet and PVCs gone): callers must not reconcile children until it is, or the old
// signer pods (deletion is asynchronous) could briefly sign the same consensus key as the restored
// local signer. Each signer's resources are name-scoped, so per-signer teardown never touches another.
func (r *Reconciler) reconcileSignerTeardown(ctx context.Context, nodeSet *appsv1.ChainNodeSet) (bool, error) {
	desired := map[string]struct{}{}
	for _, s := range nodeSet.ResolveCosmosigners() {
		desired[s.Name] = struct{}{}
	}

	// Snapshot the recorded signer names: RemoveCosmosignerStatus mutates the slice we would iterate.
	var stale []string
	for _, st := range nodeSet.Status.Cosmosigners {
		if _, ok := desired[st.Name]; !ok {
			stale = append(stale, st.Name)
		}
	}

	allDone := true
	changed := false
	for _, name := range stale {
		if err := cosmosigner.Undeploy(ctx, r.Client, nodeSet, nodeSet.GetNamespace(), name); err != nil {
			return false, err
		}
		// Teardown completion gates both the status-entry drop and the caller's child reconciliation.
		// Clearing the entry while the old raft cluster is still terminating would let a remove-and-
		// immediate-re-add bypass the replica guard and bind the surviving PVCs with stale membership.
		tornDown, err := cosmosigner.IsTornDown(ctx, r.Client, nodeSet, nodeSet.GetNamespace(), name)
		if err != nil {
			return false, err
		}
		if !tornDown {
			allDone = false
			continue
		}
		nodeSet.RemoveCosmosignerStatus(name)
		changed = true
	}

	if changed {
		// Tolerate a conflict: the drop is idempotent, so a concurrent update just defers it to the
		// next reconcile rather than spinning the workqueue with no progress.
		if err := r.Status().Update(ctx, nodeSet); err != nil && !errors.IsConflict(err) {
			return false, err
		}
	}
	return allDone, nil
}

// ensureCosmosigner deploys every managed cosmosigner a ChainNodeSet runs (the top-level
// .spec.cosmosigner plus each per-group .spec.nodes[].cosmosigner, expanded per instance for a
// multi-instance validator group). It is a no-op until the chain ID is known, since a signer's config
// requires it. Teardown of REMOVED signers is driven earlier by reconcileSignerTeardown so a child's
// signing path is never switched while old signer pods can still sign.
func (r *Reconciler) ensureCosmosigner(ctx context.Context, nodeSet *appsv1.ChainNodeSet) error {
	signers := nodeSet.ResolveCosmosigners()
	if len(signers) == 0 {
		return nil
	}

	// The signer config needs the chain ID; wait for genesis to be available.
	if nodeSet.Status.ChainID == "" {
		return nil
	}

	// PERSIST a status entry for every desired signer BEFORE creating any signer resource:
	// reconcileSignerTeardown derives the set of removable signers from status, so a signer whose
	// resources exist without an entry (e.g. after a crash between resource creation and the batched
	// status write below) would never be undeployed — it would keep dialing and signing after the
	// spec dropped it. Writing the entries first makes that window crash-safe: no entry, no resources.
	entriesAdded := false
	for _, s := range signers {
		if nodeSet.GetCosmosignerStatus(s.Name) == nil {
			nodeSet.EnsureCosmosignerStatus(s.Name)
			entriesAdded = true
		}
	}
	if entriesAdded {
		if err := r.Status().Update(ctx, nodeSet); err != nil {
			return err
		}
	}

	changed := false
	for _, s := range signers {
		c, err := r.reconcileSigner(ctx, nodeSet, s)
		if err != nil {
			return err
		}
		changed = changed || c
	}
	if changed {
		return r.Status().Update(ctx, nodeSet)
	}
	return nil
}

// reconcileSigner deploys one resolved signer and records its persisted status invariants, mutating
// nodeSet.Status in memory. It returns whether a status field changed (so the caller batches a single
// status write for all signers). Nothing is persisted here directly except through that batched write.
func (r *Reconciler) reconcileSigner(ctx context.Context, nodeSet *appsv1.ChainNodeSet, s appsv1.ResolvedSigner) (bool, error) {
	params, err := r.cosmosignerParams(ctx, nodeSet, s)
	if err != nil {
		return false, err
	}

	// The status entry was persisted by ensureCosmosigner before any resource creation (so teardown
	// can always discover this signer from status, even across a crash).
	changed := false
	st := nodeSet.EnsureCosmosignerStatus(s.Name)

	// Import a locally-generated key into Vault when requested (one-shot, once-only). Returns pending
	// when the validator has not produced its key yet.
	importPending, importChanged, err := r.maybeImportCosmosignerKey(ctx, nodeSet, s, params)
	if err != nil {
		return false, err
	}
	changed = changed || importChanged

	// Render once: the ConfigMap contents and the pod-template ROLLME hash come from the same render.
	configYAML, err := params.ConfigYAML()
	if err != nil {
		return false, err
	}
	if err := r.applyCosmosignerObject(ctx, nodeSet, params.ConfigMap(configYAML)); err != nil {
		return false, err
	}
	if err := r.applyCosmosignerObject(ctx, nodeSet, params.RaftService()); err != nil {
		return false, err
	}
	if err := r.applyCosmosignerObject(ctx, nodeSet, params.DiscoveryService()); err != nil {
		return false, err
	}

	// Do not roll out the signer until the validator's generated key has been imported into Vault,
	// otherwise the signer would come up against an empty/stale transit key while the validator
	// registers the local pubkey. An ALREADY-RUNNING signer is scaled to zero for the same reason:
	// after a source/target change it would keep signing with the previously imported key while the
	// re-import is pending. A later reconcile completes the import and (re)deploys the signer.
	if importPending {
		if _, err := cosmosigner.ScaleDown(ctx, r.Client, nodeSet, nodeSet.GetNamespace(), params.Name); err != nil {
			return false, err
		}
		return changed, nil
	}

	sts, err := params.StatefulSet(configYAML)
	if err != nil {
		return false, err
	}
	if err := r.applyCosmosignerObject(ctx, nodeSet, sts); err != nil {
		return false, err
	}

	// Record persisted invariants only after this signer's CURRENT generation is fully rolled out
	// (observed + all replicas updated and ready, read from the live object — the freshly rendered sts
	// carries no status). A config that never worked therefore stays correctable.
	//
	//   - Replicas is recorded for EVERY signer (validator-targeted and sentry alike): the raft
	//     membership baked into the per-pod state cannot be migrated by re-rendering a bootstrap list,
	//     so the no-webhook path rejects a later replica change from this recorded value.
	//   - The signing digest, serving identity/group/instance are only meaningful when the signer serves
	//     a validator: a sentry-mode signer's key lives out-of-band and must stay add/remove/rotate-able.
	needReplicas := st.Replicas == nil
	needServing := s.TargetsValidator() && st.SigningDigest == ""
	// Backfilled independently of Replicas so a signer that recorded its replica count before the
	// storage fields existed still gets its PVC template locked.
	needStorage := st.StateStorageSize == ""
	if needReplicas || needServing || needStorage {
		rolledOut, err := cosmosigner.IsRolledOut(ctx, r.Client, nodeSet.GetNamespace(), params.Name, params.Replicas)
		if err != nil {
			return false, err
		}
		if rolledOut {
			if needReplicas {
				st.Replicas = ptr.To(params.Replicas)
				changed = true
			}
			if needStorage {
				// Locks the PVC template on the no-webhook path and across a remove-and-re-add while the
				// old PVCs may still exist.
				st.StateStorageSize = s.Spec.GetStateStorageSize()
				st.StateStorageClassName = s.Spec.StorageClassName
				changed = true
			}
			if needServing {
				// The serving identity + group + instance let the no-webhook path judge a later REMOVAL:
				// it is admitted only when the SERVED validator's own signing path still resolves this
				// identity.
				st.SigningDigest = s.Digest()
				st.ServingIdentity = s.ValidatorTargetedIdentity()
				st.ServingGroup = s.ValidatorGroup
				st.ServingInstance = s.ValidatorInstance
				changed = true
			}
		}
	}
	return changed, nil
}

// cosmosignerParams resolves the builder parameters for one signer, ensuring the software key secret
// exists when the software backend is used.
func (r *Reconciler) cosmosignerParams(ctx context.Context, nodeSet *appsv1.ChainNodeSet, s appsv1.ResolvedSigner) (cosmosigner.Params, error) {
	c := s.Spec

	// Signer pods/resources must never inherit internal selector labels (group/global Service
	// selectors, P2P peer discovery, resource-cleanup selectors) — see
	// controllers.CosmosignerReservedSelectorLabels. The generated global ingress/gateway membership
	// label names are per-nodeset and appended below.
	exclude := controllers.CosmosignerReservedSelectorLabels()
	for _, ingress := range nodeSet.Spec.Ingresses {
		exclude = append(exclude, ingress.GetName(nodeSet))
	}
	for _, gw := range nodeSet.Spec.GatewayRoutes {
		exclude = append(exclude, gw.GetName(nodeSet))
	}
	labels := utils.ExcludeMapKeys(WithChainNodeSetLabels(nodeSet, map[string]string{
		controllers.LabelChainNodeSet: nodeSet.GetName(),
	}), exclude...)

	backend, err := r.cosmosignerBackend(ctx, nodeSet, s)
	if err != nil {
		return cosmosigner.Params{}, err
	}

	return cosmosigner.Params{
		Name:               s.Name,
		Namespace:          nodeSet.GetNamespace(),
		OwnerUID:           nodeSet.GetUID(),
		ChainID:            nodeSet.Status.ChainID,
		Image:              c.GetImage(r.opts.CosmosignerImage),
		Replicas:           c.GetReplicas(),
		LogLevel:           c.GetLogLevel(),
		StateStorageSize:   c.GetStateStorageSize(),
		StorageClassName:   c.StorageClassName,
		Resources:          c.GetResources(),
		RaftTLSSecret:      c.RaftTLSSecret,
		ServiceAccountName: c.GetServiceAccountName(),
		Backend:            backend,
		Labels:             labels,
		TargetSelector: map[string]string{
			controllers.LabelChainNodeSet:      nodeSet.GetName(),
			controllers.LabelCosmosignerTarget: s.Name,
		},
	}, nil
}

// cosmosignerBackend translates a signer's CRD backend into the builder backend. Key material is never
// generated here: the software backend references either an explicit secret or the targeted validator
// instance's own key secret (produced by its genesis/create-validator flow), so the signer always
// signs with the exact consensus key registered on-chain.
func (r *Reconciler) cosmosignerBackend(ctx context.Context, nodeSet *appsv1.ChainNodeSet, s appsv1.ResolvedSigner) (cosmosigner.Backend, error) {
	c := s.Spec
	switch {
	case c.UsesSoftwareBackend():
		secretName := s.SoftwareKeySecret
		if secretName == "" {
			return cosmosigner.Backend{}, fmt.Errorf("cosmosigner software backend has no resolvable private-key secret")
		}
		// Preflight the key secret whenever no controller flow will (re)create it, instead of rolling
		// out signer pods stuck on a missing Secret mount:
		//   - sentry mode (no validator target): the key is registered on-chain out-of-band and always
		//     user-supplied (pre-provision it before genesis is fixed — a key minted here could never
		//     be in the validator set);
		//   - a targeted EXTERNAL-GENESIS validator: its explicit privateKeySecret is user-supplied
		//     too — only init/createValidator validator flows generate their own key secret;
		//   - a targeted init/createValidator validator whose key flow already COMPLETED (its pubkey
		//     is recorded in status): the key secret is never regenerated, so a deleted Secret stays
		//     deleted.
		// Only a target whose key-generation flow is still pending skips the check — the child ChainNode
		// produces the secret during that flow.
		keyFlowPending := false
		if s.ValidatorGroup != "" {
			generates := false
			if s.ValidatorGroup == appsv1.ReservedValidatorGroupName {
				v := nodeSet.Spec.Validator
				generates = v != nil && (v.Init != nil || v.CreateValidator != nil)
			} else {
				for _, g := range nodeSet.Spec.Nodes {
					if g.Name == s.ValidatorGroup && g.Validator != nil {
						generates = g.Validator.Init != nil || g.Validator.CreateValidator != nil
					}
				}
			}
			if generates {
				keyFlowPending = true
				instance := 0
				if s.ValidatorInstance != nil {
					instance = *s.ValidatorInstance
				}
				vname := validatorNodeName(nodeSet, s.ValidatorGroup, instance)
				for _, v := range nodeSet.Status.Validators {
					if v.Name == vname && v.PubKey != "" {
						// The targeted validator already registered its key; the flow will not run again.
						keyFlowPending = false
					}
				}
			}
		}
		if !keyFlowPending {
			exists, err := r.secretHasKey(ctx, nodeSet.GetNamespace(), secretName, privKeyFilename)
			if err != nil {
				return cosmosigner.Backend{}, err
			}
			if !exists {
				return cosmosigner.Backend{}, fmt.Errorf("cosmosigner software key secret %q not found: provide the consensus key registered on-chain — refusing to roll out a signer with no key", secretName)
			}
		}
		return cosmosigner.Backend{Software: &cosmosigner.SoftwareBackend{SecretName: secretName}}, nil

	case c.UsesVaultBackend():
		v := c.Backend.Vault
		return cosmosigner.Backend{Vault: &cosmosigner.VaultBackend{
			Address:           v.Address,
			KeyName:           v.KeyName,
			Mount:             v.GetVaultMount(),
			Namespace:         ptr.Deref(v.Namespace, ""),
			TokenSecret:       v.TokenSecret,
			CertificateSecret: v.CertificateSecret,
			AutoRenewToken:    v.AutoRenewToken,
		}}, nil

	case c.UsesGcpKmsBackend():
		g := c.Backend.GcpKMS
		return cosmosigner.Backend{GCP: &cosmosigner.GcpBackend{
			KeyVersion:        g.KeyVersion,
			CredentialsSecret: g.CredentialsSecret,
		}}, nil
	}
	return cosmosigner.Backend{}, fmt.Errorf("cosmosigner has no backend configured")
}

// maybeImportCosmosignerKey imports the targeted validator's generated consensus key into Vault once,
// when uploadGenerated is set. The source is the validator instance's own priv-key secret (created by
// its genesis/create-validator flow), so Vault holds exactly the key registered on-chain. The import
// fingerprint is tracked per signer in its CosmosignerStatus.KeyImported; this mutates nodeSet.Status
// in memory and returns whether it changed (the caller batches the status write). importPending is
// true while the signer must not be rolled out yet.
func (r *Reconciler) maybeImportCosmosignerKey(ctx context.Context, nodeSet *appsv1.ChainNodeSet, s appsv1.ResolvedSigner, params cosmosigner.Params) (importPending, changed bool, err error) {
	c := s.Spec
	// uploadGenerated is auto-defaulted for genesis-init targets (their consensus key is always
	// generated locally, so it must be imported), matching the documented tmKMS-parity behavior.
	if !c.VaultUploadsGenerated(signerTargetInitializesGenesis(nodeSet, s)) {
		return false, false, nil
	}

	sourceSecret := s.SoftwareKeySecret
	if sourceSecret == "" {
		// The webhook rejects uploadGenerated without a validator target; guard defensively.
		return false, false, fmt.Errorf("cosmosigner uploadGenerated requires a targeted validator whose key can be imported")
	}

	st := nodeSet.EnsureCosmosignerStatus(s.Name)

	// A signer that already rolled out and served (digest recorded) WITHOUT ever importing can only be
	// a pre-provisioned signer whose uploadGenerated was flipped on afterwards. Vault already holds the
	// key that is serving on-chain; quiescing the live signer to import bootstrap material that may be
	// absent or different would leave the validator not signing. Treat the late flip as a no-op. (A
	// signer that legitimately imports records KeyImported BEFORE its first rollout, so this state is
	// unambiguous.)
	if st.SigningDigest != "" && st.KeyImported == "" {
		return false, false, nil
	}

	// The key is produced by the validator's genesis/create-validator flow; wait for it rather than
	// generating (and thereby diverging from) a different key. The import is still pending until it
	// exists, so the caller must not roll out the signer yet.
	keyMaterial, err := r.secretKey(ctx, nodeSet.GetNamespace(), sourceSecret, privKeyFilename)
	if err != nil {
		return false, false, err
	}
	if len(keyMaterial) == 0 {
		// No source material available. A completed import for the CURRENT Vault target and source (the
		// recorded target half matches) stays valid: Vault holds the registered key and the bootstrap
		// Secret is only needed at import time, so a Secret deleted after that import must NOT re-mark
		// the import pending (which would scale the signer to zero). A record from a DIFFERENT
		// target/source proves nothing about this spec — the import is genuinely pending.
		if appsv1.ImportAnnotationMatchesTarget(st.KeyImported, c.Backend.Vault.ImportTargetFingerprint(sourceSecret)) {
			return false, false, nil
		}
		return true, false, nil
	}

	// The record fingerprints the Vault target, the resolved source secret name, AND the key material,
	// so changing the target (key name/mount/address/namespace), the source secret, or its bytes (an
	// in-place update during bootstrap) re-imports instead of leaving the signer pointed at a stale
	// transit key.
	want := c.Backend.Vault.ImportFingerprint(sourceSecret, keyMaterial)
	if st.KeyImported == want {
		return false, false, nil
	}

	// BYTE-only change after the signer already rolled out and served (digest recorded) with an import
	// completed for this same target/source: the source Secret is stale bootstrap material by then —
	// Vault holds the key that was verified at import time and is signing on-chain. Re-importing the
	// edited bytes would scale the live signer to zero and, at best, fail the import (pubkey mismatch)
	// leaving the validator not signing — so the edit is ignored instead. During bootstrap (no digest
	// yet) a byte change still re-imports: the registered key may legitimately have been regenerated.
	if st.SigningDigest != "" &&
		appsv1.ImportAnnotationMatchesTarget(st.KeyImported, c.Backend.Vault.ImportTargetFingerprint(sourceSecret)) {
		return false, false, nil
	}

	// Quiesce any already-running signer BEFORE the (synchronous) re-import: on a source/target change
	// it would otherwise keep signing with the previously imported key while the new one is being
	// imported. Scale-down is asynchronous — until every signer pod is actually gone the import stays
	// pending (retried on a later reconcile), which also prevents the caller from re-applying the
	// StatefulSet at full replicas and cancelling the scale-down.
	quiesced, err := cosmosigner.ScaleDown(ctx, r.Client, nodeSet, nodeSet.GetNamespace(), params.Name)
	if err != nil {
		return false, false, err
	}
	if !quiesced {
		return true, false, nil
	}

	runner := cosmosigner.JobRunner{
		Client: r.ClientSet,
		Scheme: r.Scheme,
		Owner:  nodeSet,
		Params: params,
	}
	if err := runner.ImportKey(ctx, sourceSecret); err != nil {
		r.recorder.Event(nodeSet, corev1.EventTypeWarning, appsv1.ReasonUploadFailure,
			controllers.FormatErrorEvent("failed to import cosmosigner key to Vault", err))
		return false, false, err
	}

	// Record the import fingerprint in the signer's status entry. A transient failure of the batched
	// status write just re-runs the (idempotent) import; the import command itself verifies the stored
	// pubkey matches the source key.
	st.KeyImported = want
	return false, true, nil
}

// signerNameForNode returns the name of the managed signer that targets a specific node — a group's
// instance — and whether one does. A sentry signer (no per-instance identity) fronts every pod of its
// target group; a validator signer targets one instance, so a multi-instance validator group's
// instances each map to their own per-instance signer. The returned name is stamped as the node's
// LabelCosmosignerTarget so the signer's discovery Service selects exactly that node's pod(s).
func signerNameForNode(nodeSet *appsv1.ChainNodeSet, group string, index int) (string, bool) {
	for _, s := range nodeSet.ResolveCosmosigners() {
		targetsGroup := false
		for _, t := range s.TargetGroups {
			if t == group {
				targetsGroup = true
				break
			}
		}
		if !targetsGroup {
			continue
		}
		// The per-instance filter applies ONLY to the signer's validator group (a per-instance signer
		// serves exactly one validator instance). Every pod of any OTHER target group — sentry fan-out,
		// including extra groups fronted alongside a single-instance validator by the top-level signer —
		// is a signing endpoint regardless of its index.
		if s.ValidatorInstance != nil && group == s.ValidatorGroup && *s.ValidatorInstance != index {
			continue
		}
		return s.Name, true
	}
	return "", false
}

// signerTargetInitializesGenesis reports whether the validator a signer targets initializes a new
// genesis (its consensus key is generated locally and must be imported).
func signerTargetInitializesGenesis(nodeSet *appsv1.ChainNodeSet, s appsv1.ResolvedSigner) bool {
	switch {
	case s.ValidatorGroup == "":
		return false
	case s.ValidatorGroup == appsv1.ReservedValidatorGroupName:
		return nodeSet.Spec.Validator != nil && nodeSet.Spec.Validator.Init != nil
	default:
		for _, g := range nodeSet.Spec.Nodes {
			if g.Name == s.ValidatorGroup && g.Validator != nil && g.Validator.Init != nil {
				return true
			}
		}
	}
	return false
}

// secretHasKey reports whether the named secret exists and contains a non-empty value for key.
func (r *Reconciler) secretHasKey(ctx context.Context, namespace, name, key string) (bool, error) {
	data, err := r.secretKey(ctx, namespace, name, key)
	if err != nil {
		return false, err
	}
	return len(data) > 0, nil
}

// secretKey returns the value stored under key in the named secret, or nil when the secret does not
// exist yet (so callers can treat a missing secret as "not ready" rather than an error).
func (r *Reconciler) secretKey(ctx context.Context, namespace, name, key string) ([]byte, error) {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, secret); err != nil {
		if errors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return secret.Data[key], nil
}

func (r *Reconciler) applyCosmosignerObject(ctx context.Context, nodeSet *appsv1.ChainNodeSet, obj client.Object) error {
	return cosmosigner.ApplyOwned(ctx, r.Client, r.Scheme, nodeSet, obj)
}
