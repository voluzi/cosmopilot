package chainnode

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

// cosmosignerName is the base name for a standalone ChainNode's managed signer resources.
func cosmosignerName(chainNode *appsv1.ChainNode) string {
	return fmt.Sprintf("%s-signer", chainNode.GetName())
}

// cosmosignerTargetLabelValue returns the cosmosigner discovery-service selector label value this
// node's pod must carry, and whether it is a signer target at all. A standalone node with its own
// .spec.cosmosigner uses its own signer name; a ChainNodeSet-managed target (RemoteSignerTarget)
// reuses the value the nodeset controller stamped on the child ChainNode's metadata.
func cosmosignerTargetLabelValue(chainNode *appsv1.ChainNode) (string, bool) {
	if chainNode.UsesCosmosigner() {
		return cosmosignerName(chainNode), true
	}
	if chainNode.Spec.RemoteSignerTarget {
		if v := chainNode.Labels[controllers.LabelCosmosignerTarget]; v != "" {
			return v, true
		}
	}
	return "", false
}

// backfillCosmosignerLegacyStatus backfills the cosmosigner status invariants on nodes upgraded
// from versions predating them. It runs at the TOP of Reconcile — before configs/pod are touched —
// and reports whether a status write happened, in which case the caller must stop the current
// reconcile: the no-webhook validation must judge the fresh markers before the pod is switched to a
// potentially unverifiable remote-signer spec (which would drop the validator's local key mount).
//
//   - CosmosignerAtEstablishment: nil only on pre-marker chains (SetEstablishedChainID records it
//     atomically otherwise); backfilled conservatively — only a digest-proven identity is kept.
//   - CosmosignerServingIdentity: a digest recorded by a pre-field version proves a validator signer
//     served; backfilled regardless of the establishment marker so removal stays guarded.
func (r *Reconciler) backfillCosmosignerLegacyStatus(ctx context.Context, chainNode *appsv1.ChainNode) (bool, error) {
	if chainNode.Status.ChainID == "" {
		return false, nil
	}
	changed := false

	if chainNode.Status.CosmosignerSigningDigest != "" && chainNode.Status.CosmosignerServingIdentity == "" {
		if identity := chainNode.CosmosignerValidatorTargetedIdentity(); identity != "" {
			chainNode.Status.CosmosignerServingIdentity = identity
			changed = true
		}
	}

	if chainNode.Status.CosmosignerAtEstablishment == nil {
		identity := ""
		if chainNode.Status.CosmosignerSigningDigest != "" {
			identity = chainNode.CosmosignerValidatorTargetedIdentity()
		}
		chainNode.Status.CosmosignerAtEstablishment = ptr.To(identity)
		changed = true
	}

	if changed {
		return true, r.Status().Update(ctx, chainNode)
	}
	return false, nil
}

