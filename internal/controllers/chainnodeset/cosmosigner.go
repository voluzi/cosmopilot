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

// cosmosignerName is the base name for a ChainNodeSet's managed signer resources.
func cosmosignerName(nodeSet *appsv1.ChainNodeSet) string {
	return fmt.Sprintf("%s-signer", nodeSet.GetName())
}

// backfillCosmosignerLegacyStatus backfills the cosmosigner status invariants on chains upgraded
// from versions predating them. It runs at the TOP of Reconcile — before any child ChainNode or
// signer resource is touched — and reports whether a status write happened, in which case the caller
// must stop the current reconcile: the no-webhook validation (which runs before reconcile) must
// judge the fresh markers before children are reconciled from a potentially unverifiable spec.
//
//   - CosmosignerAtEstablishment: normally recorded atomically with the chain ID (see
//     SetEstablishedChainID), so nil only occurs on pre-marker chains. Backfilled conservatively: an
//     identity proven by a recorded rollout digest (it served) is kept; anything unproven records ""
//     and so stays subject to the addition guard.
//   - CosmosignerServingIdentity/Group: a digest recorded by a pre-field version proves a
//     validator-targeted signer served; backfill both so removal is guarded (and a pre-group-field
//     serving identity is never permanently blocked by an unknown group). This runs regardless of
//     the establishment marker.
func (r *Reconciler) backfillCosmosignerLegacyStatus(ctx context.Context, nodeSet *appsv1.ChainNodeSet) (bool, error) {
	if nodeSet.Status.ChainID == "" {
		return false, nil
	}
	changed := false

	if nodeSet.Status.CosmosignerSigningDigest != "" &&
		(nodeSet.Status.CosmosignerServingIdentity == "" || nodeSet.Status.CosmosignerServingGroup == "") {
		if identity := nodeSet.CosmosignerValidatorTargetedIdentity(); identity != "" {
			nodeSet.Status.CosmosignerServingIdentity = identity
			nodeSet.Status.CosmosignerServingGroup = nodeSet.CosmosignerTargetedValidatorGroup()
			changed = true
		}
	}

	if nodeSet.Status.CosmosignerAtEstablishment == nil {
		identity := ""
		if nodeSet.Status.CosmosignerSigningDigest != "" {
			identity = nodeSet.CosmosignerValidatorTargetedIdentity()
		}
		nodeSet.Status.CosmosignerAtEstablishment = ptr.To(identity)
		changed = true
	}

	if changed {
		return true, r.Status().Update(ctx, nodeSet)
	}
	return false, nil
}

