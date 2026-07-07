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

	// Import a locally-generated key into Vault when requested (one-shot, once-only).
	if err := r.maybeImportCosmosignerKey(ctx, nodeSet, params); err != nil {
		return err
	}

	cm, err := params.ConfigMap()
	if err != nil {
		return err
	}
	if err := r.applyCosmosignerObject(ctx, nodeSet, cm); err != nil {
		return err
	}

	if err := r.ensureService(ctx, r.ownedService(nodeSet, params.RaftService())); err != nil {
		return err
	}
	if err := r.ensureService(ctx, r.ownedService(nodeSet, params.DiscoveryService())); err != nil {
		return err
	}

	sts, err := params.StatefulSet()
	if err != nil {
		return err
	}
	return r.applyCosmosignerObject(ctx, nodeSet, sts)
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

// cosmosignerSoftwareSecretName resolves the secret holding the software backend's key: an explicit
// privateKeySecret, otherwise the targeted validator's key secret. The webhook guarantees one of
// these is available.
func (r *Reconciler) cosmosignerSoftwareSecretName(nodeSet *appsv1.ChainNodeSet) (string, bool) {
	if s := nodeSet.Spec.Cosmosigner.Backend.Software.PrivateKeySecret; s != nil && *s != "" {
		return *s, true
	}
	return nodeSet.CosmosignerValidatorTargetSecret()
}

// maybeImportCosmosignerKey imports the targeted validator's generated consensus key into Vault
// once, when uploadGenerated is set. The source is the validator's own priv-key secret (created by
// its genesis/create-validator flow), so Vault holds exactly the key registered on-chain.
func (r *Reconciler) maybeImportCosmosignerKey(ctx context.Context, nodeSet *appsv1.ChainNodeSet, params cosmosigner.Params) error {
	c := nodeSet.Spec.Cosmosigner
	if !c.UsesVaultBackend() || !c.Backend.Vault.UploadGenerated {
		return nil
	}
	if nodeSet.Annotations[controllers.AnnotationCosmosignerKeyImported] == controllers.StringValueTrue {
		return nil
	}

	sourceSecret, ok := nodeSet.CosmosignerValidatorTargetSecret()
	if !ok {
		// The webhook rejects uploadGenerated without a validator target; guard defensively.
		return fmt.Errorf("cosmosigner uploadGenerated requires a targeted validator whose key can be imported")
	}

	// The key is produced by the validator's genesis/create-validator flow; wait for it rather than
	// generating (and thereby diverging from) a different key.
	if exists, err := r.secretHasKey(ctx, nodeSet.GetNamespace(), sourceSecret, privKeyFilename); err != nil {
		return err
	} else if !exists {
		return nil
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
		return err
	}

	// Mark imported. A transient conflict on this update just re-runs the (idempotent) import; the
	// import command itself verifies the stored pubkey matches the source key.
	return r.markCosmosignerKeyImported(ctx, nodeSet)
}

// markCosmosignerKeyImported records the once-only import annotation, tolerating a concurrent update
// by re-fetching and retrying.
func (r *Reconciler) markCosmosignerKeyImported(ctx context.Context, nodeSet *appsv1.ChainNodeSet) error {
	if nodeSet.Annotations == nil {
		nodeSet.Annotations = map[string]string{}
	}
	nodeSet.Annotations[controllers.AnnotationCosmosignerKeyImported] = controllers.StringValueTrue
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
	fresh.Annotations[controllers.AnnotationCosmosignerKeyImported] = controllers.StringValueTrue
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
	return nil
}

// applyCosmosignerObject sets the owner reference and creates or updates a managed object,
// preserving StatefulSet fields that Kubernetes forbids updating.
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
	preserveImmutableStatefulSetFields(obj, existing)
	obj.SetResourceVersion(existing.GetResourceVersion())
	return r.Update(ctx, obj)
}

// preserveImmutableStatefulSetFields copies the fields Kubernetes rejects updates to (selector,
// serviceName, podManagementPolicy, volumeClaimTemplates) from the existing StatefulSet onto the
// desired one, so a changed stateStorageSize/storageClassName does not wedge the reconcile loop.
func preserveImmutableStatefulSetFields(desired, existing client.Object) {
	d, ok := desired.(*appsv1k8s.StatefulSet)
	if !ok {
		return
	}
	e, ok := existing.(*appsv1k8s.StatefulSet)
	if !ok {
		return
	}
	d.Spec.Selector = e.Spec.Selector
	d.Spec.ServiceName = e.Spec.ServiceName
	d.Spec.PodManagementPolicy = e.Spec.PodManagementPolicy
	d.Spec.VolumeClaimTemplates = e.Spec.VolumeClaimTemplates
}

// ownedService sets the owner reference on a service built by the cosmosigner package so it can be
// applied via the shared ensureService helper.
func (r *Reconciler) ownedService(nodeSet *appsv1.ChainNodeSet, svc *corev1.Service) *corev1.Service {
	_ = controllerutil.SetControllerReference(nodeSet, svc, r.Scheme)
	return svc
}

func ptrString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