// ensureCosmosigner deploys (or tears down) a managed cosmosigner remote signer for a standalone
// ChainNode. It is a no-op until the chain ID is known. It returns wait=true while a removed
// signer's teardown is still in flight: the caller must NOT proceed to pod reconciliation then,
// or the node could be switched back to its local/tmKMS signing path while old signer pods (deletion
// is asynchronous) can still sign the same consensus key.
func (r *Reconciler) ensureCosmosigner(ctx context.Context, chainNode *appsv1.ChainNode) (wait bool, err error) {
	if chainNode.Spec.Cosmosigner == nil {
		tornDown, err := r.undeployCosmosigner(ctx, chainNode)
		if err != nil {
			return false, err
		}
		return !tornDown, nil
	}
	if chainNode.Status.ChainID == "" {
		return false, nil
	}

	// Persist the raft membership/PVC-template locks before creating/updating any signer resource.
	// Once the StatefulSet exists, the recorded membership and claims may already be formed; a crash
	// or failed later status update must not leave the no-webhook path without values to enforce.
	locksChanged := false
	if chainNode.Status.CosmosignerReplicas == nil {
		chainNode.Status.CosmosignerReplicas = ptr.To(chainNode.Spec.Cosmosigner.GetReplicas())
		locksChanged = true
	}
	if chainNode.Status.CosmosignerStateStorageSize == "" {
		chainNode.Status.CosmosignerStateStorageSize = chainNode.Spec.Cosmosigner.GetStateStorageSize()
		chainNode.Status.CosmosignerStateStorageClassName = chainNode.Spec.Cosmosigner.StorageClassName
		locksChanged = true
	}
	if locksChanged {
		if err := r.Status().Update(ctx, chainNode); err != nil {
			return false, err
		}
	}

	params, err := r.cosmosignerParams(ctx, chainNode)
	if err != nil {
		return false, err
	}

	importPending, err := r.maybeImportCosmosignerKey(ctx, chainNode, params)
	if err != nil {
		return false, err
	}

	// Render once: the ConfigMap contents and the pod-template ROLLME hash come from the same render.
	configYAML, err := params.ConfigYAML()
	if err != nil {
		return false, err
	}
	if err := r.applyCosmosignerObject(ctx, chainNode, params.ConfigMap(configYAML)); err != nil {
		return false, err
	}

	if err := r.applyCosmosignerObject(ctx, chainNode, params.RaftService()); err != nil {
		return false, err
	}
	if err := r.applyCosmosignerObject(ctx, chainNode, params.DiscoveryService()); err != nil {
		return false, err
	}

	// Do not roll out the signer until the node's generated key has been imported into Vault; an
	// already-running signer is scaled to zero so it cannot keep signing with the previously
	// imported key while a re-import is pending.
	if importPending {
		_, err := cosmosigner.ScaleDown(ctx, r.Client, chainNode, chainNode.GetNamespace(), params.Name)
		return false, err
	}

	sts, err := params.StatefulSet(configYAML)
	if err != nil {
		return false, err
	}
	if err := r.applyCosmosignerObject(ctx, chainNode, sts); err != nil {
		return false, err
	}

	// Record persisted invariants only after the CURRENT signer generation is fully rolled out (read
	// from the live object — the freshly rendered sts carries no status), so a config that never
	// worked stays correctable and a pending change is never locked in by a previous revision's
	// readiness.
	//
	//   - Replicas is recorded for EVERY signer (validator and sentry alike): the raft membership
	//     baked into the per-pod state cannot be migrated by re-rendering a bootstrap list, so the
	//     no-webhook path rejects a later replica change from this recorded value.
	//   - The signing digest is recorded only for a validator: a non-validator sentry's key lives
	//     out-of-band and must stay add/remove/rotate-able (mirrors the webhook waiver).
	needReplicas := chainNode.Status.CosmosignerReplicas == nil
	needDigest := chainNode.Status.CosmosignerSigningDigest == "" && chainNode.IsValidator()
	// A digest recorded by a version predating the serving-identity field leaves it empty; backfill
	// it under the same rolled-out proof so the removal guard also covers those signers.
	needServing := chainNode.Status.CosmosignerServingIdentity == "" &&
		chainNode.Status.CosmosignerSigningDigest != "" && chainNode.IsValidator()
	// Backfilled independently of Replicas so a signer that recorded its replica count before the
	// storage fields existed still gets its PVC template locked.
	needStorage := chainNode.Status.CosmosignerStateStorageSize == ""
	if needReplicas || needDigest || needServing || needStorage {
		rolledOut, err := cosmosigner.IsRolledOut(ctx, r.Client, chainNode.GetNamespace(), params.Name, params.Replicas)
		if err != nil {
			return false, err
		}
		if rolledOut {
			if needReplicas {
				chainNode.Status.CosmosignerReplicas = ptr.To(params.Replicas)
			}
			if needStorage {
				// Locks the PVC template on the no-webhook path and across a remove-and-re-add while the
				// old PVCs may still exist.
				chainNode.Status.CosmosignerStateStorageSize = chainNode.Spec.Cosmosigner.GetStateStorageSize()
				chainNode.Status.CosmosignerStateStorageClassName = chainNode.Spec.Cosmosigner.StorageClassName
			}
			if needDigest {
				chainNode.Status.CosmosignerSigningDigest = chainNode.CosmosignerSigningDigest()
			}
			if needDigest || needServing {
				// The serving identity lets the no-webhook path judge a later REMOVAL: it is admitted
				// only when the validator's own signing path still resolves this identity.
				chainNode.Status.CosmosignerServingIdentity = chainNode.CosmosignerValidatorTargetedIdentity()
			}
			return false, r.Status().Update(ctx, chainNode)
		}
	}
	return false, nil
}