// ensureCosmosigner deploys (or tears down) the managed cosmosigner remote signer for a
// ChainNodeSet. It is a no-op until the chain ID is known, since the signer config requires it.
func (r *Reconciler) ensureCosmosigner(ctx context.Context, nodeSet *appsv1.ChainNodeSet) error {
	if nodeSet.Spec.Cosmosigner == nil {
		// Teardown is driven EARLY in Reconcile (before children are reconciled) so the child signing
		// path is never switched while old signer pods can still sign; nothing to do here.
		return nil
	}

	// The signer config needs the chain ID; wait for genesis to be available.
	if nodeSet.Status.ChainID == "" {
		return nil
	}

	params, err := r.cosmosignerParams(ctx, nodeSet)
	if err != nil {
		return err
	}

	// Import a locally-generated key into Vault when requested (one-shot, once-only). Returns
	// pending when the validator has not produced its key yet.
	importPending, err := r.maybeImportCosmosignerKey(ctx, nodeSet, params)
	if err != nil {
		return err
	}

	// Render once: the ConfigMap contents and the pod-template ROLLME hash come from the same render.
	configYAML, err := params.ConfigYAML()
	if err != nil {
		return err
	}
	if err := r.applyCosmosignerObject(ctx, nodeSet, params.ConfigMap(configYAML)); err != nil {
		return err
	}

	if err := r.applyCosmosignerObject(ctx, nodeSet, params.RaftService()); err != nil {
		return err
	}
	if err := r.applyCosmosignerObject(ctx, nodeSet, params.DiscoveryService()); err != nil {
		return err
	}

	// Do not roll out the signer until the validator's generated key has been imported into Vault,
	// otherwise the signer would come up against an empty/stale transit key while the validator
	// registers the local pubkey. An ALREADY-RUNNING signer is scaled to zero for the same reason:
	// after a source/target change it would keep signing with the previously imported key while the
	// re-import is pending. A later reconcile completes the import and (re)deploys the signer.
	if importPending {
		_, err := cosmosigner.ScaleDown(ctx, r.Client, nodeSet, nodeSet.GetNamespace(), params.Name)
		return err
	}

	sts, err := params.StatefulSet(configYAML)
	if err != nil {
		return err
	}
	if err := r.applyCosmosignerObject(ctx, nodeSet, sts); err != nil {
		return err
	}

	// Record persisted invariants only after the CURRENT signer generation is fully rolled out
	// (observed + all replicas updated and ready, read from the live object — the freshly rendered
	// sts carries no status). A config that never worked therefore stays correctable, and readiness
	// left over from a previous revision cannot lock in a pending change.
	//
	//   - Replicas is recorded for EVERY signer (validator-targeted and sentry alike): the raft
	//     membership baked into the per-pod state cannot be migrated by re-rendering a bootstrap list,
	//     so the no-webhook path rejects a later replica change from this recorded value.
	//   - The signing digest is only meaningful when the signer serves a validator: a sentry-mode
	//     signer's key lives out-of-band and must stay add/remove/rotate-able, mirroring the webhook's
	//     empty-identity waiver — so it is recorded only for a validator target.
	_, hasValidatorTarget := nodeSet.CosmosignerValidatorTargetSecret()
	needReplicas := nodeSet.Status.CosmosignerReplicas == nil
	needDigest := nodeSet.Status.CosmosignerSigningDigest == "" && hasValidatorTarget
	// A digest recorded by a version predating the serving-identity/group fields leaves them empty;
	// backfill under the same rolled-out proof so the removal guard covers those signers and is never
	// permanently blocked by an unknown group.
	needServing := (nodeSet.Status.CosmosignerServingIdentity == "" || nodeSet.Status.CosmosignerServingGroup == "") &&
		nodeSet.Status.CosmosignerSigningDigest != "" && hasValidatorTarget
	if needReplicas || needDigest || needServing {
		rolledOut, err := cosmosigner.IsRolledOut(ctx, r.Client, nodeSet.GetNamespace(), params.Name, params.Replicas)
		if err != nil {
			return err
		}
		if rolledOut {
			if needReplicas {
				nodeSet.Status.CosmosignerReplicas = ptr.To(params.Replicas)
			}
			if needDigest {
				nodeSet.Status.CosmosignerSigningDigest = nodeSet.CosmosignerSigningDigest()
			}
			if needDigest || needServing {
				// The serving identity + group let the no-webhook path judge a later REMOVAL: it is
				// admitted only when the SERVED validator's own signing path still resolves this identity.
				nodeSet.Status.CosmosignerServingIdentity = nodeSet.CosmosignerValidatorTargetedIdentity()
				nodeSet.Status.CosmosignerServingGroup = nodeSet.CosmosignerTargetedValidatorGroup()
			}
			return r.Status().Update(ctx, nodeSet)
		}
	}
	return nil
}

// cosmosignerParams resolves the builder parameters, ensuring the software key secret exists when
// the software backend is used.
func (r *Reconciler) cosmosignerParams(ctx context.Context, nodeSet *appsv1.ChainNodeSet) (cosmosigner.Params, error) {
	c := nodeSet.Spec.Cosmosigner
	name := cosmosignerName(nodeSet)

	// Signer pods/resources must never inherit internal selector labels (group/global Service
	// selectors, P2P peer discovery, resource-cleanup selectors) — see
	// controllers.CosmosignerReservedSelectorLabels. The generated global ingress/gateway
	// membership label names are per-nodeset and appended below.
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

	backend, err := r.cosmosignerBackend(ctx, nodeSet)
	if err != nil {
		return cosmosigner.Params{}, err
	}

	return cosmosigner.Params{
		Name:               name,
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
			controllers.LabelCosmosignerTarget: name,
		},
	}, nil
}

