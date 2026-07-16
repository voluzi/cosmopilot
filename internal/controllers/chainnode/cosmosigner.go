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
	params, err := r.preflightCosmosigner(ctx, chainNode)
	if err != nil {
		return false, err
	}
	return r.ensureCosmosignerWithParams(ctx, chainNode, params)
}

func (r *Reconciler) ensureCosmosignerWithParams(ctx context.Context, chainNode *appsv1.ChainNode, params cosmosigner.Params) (wait bool, err error) {
	// INITIALISE the raft membership/PVC-template locks BEFORE creating any signer resource. The
	// values come from the live signer state (if any), so an existing unrecorded StatefulSet is not
	// "re-locked" to a different replica count or PVC template than the one the raft cluster was
	// actually formed with; they fall back to the spec only when no signer state exists yet (a true
	// first rollout). When anything is recorded here, RETURN (wait=true): the status write re-triggers
	// a reconcile, which runs Validate against the freshly recorded locks BEFORE any resource is
	// applied — otherwise a legacy/status-lost signer could record the live lock and then apply a
	// lock-violating spec change in the same pass. Deferring is crash-safe: no lock, no resource.
	if chainNode.Status.CosmosignerReplicas == nil || chainNode.Status.CosmosignerStateStorageSize == "" {
		liveReplicas, liveSize, liveClass, foundReplicas, foundStorage, err := cosmosigner.ReadSignerLock(ctx, r.Client, chainNode, chainNode.GetNamespace(), cosmosignerName(chainNode))
		if err != nil {
			return false, err
		}
		if chainNode.Status.CosmosignerReplicas == nil {
			if foundReplicas {
				chainNode.Status.CosmosignerReplicas = ptr.To(liveReplicas)
			} else {
				chainNode.Status.CosmosignerReplicas = ptr.To(chainNode.Spec.Cosmosigner.GetReplicas())
			}
		}
		if chainNode.Status.CosmosignerStateStorageSize == "" {
			if foundStorage {
				chainNode.Status.CosmosignerStateStorageSize = liveSize
				chainNode.Status.CosmosignerStateStorageClassName = liveClass
			} else {
				chainNode.Status.CosmosignerStateStorageSize = chainNode.Spec.Cosmosigner.GetStateStorageSize()
				chainNode.Status.CosmosignerStateStorageClassName = chainNode.Spec.Cosmosigner.StorageClassName
			}
		}
		if err := r.Status().Update(ctx, chainNode); err != nil {
			return false, err
		}
		return true, nil
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

	// The replica/PVC-template locks are recorded before any resource is applied (above). Here we
	// record the signing digest + serving identity — only for a validator (a sentry key lives
	// out-of-band and stays add/remove/rotate-able) and only after the current generation is fully
	// rolled out (read from the live object — the freshly rendered sts carries no status), so a config
	// that never worked stays correctable. A digest recorded by a version predating the serving field
	// leaves it empty; backfill it under the same rolled-out proof so the removal guard covers those.
	needDigest := chainNode.Status.CosmosignerSigningDigest == "" && chainNode.IsValidator()
	needServing := chainNode.Status.CosmosignerServingIdentity == "" &&
		chainNode.Status.CosmosignerSigningDigest != "" && chainNode.IsValidator()
	if needDigest || needServing {
		rolledOut, err := cosmosigner.IsRolledOut(ctx, r.Client, chainNode.GetNamespace(), params.Name, params.Replicas)
		if err != nil {
			return false, err
		}
		if rolledOut {
			if needDigest {
				chainNode.Status.CosmosignerSigningDigest = chainNode.CosmosignerSigningDigest()
			}
			// The serving identity lets the no-webhook path judge a later REMOVAL: it is admitted only
			// when the validator's own signing path still resolves this identity.
			chainNode.Status.CosmosignerServingIdentity = chainNode.CosmosignerValidatorTargetedIdentity()
			return false, r.Status().Update(ctx, chainNode)
		}
	}
	return false, nil
}

func (r *Reconciler) preflightCosmosigner(ctx context.Context, chainNode *appsv1.ChainNode) (cosmosigner.Params, error) {
	// Preflight deployability BEFORE the immutable raft/PVC locks are recorded, so a signer that
	// cannot deploy yet (a missing/incomplete raft-TLS Secret, or a missing backend auth/software Secret
	// resolved inside cosmosignerParams) fails WITHOUT first trapping the operator into the
	// initially-chosen replica count / storage template — which the webhook would then refuse to change
	// even though no signer was ever created. The raft mTLS Secret is mounted at pod startup; the params
	// resolution verifies the backend Secrets. Neither depends on the recorded locks.
	if err := cosmosigner.RequireRaftTLSSecret(ctx, r.Client, chainNode.GetNamespace(), chainNode.Spec.Cosmosigner.RaftTLSSecret); err != nil {
		return cosmosigner.Params{}, err
	}
	params, err := r.cosmosignerParams(ctx, chainNode)
	if err != nil {
		return cosmosigner.Params{}, err
	}
	// The signer StatefulSet and import pod run as the configured ServiceAccount; a missing one keeps
	// Kubernetes from starting them.
	if err := cosmosigner.RequireServiceAccount(ctx, r.Client, chainNode.GetNamespace(), chainNode.Spec.Cosmosigner.GetServiceAccountName()); err != nil {
		return cosmosigner.Params{}, err
	}
	// Run the same deploy-time blockers ApplyOwned/runJob would hit (name collision, foreign/ambiguous
	// raft-state PVCs) BEFORE recording locks or importing into Vault — otherwise a foreign same-name
	// signer would persist locks (and, for uploadGenerated, mutate the Vault key) for a signer ApplyOwned
	// then refuses. Only an uploadGenerated signer runs the one-shot <name>-import pod.
	usesImportPod := chainNode.Spec.Cosmosigner.VaultUploadsGenerated(chainNode.ShouldInitGenesis())
	if err := cosmosigner.PreflightDeployable(ctx, r.Client, chainNode, chainNode.GetNamespace(), cosmosignerName(chainNode), chainNode.Spec.Cosmosigner.GetReplicas(), usesImportPod); err != nil {
		return cosmosigner.Params{}, err
	}
	// Preflight the uploadGenerated import SOURCE (read-only) before locks/import: a terminally missing
	// source key would otherwise be found only inside maybeImportCosmosignerKey, after locks are recorded.
	if err := r.preflightCosmosignerImportSource(ctx, chainNode); err != nil {
		return cosmosigner.Params{}, err
	}
	return params, nil
}

func (r *Reconciler) reconcileSigningConfigs(ctx context.Context, chainNode *appsv1.ChainNode) (bool, error) {
	if chainNode.Spec.Cosmosigner == nil || chainNode.Status.ChainID == "" {
		if err := r.ensureTmKMSConfig(ctx, chainNode); err != nil {
			return false, err
		}
		return r.ensureCosmosigner(ctx, chainNode)
	}
	params, err := r.preflightCosmosigner(ctx, chainNode)
	if err != nil {
		return false, err
	}
	if err := r.ensureTmKMSConfig(ctx, chainNode); err != nil {
		return false, err
	}
	return r.ensureCosmosignerWithParams(ctx, chainNode, params)
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
		// The Vault token authenticates every signing call and the optional CA certificate is mounted at
		// startup; a missing/mistyped Secret would recreate the validator pod without its local key for a
		// signer that can never reach Vault. Verify them before the backend is allowed to deploy.
		if err := cosmosigner.RequireSecretSelector(ctx, r.Client, chainNode.GetNamespace(), "Vault token", v.TokenSecret); err != nil {
			return cosmosigner.Backend{}, err
		}
		if err := cosmosigner.RequireSecretSelector(ctx, r.Client, chainNode.GetNamespace(), "Vault certificate", v.CertificateSecret); err != nil {
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
		if err := cosmosigner.RequireSecretSelector(ctx, r.Client, chainNode.GetNamespace(), "GCP credentials", g.CredentialsSecret); err != nil {
			return cosmosigner.Backend{}, err
		}
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

// preflightCosmosignerImportSource errors when a Vault uploadGenerated signer's source key Secret is
// TERMINALLY missing — read-only, so it can run before the raft/PVC locks are recorded and before any
// import mutates Vault. It mirrors maybeImportCosmosignerKey's terminal-missing path: a completed import
// for the current target (annotation matches) or a genuinely pending key-generation flow is fine; only a
// source that no controller flow will create is an error.
func (r *Reconciler) preflightCosmosignerImportSource(ctx context.Context, chainNode *appsv1.ChainNode) error {
	c := chainNode.Spec.Cosmosigner
	if !c.VaultUploadsGenerated(chainNode.ShouldInitGenesis()) || chainNode.Status.CosmosignerSigningDigest != "" {
		return nil
	}
	sourceSecret := r.cosmosignerNodeKeySecret(chainNode)
	if appsv1.ImportAnnotationMatchesTarget(chainNode.Annotations[controllers.AnnotationCosmosignerKeyImported], c.Backend.Vault.ImportTargetFingerprint(sourceSecret)) {
		return nil
	}
	if r.cosmosignerImportSourcePending(chainNode) {
		return nil
	}
	secret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: chainNode.GetNamespace(), Name: sourceSecret}, secret); err != nil {
		if errors.IsNotFound(err) {
			return fmt.Errorf("cosmosigner Vault uploadGenerated source secret %q is missing %s: provide the validator key to import", sourceSecret, PrivKeyFilename)
		}
		return err
	}
	if len(secret.Data[PrivKeyFilename]) == 0 {
		return fmt.Errorf("cosmosigner Vault uploadGenerated source secret %q is missing %s: provide the validator key to import", sourceSecret, PrivKeyFilename)
	}
	return nil
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

	// The import is TERMINAL once the signer has rolled out and served (SigningDigest recorded). For a
	// uploadGenerated signer, a recorded digest implies a completed import (rollout is blocked while the
	// import is pending), and the served validator's on-chain key is immutable thereafter, so no later
	// edit to the source Secret — its bytes, or (since it is the node's own privateKeySecret) its name —
	// can legitimately require a re-import. Re-importing would only quiesce the live signer to import
	// possibly-absent/different bootstrap material and stop signing. It also covers a late uploadGenerated
	// flip on a served pre-provisioned signer (digest set, never imported): Vault already holds the
	// serving key. During bootstrap (no digest yet) a byte change still re-imports below.
	if chainNode.Status.CosmosignerSigningDigest != "" {
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