func (r *Reconciler) cosmosignerParams(ctx context.Context, chainNode *appsv1.ChainNode) (cosmosigner.Params, error) {
	c := chainNode.Spec.Cosmosigner
	name := cosmosignerName(chainNode)

	// Exclude the internal selector labels (group/global Service selectors, P2P peer discovery,
	// cleanup selectors) so signer resources can never be selected as node Services or peers —
	// see controllers.CosmosignerReservedSelectorLabels. The ChainNodeSet label is excluded too:
	// every ChainNodeSet global Service selector requires `nodeset=<name>`, so dropping that one
	// key breaks any global-route selector match regardless of which per-nodeset route-membership
	// labels (whose names are dynamic and unknowable here) were inherited.
	exclude := append(controllers.CosmosignerReservedSelectorLabels(), controllers.LabelChainNodeSet)
	labels := utils.ExcludeMapKeys(WithChainNodeLabels(chainNode, map[string]string{
		controllers.LabelChainNode: chainNode.GetName(),
	}), exclude...)

	backend, err := r.cosmosignerBackend(ctx, chainNode)
	if err != nil {
		return cosmosigner.Params{}, err
	}

	return cosmosigner.Params{
		Name:               name,
		Namespace:          chainNode.GetNamespace(),
		OwnerUID:           chainNode.GetUID(),
		ChainID:            chainNode.Status.ChainID,
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
		// The chain-node label disambiguates from a same-named ChainNodeSet's target pods, which
		// carry the same cosmosigner-target value (`<name>-signer`) but never the chain-node label.
		TargetSelector: map[string]string{
			controllers.LabelChainNode:         chainNode.GetName(),
			controllers.LabelCosmosignerTarget: name,
		},
	}, nil
}

// cosmosignerBackend translates the CRD backend into the builder backend. The software backend
// references the node's own priv-key secret (created by its genesis/create-validator flow) or an
// explicit secret; no key is generated here, so the signer always signs with the node's registered
// consensus key.
func (r *Reconciler) cosmosignerBackend(ctx context.Context, chainNode *appsv1.ChainNode) (cosmosigner.Backend, error) {
	c := chainNode.Spec.Cosmosigner
	switch {
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
	default:
		secretName := r.cosmosignerSoftwareSecretName(chainNode)
		// Preflight the key secret whenever no controller flow will (re)create it, instead of rolling
		// out signer pods stuck on a missing Secret mount:
		//   - sentry mode (non-validator): the key is registered out-of-band and always user-supplied;
		//   - an external-genesis validator: its key is user-supplied too (RequiresPrivKey only
		//     generates keys for init/createValidator validators);
		//   - an init/createValidator validator whose key flow already COMPLETED (Status.PubKey set):
		//     RequiresPrivKey no longer regenerates the secret, so a deleted Secret stays deleted.
		// Only an init/createValidator validator whose key flow is still pending skips the check —
		// ensureSigningKey produces the secret on this same reconcile.
		keyFlowPending := chainNode.Status.PubKey == "" &&
			(chainNode.ShouldInitGenesis() || (chainNode.IsValidator() && chainNode.Spec.Validator.CreateValidator != nil))
		if !keyFlowPending {
			secret := &corev1.Secret{}
			if err := r.Get(ctx, client.ObjectKey{Namespace: chainNode.GetNamespace(), Name: secretName}, secret); err != nil {
				if errors.IsNotFound(err) {
					return cosmosigner.Backend{}, fmt.Errorf("cosmosigner software key secret %q not found: provide the consensus key registered on-chain — refusing to roll out a signer with no key", secretName)
				}
				return cosmosigner.Backend{}, err
			}
			if len(secret.Data[PrivKeyFilename]) == 0 {
				return cosmosigner.Backend{}, fmt.Errorf("cosmosigner software key secret %q has no %s: provide the registered consensus key", secretName, PrivKeyFilename)
			}
		}
		return cosmosigner.Backend{Software: &cosmosigner.SoftwareBackend{SecretName: secretName}}, nil
	}
}

func (r *Reconciler) cosmosignerSoftwareSecretName(chainNode *appsv1.ChainNode) string {
	// A validator always signs with its own registered key (the webhook forbids an explicit
	// software override in that case). Only a non-validator sentry uses the explicit secret.
	if chainNode.IsValidator() {
		return r.cosmosignerNodeKeySecret(chainNode)
	}
	if s := chainNode.Spec.Cosmosigner.Backend.Software.PrivateKeySecret; s != nil && *s != "" {
		return *s
	}
	return r.cosmosignerNodeKeySecret(chainNode)
}

// cosmosignerNodeKeySecret resolves the node's own consensus key secret: the validator's resolved
// private-key secret when the node is a validator, otherwise the default name.
func (r *Reconciler) cosmosignerNodeKeySecret(chainNode *appsv1.ChainNode) string {
	if chainNode.IsValidator() {
		return chainNode.Spec.Validator.GetPrivKeySecretName(chainNode)
	}
	return fmt.Sprintf("%s-priv-key", chainNode.GetName())
}

