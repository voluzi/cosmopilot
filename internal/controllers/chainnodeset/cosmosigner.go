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

// preflightCosmosigners fails the reconcile when any desired signer cannot be deployed, so children
// are not switched to a remote signer that will never come up. It is READ-ONLY (resolves params and
// reads Secrets; never applies resources). It runs before ensureValidator/ensureNodes stamp
// RemoteSignerTarget on the child ChainNodes, so a bad signer spec leaves the validators on their
// existing local signing path instead of dropping the local key. Genuinely-pending states (a
// validator key-generation flow that has not run yet) are NOT failures.
func (r *Reconciler) preflightCosmosigners(ctx context.Context, nodeSet *appsv1.ChainNodeSet) error {
	for _, s := range nodeSet.ResolveCosmosigners() {
		// Resolving params verifies a software backend's key secret and the backend's referenced auth
		// Secrets (Vault token/certificate, GCP credentials) exist (unless a validator key-generation
		// flow is still pending — handled inside cosmosignerBackend).
		if _, err := r.cosmosignerParams(ctx, nodeSet, s); err != nil {
			return err
		}
		// Run the same deploy-time blockers ApplyOwned would hit when it creates the StatefulSet (name
		// collision, foreign/ambiguous raft-state PVCs), so a signer that can never be created does not
		// cause children to be retargeted to a remote signer that will never come up.
		if err := cosmosigner.PreflightDeployable(ctx, r.Client, nodeSet, nodeSet.GetNamespace(), s.Name); err != nil {
			return err
		}
		// A Vault uploadGenerated signer imports the validator's own key; if that source secret is
		// missing and no controller flow will create it, the signer can never roll out, so fail before
		// children switch. But the source is only bootstrap material: once the import for the CURRENT
		// target/source has completed (matching KeyImported, mirroring maybeImportCosmosignerKey's
		// absent-source fast path), or the signer has served (SigningDigest recorded), Vault already
		// holds the key and the missing Secret is fine.
		if s.Spec.VaultUploadsGenerated(signerTargetInitializesGenesis(nodeSet, s)) {
			st := nodeSet.GetCosmosignerStatus(s.Name)
			if st != nil && st.SigningDigest != "" {
				continue
			}
			if st != nil && appsv1.ImportAnnotationMatchesTarget(st.KeyImported, s.Spec.Backend.Vault.ImportTargetFingerprint(s.SoftwareKeySecret)) {
				continue
			}
			if r.signerImportSourcePending(nodeSet, s) {
				continue
			}
			exists, err := r.secretHasKey(ctx, nodeSet.GetNamespace(), s.SoftwareKeySecret, privKeyFilename)
			if err != nil {
				return err
			}
			if !exists {
				return fmt.Errorf("cosmosigner %q Vault uploadGenerated source secret %q is missing %s: provide the validator key to import", s.Name, s.SoftwareKeySecret, privKeyFilename)
			}
		}
	}
	return nil
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

	// Locks are normally recorded before children are retargeted (initCosmosignerLocks runs in Reconcile
	// ahead of ensureValidator/ensureNodes). Call it again here so ensureCosmosigner is self-contained: on
	// the pass that records them it returns (the status write re-triggers a reconcile that runs
	// validateForReconcile against the fresh locks before any resource is applied); once recorded it is a
	// no-op and we proceed straight to deploy.
	if changed, err := r.initCosmosignerLocks(ctx, nodeSet); err != nil {
		return err
	} else if changed {
		return nil
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

// initCosmosignerLocks persists a status entry and the raft-membership/PVC-template locks for every
// desired signer, plus the at-establishment marker for a genesis-registered software sentry whose entry
// is first created after establishment. It reports whether anything was written. It is invoked from
// Reconcile BEFORE ensureValidator/ensureNodes retarget children, so the entries and locks exist before
// a child is switched to a signer whose StatefulSet is created later in the same reconcile — otherwise a
// retargeted validator would point at a signer that does not yet exist. The caller requeues when it
// returns true so validateForReconcile runs against the fresh locks before any signer resource is
// applied (a legacy/status-lost signer with a live 1-replica cluster must not record the live lock and
// then apply a replicas:3 change in the same pass). Recording an entry also lets reconcileSignerTeardown
// see the signer. No-op once everything is recorded; safe to call repeatedly.
func (r *Reconciler) initCosmosignerLocks(ctx context.Context, nodeSet *appsv1.ChainNodeSet) (bool, error) {
	if nodeSet.Status.ChainID == "" {
		return false, nil
	}
	changed := false
	for _, s := range nodeSet.ResolveCosmosigners() {
		created := nodeSet.GetCosmosignerStatus(s.Name) == nil
		st := nodeSet.EnsureCosmosignerStatus(s.Name)
		if created {
			changed = true
			// Backfill the genesis-sentry establishment marker for an entry first created AFTER
			// establishment: SetEstablishedChainID runs only once, so a genesis-registered software sentry
			// whose signer was added later would otherwise keep a nil marker and escape the no-webhook
			// key-change/removal guards. The genesis set is immutable, so this is the identity establishment
			// would have recorded. A validator-targeted signer keeps its nil marker — that nil is how the
			// no-webhook ADD guard detects a post-establishment addition — so only genesis sentries backfill.
			if st.AtEstablishment == nil {
				if id := nodeSet.GenesisSentryEstablishmentIdentity(s); id != "" {
					st.AtEstablishment = &id
				}
			}
		}
		if st.Replicas == nil || st.StateStorageSize == "" {
			if err := r.initSignerLock(ctx, nodeSet, s, st); err != nil {
				return false, err
			}
			changed = true
		}
	}
	if changed {
		return true, r.Status().Update(ctx, nodeSet)
	}
	return false, nil
}

// initSignerLock initialises a signer's Replicas/StateStorageSize/ClassName from the live signer
// state owned by this controller, falling back to the spec when no signer state exists (a true first
// rollout). Anchoring on the live state prevents an in-flight roll-out (failed first reconcile, or an
// already-deployed signer whose status was lost) from being "re-locked" to a different replica count
// or PVC template than the one the raft cluster was actually formed with. It mutates st in memory;
// the caller persists it.
func (r *Reconciler) initSignerLock(ctx context.Context, nodeSet *appsv1.ChainNodeSet, s appsv1.ResolvedSigner, st *appsv1.CosmosignerStatus) error {
	liveReplicas, liveSize, liveClass, foundReplicas, foundStorage, err := cosmosigner.ReadSignerLock(ctx, r.Client, nodeSet, nodeSet.GetNamespace(), s.Name)
	if err != nil {
		return err
	}
	if st.Replicas == nil {
		if foundReplicas {
			st.Replicas = ptr.To(liveReplicas)
		} else {
			st.Replicas = ptr.To(s.Spec.GetReplicas())
		}
	}
	if st.StateStorageSize == "" {
		if foundStorage {
			st.StateStorageSize = liveSize
			st.StateStorageClassName = liveClass
		} else {
			st.StateStorageSize = s.Spec.GetStateStorageSize()
			st.StateStorageClassName = s.Spec.StorageClassName
		}
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

	// The replica/PVC-template locks are recorded before any resource is applied (see
	// ensureCosmosigner). Here we record the signing digest + serving identity/group/instance, which
	// are only meaningful for a validator-targeted signer (a sentry key lives out-of-band and stays
	// add/remove/rotate-able) and only AFTER the signer's current generation is fully rolled out
	// (observed + updated + ready). A new validator signer has no digest yet; a legacy signer upgraded
	// from a status shape predating the serving fields has a non-empty digest but empty serving fields
	// — that combination blocks the no-webhook removal guard (treated as unverifiable) and must be
	// backfilled on the next rolled-out reconcile.
	needServing := (s.TargetsValidator() && st.SigningDigest == "") ||
		(st.SigningDigest != "" && st.ServingIdentity == "" && s.TargetsValidator())
	if needServing {
		rolledOut, err := cosmosigner.IsRolledOut(ctx, r.Client, nodeSet.GetNamespace(), params.Name, params.Replicas)
		if err != nil {
			return false, err
		}
		if rolledOut {
			st.SigningDigest = s.Digest()
			st.ServingIdentity = s.ValidatorTargetedIdentity()
			st.ServingGroup = s.ValidatorGroup
			st.ServingInstance = s.ValidatorInstance
			changed = true
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
			instance := 0
			if s.ValidatorInstance != nil {
				instance = *s.ValidatorInstance
			}
			// Whether THIS instance generates its own key must come from its PER-INSTANCE config, not the
			// group's: in a multi-instance genesis-init group only instance 0 keeps Init, so the other
			// instances' keys are pre-created before genesis and never regenerated once the chain is
			// established — treating them as "pending" would let the signer deploy with a missing key.
			generates := false
			if s.ValidatorGroup == appsv1.ReservedValidatorGroupName {
				v := nodeSet.Spec.Validator
				generates = v != nil && (v.Init != nil || v.CreateValidator != nil)
			} else {
				for _, g := range nodeSet.Spec.Nodes {
					if g.Name == s.ValidatorGroup && g.Validator != nil {
						cfg := deriveGroupValidatorConfig(nodeSet, g.Name, instance, g.GetInstances(), g.Validator)
						generates = cfg.Init != nil || cfg.CreateValidator != nil
					}
				}
			}
			if generates {
				keyFlowPending = true
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
		// The Vault token authenticates every signing call and the optional CA certificate is mounted at
		// startup; a missing Secret would roll out a signer that can never reach Vault. Verify them before
		// deploy (and, via preflightCosmosigners, before children are retargeted).
		if err := r.requireSecretSelector(ctx, nodeSet.GetNamespace(), s.Name, "Vault token", v.TokenSecret); err != nil {
			return cosmosigner.Backend{}, err
		}
		if err := r.requireSecretSelector(ctx, nodeSet.GetNamespace(), s.Name, "Vault certificate", v.CertificateSecret); err != nil {
			return cosmosigner.Backend{}, err
		}
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
		// The GCP credentials Secret (when set — omitted for Workload Identity) is mounted at startup.
		if err := r.requireSecretSelector(ctx, nodeSet.GetNamespace(), s.Name, "GCP credentials", g.CredentialsSecret); err != nil {
			return cosmosigner.Backend{}, err
		}
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

	// The import is TERMINAL once the signer has rolled out and served (SigningDigest recorded). For a
	// uploadGenerated signer, a recorded digest implies a completed import (rollout is blocked while
	// the import is pending), and the served validator's on-chain key is immutable thereafter, so no
	// later edit to the source Secret — its bytes, or (since it is the validator's own privateKeySecret)
	// its name — can legitimately require a re-import. Re-importing would only quiesce the live signer to
	// import possibly-absent/different bootstrap material and stop signing. It also covers a late
	// uploadGenerated flip on a served pre-provisioned signer (digest set, never imported): Vault already
	// holds the serving key. During bootstrap (no digest yet) a byte change still re-imports below.
	if st.SigningDigest != "" {
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
		// target/source proves nothing about this spec.
		if appsv1.ImportAnnotationMatchesTarget(st.KeyImported, c.Backend.Vault.ImportTargetFingerprint(sourceSecret)) {
			return false, false, nil
		}
		// Wait only while a controller-owned key-generation flow is genuinely pending. For an explicit
		// external-genesis key (or a generated key whose pubkey is already recorded), nothing will create
		// this source later; surfacing an error avoids silently scaling the signer to zero forever.
		if r.signerImportSourcePending(nodeSet, s) {
			return true, false, nil
		}
		return false, false, fmt.Errorf("cosmosigner %q Vault uploadGenerated source secret %q is missing %s: provide the validator key to import", s.Name, sourceSecret, privKeyFilename)
	}

	// The record fingerprints the Vault target, the resolved source secret name, AND the key material,
	// so changing the target (key name/mount/address/namespace), the source secret, or its bytes (an
	// in-place update during bootstrap) re-imports instead of leaving the signer pointed at a stale
	// transit key.
	want := c.Backend.Vault.ImportFingerprint(sourceSecret, keyMaterial)
	if st.KeyImported == want {
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

	// Persist the import fingerprint IMMEDIATELY — before the caller rolls out the StatefulSet — so a
	// later status-write conflict cannot leave a live signer without its KeyImported record (the next
	// reconcile would then ScaleDown and re-import, taking a correct live signer offline even though
	// Vault already holds the key). On failure we abort before the StatefulSet is applied (no live
	// signer to disrupt); the next reconcile re-imports (ImportKey is idempotent — it verifies the
	// stored pubkey) and re-persists. r.Status().Update refreshes nodeSet's resourceVersion on success,
	// so the caller's later batched status write stays consistent. Mirrors the standalone path.
	st.KeyImported = want
	if err := r.Status().Update(ctx, nodeSet); err != nil {
		return false, false, err
	}
	return false, false, nil
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

// signerImportSourcePending reports whether the source key Secret for a Vault upload may still be
// created by this controller. Explicit privateKeySecret values are user-supplied and are never
// generated; after the target validator pubkey is recorded, the init/createValidator key flow is
// complete and a missing Secret is an error rather than a pending condition. It uses the PER-INSTANCE
// validator config: in a multi-instance genesis-initializing group only instance 0 keeps Init, so the
// non-init instances (whose keys are pre-created before genesis and never regenerated) are correctly
// treated as terminal, not perpetually pending.
func (r *Reconciler) signerImportSourcePending(nodeSet *appsv1.ChainNodeSet, s appsv1.ResolvedSigner) bool {
	if s.ValidatorGroup == "" {
		return false
	}
	instance := 0
	if s.ValidatorInstance != nil {
		instance = *s.ValidatorInstance
	}

	cfg := nodeSet.Spec.Validator
	if s.ValidatorGroup != appsv1.ReservedValidatorGroupName {
		cfg = nil
		for i := range nodeSet.Spec.Nodes {
			g := &nodeSet.Spec.Nodes[i]
			if g.Name == s.ValidatorGroup && g.Validator != nil {
				cfg = deriveGroupValidatorConfig(nodeSet, g.Name, instance, g.GetInstances(), g.Validator)
				break
			}
		}
	}
	if cfg == nil {
		return false
	}
	if cfg.PrivateKeySecret != nil && *cfg.PrivateKeySecret != "" {
		return false
	}
	generates := cfg.Init != nil || cfg.CreateValidator != nil
	if !generates {
		return false
	}
	name := validatorNodeName(nodeSet, s.ValidatorGroup, instance)
	for _, v := range nodeSet.Status.Validators {
		if v.Name == name && v.PubKey != "" {
			return false
		}
	}
	return true
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

// requireSecretSelector errors when a referenced Secret key is absent, so a signer that mounts a
// missing auth Secret is caught at preflight instead of crash-looping after deploy. A nil selector (an
// optional reference left unset) is accepted.
func (r *Reconciler) requireSecretSelector(ctx context.Context, namespace, signer, purpose string, sel *corev1.SecretKeySelector) error {
	if sel == nil {
		return nil
	}
	exists, err := r.secretHasKey(ctx, namespace, sel.Name, sel.Key)
	if err != nil {
		return err
	}
	if !exists {
		return fmt.Errorf("cosmosigner %q %s secret %q is missing key %q: provide it before deploying the signer", signer, purpose, sel.Name, sel.Key)
	}
	return nil
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
