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

// ensureCosmosigner deploys (or tears down) the managed cosmosigner remote signer for a
// ChainNodeSet. It is a no-op until the chain ID is known, since the signer config requires it.
func (r *Reconciler) ensureCosmosigner(ctx context.Context, nodeSet *appsv1.ChainNodeSet) error {
	if nodeSet.Spec.Cosmosigner == nil {
		return r.undeployCosmosigner(ctx, nodeSet)
	}

	// The signer config needs the chain ID; wait for genesis to be available.
	if nodeSet.Status.ChainID == "" {
		return nil
	}

	params, err := r.cosmosignerParams(nodeSet)
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
	// registers the local pubkey. A later reconcile completes the import and deploys the signer.
	if importPending {
		return nil
	}

	sts, err := params.StatefulSet(configYAML)
	if err != nil {
		return err
	}
	if err := r.applyCosmosignerObject(ctx, nodeSet, sts); err != nil {
		return err
	}

	// Record the signing identity only after the CURRENT signer generation is fully rolled out
	// (observed + all replicas updated and ready, read from the live object — the freshly rendered
	// sts carries no status). A config that never worked therefore stays correctable, and readiness
	// left over from a previous revision cannot lock in a pending change. The digest is only
	// meaningful when the signer serves a validator: a sentry-mode signer's key lives out-of-band
	// and must remain add/remove/rotate-able, mirroring the webhook's empty-identity waiver.
	if nodeSet.Status.CosmosignerSigningDigest == "" {
		if _, hasValidatorTarget := nodeSet.CosmosignerValidatorTargetSecret(); hasValidatorTarget {
			rolledOut, err := cosmosigner.IsRolledOut(ctx, r.Client, nodeSet.GetNamespace(), params.Name, params.Replicas)
			if err != nil {
				return err
			}
			if rolledOut {
				nodeSet.Status.CosmosignerSigningDigest = nodeSet.CosmosignerSigningDigest()
				return r.Status().Update(ctx, nodeSet)
			}
		}
	}
	return nil
}

// cosmosignerParams resolves the builder parameters, ensuring the software key secret exists when
// the software backend is used.
func (r *Reconciler) cosmosignerParams(nodeSet *appsv1.ChainNodeSet) (cosmosigner.Params, error) {
	c := nodeSet.Spec.Cosmosigner
	name := cosmosignerName(nodeSet)

	// Signer pods must never inherit selector labels that would make them endpoints of node
	// Services: the group/validator selector labels (group Services select chain-node-set + group)
	// and the generated global ingress/gateway membership labels (global Services select
	// chain-node-set + <route name>). A user label on the ChainNodeSet matching any of these would
	// otherwise route chain RPC/LCD/GRPC traffic to signer pods.
	exclude := []string{controllers.LabelChainNodeSetGroup, controllers.LabelChainNodeSetValidator}
	for _, ingress := range nodeSet.Spec.Ingresses {
		exclude = append(exclude, ingress.GetName(nodeSet))
	}
	for _, gw := range nodeSet.Spec.GatewayRoutes {
		exclude = append(exclude, gw.GetName(nodeSet))
	}
	labels := utils.ExcludeMapKeys(WithChainNodeSetLabels(nodeSet, map[string]string{
		controllers.LabelChainNodeSet: nodeSet.GetName(),
	}), exclude...)

	backend, err := r.cosmosignerBackend(nodeSet)
	if err != nil {
		return cosmosigner.Params{}, err
	}

	return cosmosigner.Params{
		Name:               name,
		Namespace:          nodeSet.GetNamespace(),
		ChainID:            nodeSet.Status.ChainID,
		Image:              c.GetImage(),
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
func (r *Reconciler) cosmosignerBackend(nodeSet *appsv1.ChainNodeSet) (cosmosigner.Backend, error) {
	c := nodeSet.Spec.Cosmosigner
	switch {
	case c.UsesSoftwareBackend():
		secretName, ok := r.cosmosignerSoftwareSecretName(nodeSet)
		if !ok {
			return cosmosigner.Backend{}, fmt.Errorf("cosmosigner software backend has no resolvable private-key secret")
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
	if !c.UsesVaultBackend() || !c.Backend.Vault.UploadGenerated {
		return false, nil
	}

	sourceSecret, ok := nodeSet.CosmosignerValidatorTargetSecret()
	if !ok {
		// The webhook rejects uploadGenerated without a validator target; guard defensively.
		return false, fmt.Errorf("cosmosigner uploadGenerated requires a targeted validator whose key can be imported")
	}

	// The annotation records a fingerprint of the Vault target AND the resolved source secret, so
	// changing either the target (key name/mount/address/namespace) or the source key re-imports
	// instead of leaving the signer pointed at a stale transit key.
	want := c.Backend.Vault.ImportFingerprint(sourceSecret)
	if nodeSet.Annotations[controllers.AnnotationCosmosignerKeyImported] == want {
		return false, nil
	}

	// The key is produced by the validator's genesis/create-validator flow; wait for it rather than
	// generating (and thereby diverging from) a different key. The import is still pending until it
	// exists, so the caller must not roll out the signer yet.
	if exists, err := r.secretHasKey(ctx, nodeSet.GetNamespace(), sourceSecret, privKeyFilename); err != nil {
		return false, err
	} else if !exists {
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
	secret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, secret); err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return len(secret.Data[key]) > 0, nil
}

// undeployCosmosigner removes managed signer resources when cosmosigner is no longer configured,
// deleting only resources this ChainNodeSet actually owns.
func (r *Reconciler) undeployCosmosigner(ctx context.Context, nodeSet *appsv1.ChainNodeSet) error {
	return cosmosigner.Undeploy(ctx, r.Client, nodeSet, nodeSet.GetNamespace(), cosmosignerName(nodeSet))
}

func (r *Reconciler) applyCosmosignerObject(ctx context.Context, nodeSet *appsv1.ChainNodeSet, obj client.Object) error {
	return cosmosigner.ApplyOwned(ctx, r.Client, r.Scheme, nodeSet, obj)
}