// cosmosignerImportSourcePending reports whether the source key Secret for a Vault upload may still
// be created by this controller. Explicit privateKeySecret values are user-supplied and are never
// generated; after a validator pubkey is recorded, the init/createValidator key flow is complete and
// a missing Secret is an error rather than a pending condition.
func (r *Reconciler) cosmosignerImportSourcePending(chainNode *appsv1.ChainNode) bool {
	if chainNode.Spec.Validator == nil {
		return false
	}
	if chainNode.Spec.Validator.PrivateKeySecret != nil && *chainNode.Spec.Validator.PrivateKeySecret != "" {
		return false
	}
	if chainNode.Status.PubKey != "" {
		return false
	}
	return chainNode.Spec.Validator.Init != nil || chainNode.Spec.Validator.CreateValidator != nil
}

// maybeImportCosmosignerKey imports the node's generated consensus key into Vault once, when
// uploadGenerated is set.
func (r *Reconciler) maybeImportCosmosignerKey(ctx context.Context, chainNode *appsv1.ChainNode, params cosmosigner.Params) (bool, error) {
	c := chainNode.Spec.Cosmosigner
	// uploadGenerated is auto-defaulted for genesis-init validators (their consensus key is always
	// generated locally, so it must be imported), matching the documented tmKMS-parity behavior.
	if !c.VaultUploadsGenerated(chainNode.ShouldInitGenesis()) {
		return false, nil
	}

	sourceSecret := r.cosmosignerNodeKeySecret(chainNode)

	// A signer that already rolled out and served (digest recorded) WITHOUT ever importing can only be
	// a pre-provisioned signer whose uploadGenerated was flipped on afterwards. Vault already holds the
	// key that is serving on-chain; quiescing the live signer to import bootstrap material that may be
	// absent or different would leave the validator not signing. Treat the late flip as a no-op. (A
	// signer that legitimately imports records the annotation BEFORE its first rollout, so this state
	// is unambiguous.)
	if chainNode.Status.CosmosignerSigningDigest != "" &&
		chainNode.Annotations[controllers.AnnotationCosmosignerKeyImported] == "" {
		return false, nil
	}

	// Fetch the source key material first: the fingerprint hashes the actual bytes (not just the
	// secret name), so an in-place update of the source Secret re-imports rather than being skipped.
	secret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: chainNode.GetNamespace(), Name: sourceSecret}, secret); err != nil && !errors.IsNotFound(err) {
		return false, err
	}
	keyMaterial := secret.Data[PrivKeyFilename]
	if len(keyMaterial) == 0 {
		// No source material available. A completed import for the CURRENT Vault target and source
		// (the annotation's target half matches) stays valid: Vault holds the registered key and the
		// bootstrap Secret is only needed at import time, so a Secret deleted after that import must
		// NOT re-mark the import pending (which would scale the signer to zero). An annotation from a
		// DIFFERENT target/source proves nothing about this spec.
		if appsv1.ImportAnnotationMatchesTarget(chainNode.Annotations[controllers.AnnotationCosmosignerKeyImported], c.Backend.Vault.ImportTargetFingerprint(sourceSecret)) {
			return false, nil
		}
		// Wait only while a controller-owned key-generation flow is genuinely pending. For an explicit
		// external-genesis key (or a completed init/createValidator flow), nothing will create this
		// source later; surfacing an error avoids silently scaling the signer to zero forever.
		if r.cosmosignerImportSourcePending(chainNode) {
			return true, nil
		}
		return false, fmt.Errorf("cosmosigner Vault uploadGenerated source secret %q is missing %s: provide the validator key to import", sourceSecret, PrivKeyFilename)
	}

	// Fingerprint the Vault target, the resolved source secret name, AND the key material, so changing
	// any of them re-imports rather than leaving the annotation set. Shared with the ChainNodeSet
	// controller so both import protocols stay in lockstep.
	want := c.Backend.Vault.ImportFingerprint(sourceSecret, keyMaterial)
	if chainNode.Annotations[controllers.AnnotationCosmosignerKeyImported] == want {
		return false, nil
	}

	// BYTE-only change after the signer already rolled out and served (digest recorded) with an import
	// completed for this same target/source: the source Secret is stale bootstrap material by then —
	// Vault holds the key that was verified at import time and is signing on-chain. Re-importing the
	// edited bytes would scale the live signer to zero and, at best, fail the import (pubkey mismatch)
	// leaving the validator not signing — so the edit is ignored instead. During bootstrap (no digest
	// yet) a byte change still re-imports: the registered key may legitimately have been regenerated.
	if chainNode.Status.CosmosignerSigningDigest != "" &&
		appsv1.ImportAnnotationMatchesTarget(chainNode.Annotations[controllers.AnnotationCosmosignerKeyImported], c.Backend.Vault.ImportTargetFingerprint(sourceSecret)) {
		return false, nil
	}

	// Quiesce any already-running signer BEFORE the synchronous re-import, so it cannot keep
	// signing with the previously imported key while the new one lands. Scale-down is
	// asynchronous — until every signer pod is gone the import stays pending (retried next
	// reconcile), which also keeps the caller from re-applying the StatefulSet at full replicas.
	quiesced, err := cosmosigner.ScaleDown(ctx, r.Client, chainNode, chainNode.GetNamespace(), params.Name)
	if err != nil {
		return false, err
	}
	if !quiesced {
		return true, nil
	}

	runner := cosmosigner.JobRunner{Client: r.ClientSet, Scheme: r.Scheme, Owner: chainNode, Params: params}
	if err := runner.ImportKey(ctx, sourceSecret); err != nil {
		r.recorder.Event(chainNode, corev1.EventTypeWarning, appsv1.ReasonUploadFailure,
			controllers.FormatErrorEvent("failed to import cosmosigner key to Vault", err))
		return false, err
	}

	// Mark imported, tolerating a concurrent update: the import command is idempotent (it verifies
	// the stored pubkey), so a conflict retry must not fail the reconcile after a successful import.
	if err := r.markCosmosignerKeyImported(ctx, chainNode, want); err != nil {
		return false, err
	}
	return false, nil
}