// cosmosignerBackend translates the CRD backend into the builder backend. Key material is never
// generated here: the software backend references either an explicit secret or the targeted
// validator's own key secret (produced by the validator's genesis/create-validator flow), so the
// signer always signs with the exact consensus key registered on-chain.
func (r *Reconciler) cosmosignerBackend(ctx context.Context, nodeSet *appsv1.ChainNodeSet) (cosmosigner.Backend, error) {
	c := nodeSet.Spec.Cosmosigner
	switch {
	case c.UsesSoftwareBackend():
		secretName, ok := r.cosmosignerSoftwareSecretName(nodeSet)
		if !ok {
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
		// Only a target whose key-generation flow is still pending skips the check — the child
		// ChainNode produces the secret during that flow.
		keyFlowPending := false
		if group := nodeSet.CosmosignerTargetedValidatorGroup(); group != "" {
			generates := false
			if group == appsv1.ReservedValidatorGroupName {
				v := nodeSet.Spec.Validator
				generates = v != nil && (v.Init != nil || v.CreateValidator != nil)
			} else {
				for _, g := range nodeSet.Spec.Nodes {
					if g.Name == group && g.Validator != nil {
						generates = g.Validator.Init != nil || g.Validator.CreateValidator != nil
					}
				}
			}
			if generates {
				keyFlowPending = true
				for _, v := range nodeSet.Status.Validators {
					if v.Group == group && v.PubKey != "" {
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

// cosmosignerSoftwareSecretName resolves the secret holding the software backend's key. When a
// validator is targeted the signer must use that validator's own registered key, so it takes
// precedence over any explicit privateKeySecret (which the webhook only permits in sentry mode).
func (r *Reconciler) cosmosignerSoftwareSecretName(nodeSet *appsv1.ChainNodeSet) (string, bool) {
	if secret, ok := nodeSet.CosmosignerValidatorTargetSecret(); ok {
		return secret, true
	}
	if s := nodeSet.Spec.Cosmosigner.Backend.Software.PrivateKeySecret; s != nil && *s != "" {
		return *s, true
	}
	return "", false
}

// maybeImportCosmosignerKey imports the targeted validator's generated consensus key into Vault
// once, when uploadGenerated is set. The source is the validator's own priv-key secret (created by
// its genesis/create-validator flow), so Vault holds exactly the key registered on-chain.
func (r *Reconciler) maybeImportCosmosignerKey(ctx context.Context, nodeSet *appsv1.ChainNodeSet, params cosmosigner.Params) (bool, error) {
	c := nodeSet.Spec.Cosmosigner
	// uploadGenerated is auto-defaulted for genesis-init targets (their consensus key is always
	// generated locally, so it must be imported), matching the documented tmKMS-parity behavior.
	if !c.VaultUploadsGenerated(nodeSet.CosmosignerTargetInitializesGenesis()) {
		return false, nil
	}

	sourceSecret, ok := nodeSet.CosmosignerValidatorTargetSecret()
	if !ok {
		// The webhook rejects uploadGenerated without a validator target; guard defensively.
		return false, fmt.Errorf("cosmosigner uploadGenerated requires a targeted validator whose key can be imported")
	}

	// The key is produced by the validator's genesis/create-validator flow; wait for it rather than
	// generating (and thereby diverging from) a different key. The import is still pending until it
	// exists, so the caller must not roll out the signer yet.
	keyMaterial, err := r.secretKey(ctx, nodeSet.GetNamespace(), sourceSecret, privKeyFilename)
	if err != nil {
		return false, err
	}
	if len(keyMaterial) == 0 {
		// No source material available. A completed import for the CURRENT Vault target and source
		// (the annotation's target half matches) stays valid: Vault holds the registered key and the
		// bootstrap Secret is only needed at import time, so a Secret deleted after that import must
		// NOT re-mark the import pending (which would scale the signer to zero). An annotation from a
		// DIFFERENT target/source proves nothing about this spec — the import is genuinely pending
		// (nothing usable was ever imported for it), keeping the signer down until the source appears.
		if appsv1.ImportAnnotationMatchesTarget(nodeSet.Annotations[controllers.AnnotationCosmosignerKeyImported], c.Backend.Vault.ImportTargetFingerprint(sourceSecret)) {
			return false, nil
		}
		return true, nil
	}

	// The annotation records a fingerprint of the Vault target, the resolved source secret name, AND
	// the key material, so changing the target (key name/mount/address/namespace), the source secret,
	// or its bytes (an in-place update) re-imports instead of leaving the signer pointed at a stale
	// transit key.
	want := c.Backend.Vault.ImportFingerprint(sourceSecret, keyMaterial)
	if nodeSet.Annotations[controllers.AnnotationCosmosignerKeyImported] == want {
		return false, nil
	}

	// Quiesce any already-running signer BEFORE the (synchronous) re-import: on a source/target
	// change it would otherwise keep signing with the previously imported key while the new one is
	// being imported. Scale-down is asynchronous — until every signer pod is actually gone the
	// import stays pending (retried on a later reconcile), which also prevents the caller from
	// re-applying the StatefulSet at full replicas and cancelling the scale-down.
	quiesced, err := cosmosigner.ScaleDown(ctx, r.Client, nodeSet, nodeSet.GetNamespace(), params.Name)
	if err != nil {
		return false, err
	}
	if !quiesced {
		return true, nil
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
		return false, err
	}

	// Mark imported with the target fingerprint. A transient conflict just re-runs the (idempotent)
	// import; the import command itself verifies the stored pubkey matches the source key.
	if err := r.markCosmosignerKeyImported(ctx, nodeSet, want); err != nil {
		return false, err
	}
	return false, nil
}

// markCosmosignerKeyImported records the import annotation, tolerating a concurrent update by
// re-fetching and retrying.
func (r *Reconciler) markCosmosignerKeyImported(ctx context.Context, nodeSet *appsv1.ChainNodeSet, value string) error {
	if nodeSet.Annotations == nil {
		nodeSet.Annotations = map[string]string{}
	}
	nodeSet.Annotations[controllers.AnnotationCosmosignerKeyImported] = value
	if err := r.Update(ctx, nodeSet); err == nil {
		return nil
	} else if !errors.IsConflict(err) {
		return err
	}

	fresh := &appsv1.ChainNodeSet{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(nodeSet), fresh); err != nil {
		return err
	}
	if fresh.Annotations == nil {
		fresh.Annotations = map[string]string{}
	}
	fresh.Annotations[controllers.AnnotationCosmosignerKeyImported] = value
	return r.Update(ctx, fresh)
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

// undeployCosmosigner removes managed signer resources when cosmosigner is no longer configured,
// deleting only resources this ChainNodeSet actually owns. It reports whether teardown is COMPLETE
// (StatefulSet and PVCs gone): callers must not reconcile children onto their local/tmKMS signing
// path until it is, or the old signer pods (deletion is asynchronous) could briefly sign the same
// consensus key as the restored local signer.
func (r *Reconciler) undeployCosmosigner(ctx context.Context, nodeSet *appsv1.ChainNodeSet) (bool, error) {
	name := cosmosignerName(nodeSet)
	if err := cosmosigner.Undeploy(ctx, r.Client, nodeSet, nodeSet.GetNamespace(), name); err != nil {
		return false, err
	}

	// Teardown completion gates both the invariant clear and the caller's child reconciliation.
	tornDown, err := cosmosigner.IsTornDown(ctx, r.Client, nodeSet, nodeSet.GetNamespace(), name)
	if err != nil || !tornDown {
		return false, err
	}
	if nodeSet.Status.CosmosignerReplicas == nil && nodeSet.Status.CosmosignerSigningDigest == "" {
		return true, nil
	}
	// Clear the recorded signer invariants only once the StatefulSet AND its PVCs are actually gone.
	// Undeploy just *requests* deletion (it is asynchronous): clearing while the old raft cluster is
	// still terminating would let a remove-and-immediate-re-add bypass the replica guard and bind the
	// surviving PVCs, inheriting stale raft membership.
	nodeSet.Status.CosmosignerReplicas = nil
	nodeSet.Status.CosmosignerSigningDigest = ""
	nodeSet.Status.CosmosignerServingIdentity = ""
	nodeSet.Status.CosmosignerServingGroup = ""
	// Tolerate a conflict: the signer is already gone and this clear is idempotent, so a concurrent
	// update just defers it to the next reconcile rather than spinning the workqueue with no progress.
	if err := r.Status().Update(ctx, nodeSet); err != nil && !errors.IsConflict(err) {
		return false, err
	}
	return true, nil
}

func (r *Reconciler) applyCosmosignerObject(ctx context.Context, nodeSet *appsv1.ChainNodeSet, obj client.Object) error {
	return cosmosigner.ApplyOwned(ctx, r.Client, r.Scheme, nodeSet, obj)
}
