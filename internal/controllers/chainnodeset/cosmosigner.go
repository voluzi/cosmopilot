package chainnodeset

import (
	"context"
	"fmt"

	appsv1k8s "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
	"github.com/voluzi/cosmopilot/v2/internal/cosmosigner"
	"github.com/voluzi/cosmopilot/v2/internal/k8s"
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

	cm, err := params.ConfigMap()
	if err != nil {
		return err
	}
	if err := r.applyCosmosignerObject(ctx, nodeSet, cm); err != nil {
		return err
	}

	if err := r.ensureOwnedCosmosignerService(ctx, nodeSet, params.RaftService()); err != nil {
		return err
	}
	if err := r.ensureOwnedCosmosignerService(ctx, nodeSet, params.DiscoveryService()); err != nil {
		return err
	}

	// Do not roll out the signer until the validator's generated key has been imported into Vault,
	// otherwise the signer would come up against an empty/stale transit key while the validator
	// registers the local pubkey. A later reconcile completes the import and deploys the signer.
	if importPending {
		return nil
	}

	sts, err := params.StatefulSet()
	if err != nil {
		return err
	}
	if err := r.applyCosmosignerObject(ctx, nodeSet, sts); err != nil {
		return err
	}

	// Record the signing identity only after the signer has been successfully rolled out, so a
	// broken initial config stays correctable (the no-webhook immutability guard then only protects
	// an identity that is actually in effect).
	if nodeSet.Status.CosmosignerSigningDigest == "" {
		nodeSet.Status.CosmosignerSigningDigest = nodeSet.CosmosignerSigningDigest()
		return r.Status().Update(ctx, nodeSet)
	}
	return nil
}

// cosmosignerParams resolves the builder parameters, ensuring the software key secret exists when
// the software backend is used.
func (r *Reconciler) cosmosignerParams(nodeSet *appsv1.ChainNodeSet) (cosmosigner.Params, error) {
	c := nodeSet.Spec.Cosmosigner
	name := cosmosignerName(nodeSet)

	labels := WithChainNodeSetLabels(nodeSet, map[string]string{
		controllers.LabelChainNodeSet: nodeSet.GetName(),
	})

	backend, err := r.cosmosignerBackend(nodeSet)
	if err != nil {
		return cosmosigner.Params{}, err
	}

	return cosmosigner.Params{
		Name:             name,
		Namespace:        nodeSet.GetNamespace(),
		ChainID:          nodeSet.Status.ChainID,
		Image:            c.GetImage(),
		Replicas:         c.GetReplicas(),
		LogLevel:         c.GetLogLevel(),
		StateStorageSize: c.GetStateStorageSize(),
		StorageClassName: c.StorageClassName,
		Resources:        c.GetResources(),
		RaftTLSSecret:    c.RaftTLSSecret,
		Backend:          backend,
		Labels:           labels,
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
			Namespace:         ptrString(v.Namespace),
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

	// The annotation records a fingerprint of the Vault target so that changing the key name, mount
	// or address re-imports into the new target instead of leaving the signer pointed at an empty
	// or stale transit key.
	want := cosmosignerVaultTargetFingerprint(c.Backend.Vault)
	if nodeSet.Annotations[controllers.AnnotationCosmosignerKeyImported] == want {
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

// cosmosignerVaultTargetFingerprint returns a stable fingerprint of the Vault target a generated key
// is imported into, so a change to the target (address, namespace, mount or key) triggers a fresh
// import.
func cosmosignerVaultTargetFingerprint(v *appsv1.CosmosignerVaultBackend) string {
	return utils.Sha256(fmt.Sprintf("%s\x00%s\x00%s\x00%s", v.Address, ptrString(v.Namespace), v.GetVaultMount(), v.KeyName))
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

// undeployCosmosigner removes managed signer resources when cosmosigner is no longer configured. It
// only deletes resources this ChainNodeSet actually owns, so an unrelated resource that happens to
// share the derived name is never touched.
func (r *Reconciler) undeployCosmosigner(ctx context.Context, nodeSet *appsv1.ChainNodeSet) error {
	logger := log.FromContext(ctx)
	name := cosmosignerName(nodeSet)
	ns := nodeSet.GetNamespace()

	// If a signer StatefulSet with this derived name exists but is owned by a different CR (a
	// same-name ChainNode/ChainNodeSet collision), the whole signer — including its PVCs — belongs to
	// that other owner. Do not touch anything.
	sts := &appsv1k8s.StatefulSet{}
	err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, sts)
	switch {
	case err == nil && !metav1.IsControlledBy(sts, nodeSet):
		return nil
	case err != nil && !errors.IsNotFound(err):
		return err
	}

	objects := []client.Object{
		&appsv1k8s.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name + "-privval", Namespace: ns}},
	}
	for _, obj := range objects {
		if err := r.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return err
		}
		if !metav1.IsControlledBy(obj, nodeSet) {
			continue
		}
		if err := r.Delete(ctx, obj); err != nil && !errors.IsNotFound(err) {
			logger.Error(err, "failed to delete cosmosigner resource", "name", obj.GetName())
			return err
		}
	}

	// StatefulSet PVCs are not garbage-collected with the StatefulSet. Delete the per-pod raft-state
	// PVCs so a later re-enable starts from a clean, consistent raft membership. The foreign-owner
	// short-circuit above ensures these PVCs are ours.
	return cosmosigner.DeletePVCs(ctx, r.Client, ns, name)
}

// applyCosmosignerObject sets the owner reference and creates or updates a managed object,
// preserving StatefulSet fields that Kubernetes forbids updating. It refuses to overwrite an
// existing object owned by a different controller (a same-name CR collision).
func (r *Reconciler) applyCosmosignerObject(ctx context.Context, nodeSet *appsv1.ChainNodeSet, obj client.Object) error {
	if err := controllerutil.SetControllerReference(nodeSet, obj, r.Scheme); err != nil {
		return err
	}
	existing, ok := obj.DeepCopyObject().(client.Object)
	if !ok {
		return fmt.Errorf("object is not a client.Object")
	}
	err := r.Get(ctx, client.ObjectKeyFromObject(obj), existing)
	if errors.IsNotFound(err) {
		return r.Create(ctx, obj)
	}
	if err != nil {
		return err
	}
	if !metav1.IsControlledBy(existing, nodeSet) {
		return fmt.Errorf("cosmosigner resource %q is managed by another owner; refusing to overwrite it — rename the ChainNode/ChainNodeSet to avoid the name collision", obj.GetName())
	}
	k8s.PreserveImmutableStatefulSetFields(obj, existing)
	obj.SetResourceVersion(existing.GetResourceVersion())
	return r.Update(ctx, obj)
}

// ownedService sets the owner reference on a cosmosigner service and refuses to apply it if a
// same-named service is owned by another controller.
func (r *Reconciler) ensureOwnedCosmosignerService(ctx context.Context, nodeSet *appsv1.ChainNodeSet, svc *corev1.Service) error {
	if err := controllerutil.SetControllerReference(nodeSet, svc, r.Scheme); err != nil {
		return err
	}
	existing := &corev1.Service{}
	err := r.Get(ctx, client.ObjectKeyFromObject(svc), existing)
	if err == nil && !metav1.IsControlledBy(existing, nodeSet) {
		return fmt.Errorf("cosmosigner service %q is managed by another owner; refusing to overwrite it — rename the ChainNode/ChainNodeSet to avoid the name collision", svc.GetName())
	}
	if err != nil && !errors.IsNotFound(err) {
		return err
	}
	return r.ensureService(ctx, svc)
}

func ptrString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