// markCosmosignerKeyImported records the import annotation, re-fetching and retrying on a
// resourceVersion conflict so a successful import is never followed by a failed reconcile.
func (r *Reconciler) markCosmosignerKeyImported(ctx context.Context, chainNode *appsv1.ChainNode, value string) error {
	if chainNode.Annotations == nil {
		chainNode.Annotations = map[string]string{}
	}
	chainNode.Annotations[controllers.AnnotationCosmosignerKeyImported] = value
	if err := r.Update(ctx, chainNode); err == nil {
		return nil
	} else if !errors.IsConflict(err) {
		return err
	}

	fresh := &appsv1.ChainNode{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(chainNode), fresh); err != nil {
		return err
	}
	if fresh.Annotations == nil {
		fresh.Annotations = map[string]string{}
	}
	fresh.Annotations[controllers.AnnotationCosmosignerKeyImported] = value
	return r.Update(ctx, fresh)
}

// undeployCosmosigner removes managed signer resources this ChainNode owns, reporting whether
// teardown is COMPLETE (StatefulSet and PVCs gone). Callers must not switch the node's signing path
// back to local/tmKMS until it is.
func (r *Reconciler) undeployCosmosigner(ctx context.Context, chainNode *appsv1.ChainNode) (bool, error) {
	name := cosmosignerName(chainNode)
	if err := cosmosigner.Undeploy(ctx, r.Client, chainNode, chainNode.GetNamespace(), name); err != nil {
		return false, err
	}

	tornDown, err := cosmosigner.IsTornDown(ctx, r.Client, chainNode, chainNode.GetNamespace(), name)
	if err != nil || !tornDown {
		return false, err
	}
	if chainNode.Status.CosmosignerReplicas == nil && chainNode.Status.CosmosignerSigningDigest == "" {
		return true, nil
	}
	// Clear the recorded signer invariants only once the StatefulSet AND its PVCs are actually gone.
	// Undeploy just *requests* deletion (it is asynchronous): clearing while the old raft cluster is
	// still terminating would let a remove-and-immediate-re-add bypass the replica guard and bind the
	// surviving PVCs, inheriting stale raft membership.
	chainNode.Status.CosmosignerReplicas = nil
	chainNode.Status.CosmosignerStateStorageSize = ""
	chainNode.Status.CosmosignerStateStorageClassName = nil
	chainNode.Status.CosmosignerSigningDigest = ""
	chainNode.Status.CosmosignerServingIdentity = ""
	// Tolerate a conflict: the signer is already gone and this clear is idempotent, so a concurrent
	// update just defers it to the next reconcile rather than spinning the workqueue with no progress.
	if err := r.Status().Update(ctx, chainNode); err != nil && !errors.IsConflict(err) {
		return false, err
	}
	return true, nil
}

func (r *Reconciler) applyCosmosignerObject(ctx context.Context, chainNode *appsv1.ChainNode, obj client.Object) error {
	return cosmosigner.ApplyOwned(ctx, r.Client, r.Scheme, chainNode, obj)
}
