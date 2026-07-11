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

// ensureCosmosigner deploys (or tears down) a managed cosmosigner remote signer for a standalone
// ChainNode. It is a no-op until the chain ID is known.
func (r *Reconciler) ensureCosmosigner(ctx context.Context, chainNode *appsv1.ChainNode) error {
	if chainNode.Spec.Cosmosigner == nil {
		return r.undeployCosmosigner(ctx, chainNode)
	}
	if chainNode.Status.ChainID == "" {
		return nil
	}

	params := r.cosmosignerParams(chainNode)

	importPending, err := r.maybeImportCosmosignerKey(ctx, chainNode, params)
	if err != nil {
		return err
	}

	// Render once: the ConfigMap contents and the pod-template ROLLME hash come from the same render.
	configYAML, err := params.ConfigYAML()
	if err != nil {
		return err
	}
	if err := r.applyCosmosignerObject(ctx, chainNode, params.ConfigMap(configYAML)); err != nil {
		return err
	}

	if err := r.applyCosmosignerObject(ctx, chainNode, params.RaftService()); err != nil {
		return err
	}
	if err := r.applyCosmosignerObject(ctx, chainNode, params.DiscoveryService()); err != nil {
		return err
	}

	// Do not roll out the signer until the node's generated key has been imported into Vault; an
	// already-running signer is scaled to zero so it cannot keep signing with the previously
	// imported key while a re-import is pending.
	if importPending {
		_, err := cosmosigner.ScaleDown(ctx, r.Client, chainNode, chainNode.GetNamespace(), params.Name)
		return err
	}

	sts, err := params.StatefulSet(configYAML)
	if err != nil {
		return err
	}
	if err := r.applyCosmosignerObject(ctx, chainNode, sts); err != nil {
		return err
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
	if needReplicas || needDigest {
		rolledOut, err := cosmosigner.IsRolledOut(ctx, r.Client, chainNode.GetNamespace(), params.Name, params.Replicas)
		if err != nil {
			return err
		}
		if rolledOut {
			if needReplicas {
				chainNode.Status.CosmosignerReplicas = ptr.To(params.Replicas)
			}
			if needDigest {
				chainNode.Status.CosmosignerSigningDigest = chainNode.CosmosignerSigningDigest()
			}
			return r.Status().Update(ctx, chainNode)
		}
	}
	return nil
}

func (r *Reconciler) cosmosignerParams(chainNode *appsv1.ChainNode) cosmosigner.Params {
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

	return cosmosigner.Params{
		Name:               name,
		Namespace:          chainNode.GetNamespace(),
		ChainID:            chainNode.Status.ChainID,
		Image:              c.GetImage(),
		Replicas:           c.GetReplicas(),
		LogLevel:           c.GetLogLevel(),
		StateStorageSize:   c.GetStateStorageSize(),
		StorageClassName:   c.StorageClassName,
		Resources:          c.GetResources(),
		RaftTLSSecret:      c.RaftTLSSecret,
		ServiceAccountName: c.GetServiceAccountName(),
		Backend:            r.cosmosignerBackend(chainNode),
		Labels:             labels,
		// The chain-node label disambiguates from a same-named ChainNodeSet's target pods, which
		// carry the same cosmosigner-target value (`<name>-signer`) but never the chain-node label.
		TargetSelector: map[string]string{
			controllers.LabelChainNode:         chainNode.GetName(),
			controllers.LabelCosmosignerTarget: name,
		},
	}
}

// cosmosignerBackend translates the CRD backend into the builder backend. The software backend
// references the node's own priv-key secret (created by its genesis/create-validator flow) or an
// explicit secret; no key is generated here, so the signer always signs with the node's registered
// consensus key.
func (r *Reconciler) cosmosignerBackend(chainNode *appsv1.ChainNode) cosmosigner.Backend {
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
		}}
	case c.UsesGcpKmsBackend():
		g := c.Backend.GcpKMS
		return cosmosigner.Backend{GCP: &cosmosigner.GcpBackend{
			KeyVersion:        g.KeyVersion,
			CredentialsSecret: g.CredentialsSecret,
		}}
	default:
		return cosmosigner.Backend{Software: &cosmosigner.SoftwareBackend{SecretName: r.cosmosignerSoftwareSecretName(chainNode)}}
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

	// Fingerprint the Vault target AND the resolved source secret, so changing either re-imports
	// rather than leaving the annotation set. Shared with the ChainNodeSet controller so both
	// import protocols stay in lockstep.
	want := c.Backend.Vault.ImportFingerprint(sourceSecret)
	if chainNode.Annotations[controllers.AnnotationCosmosignerKeyImported] == want {
		return false, nil
	}
	secret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: chainNode.GetNamespace(), Name: sourceSecret}, secret); err != nil {
		if errors.IsNotFound(err) {
			// The node has not produced its key yet; import is pending, retry on a later reconcile.
			return true, nil
		}
		return false, err
	}
	if len(secret.Data[PrivKeyFilename]) == 0 {
		return true, nil
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

// undeployCosmosigner removes managed signer resources this ChainNode owns.
func (r *Reconciler) undeployCosmosigner(ctx context.Context, chainNode *appsv1.ChainNode) error {
	name := cosmosignerName(chainNode)
	if err := cosmosigner.Undeploy(ctx, r.Client, chainNode, chainNode.GetNamespace(), name); err != nil {
		return err
	}
	if chainNode.Status.CosmosignerReplicas == nil && chainNode.Status.CosmosignerSigningDigest == "" {
		return nil
	}

	// Clear the recorded signer invariants only once the StatefulSet AND its PVCs are actually gone.
	// Undeploy just *requests* deletion (it is asynchronous): clearing while the old raft cluster is
	// still terminating would let a remove-and-immediate-re-add bypass the replica guard and bind the
	// surviving PVCs, inheriting stale raft membership. While teardown is in flight, leave the
	// invariants for a later reconcile to clear.
	tornDown, err := cosmosigner.IsTornDown(ctx, r.Client, chainNode.GetNamespace(), name)
	if err != nil || !tornDown {
		return err
	}
	chainNode.Status.CosmosignerReplicas = nil
	chainNode.Status.CosmosignerSigningDigest = ""
	// Tolerate a conflict: the signer is already gone and this clear is idempotent, so a concurrent
	// update just defers it to the next reconcile rather than spinning the workqueue with no progress.
	if err := r.Status().Update(ctx, chainNode); err != nil && !errors.IsConflict(err) {
		return err
	}
	return nil
}

func (r *Reconciler) applyCosmosignerObject(ctx context.Context, chainNode *appsv1.ChainNode, obj client.Object) error {
	return cosmosigner.ApplyOwned(ctx, r.Client, r.Scheme, chainNode, obj)
}
