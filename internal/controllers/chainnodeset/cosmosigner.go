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
	"github.com/voluzi/cosmopilot/v2/internal/cometbft"
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

	params, err := r.cosmosignerParams(ctx, nodeSet)
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
func (r *Reconciler) cosmosignerParams(ctx context.Context, nodeSet *appsv1.ChainNodeSet) (cosmosigner.Params, error) {
	c := nodeSet.Spec.Cosmosigner
	name := cosmosignerName(nodeSet)

	labels := WithChainNodeSetLabels(nodeSet, map[string]string{
		controllers.LabelChainNodeSet: nodeSet.GetName(),
	})

	backend, err := r.cosmosignerBackend(ctx, nodeSet)
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

// cosmosignerBackend translates the CRD backend into the builder backend, ensuring the software
// key secret exists when needed.
func (r *Reconciler) cosmosignerBackend(ctx context.Context, nodeSet *appsv1.ChainNodeSet) (cosmosigner.Backend, error) {
	c := nodeSet.Spec.Cosmosigner
	switch {
	case c.UsesSoftwareBackend():
		secretName := r.cosmosignerSoftwareSecretName(nodeSet)
		if err := r.ensureCosmosignerSoftwareKey(ctx, nodeSet, secretName); err != nil {
			return cosmosigner.Backend{}, err
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

// cosmosignerSoftwareSecretName resolves the secret holding the software backend's key.
func (r *Reconciler) cosmosignerSoftwareSecretName(nodeSet *appsv1.ChainNodeSet) string {
	if s := nodeSet.Spec.Cosmosigner.Backend.Software.PrivateKeySecret; s != nil {
		return *s
	}
	return fmt.Sprintf("%s-cosmosigner-priv-key", nodeSet.GetName())
}

// ensureCosmosignerSoftwareKey creates the software backend key secret with a freshly generated
// consensus key when it does not yet exist. An existing secret is left untouched so the key stays
// stable.
func (r *Reconciler) ensureCosmosignerSoftwareKey(ctx context.Context, nodeSet *appsv1.ChainNodeSet, name string) error {
	return r.ensureSecret(ctx, nodeSet, name, []string{privKeyFilename}, func() (map[string][]byte, error) {
		key, err := cometbft.GeneratePrivKey()
		if err != nil {
			return nil, err
		}
		return map[string][]byte{privKeyFilename: key}, nil
	})
}

// maybeImportCosmosignerKey imports the locally-generated key into Vault once, when uploadGenerated
// is set. The source secret is the software key secret (or the target validator's key) — for v1
// this uses the dedicated cosmosigner software key secret name.
func (r *Reconciler) maybeImportCosmosignerKey(ctx context.Context, nodeSet *appsv1.ChainNodeSet, params cosmosigner.Params) error {
	c := nodeSet.Spec.Cosmosigner
	if !c.UsesVaultBackend() || !c.Backend.Vault.UploadGenerated {
		return nil
	}
	if nodeSet.Annotations[controllers.AnnotationCosmosignerKeyImported] == controllers.StringValueTrue {
		return nil
	}

	// Generate a local key to import.
	sourceSecret := fmt.Sprintf("%s-cosmosigner-priv-key", nodeSet.GetName())
	if err := r.ensureCosmosignerSoftwareKey(ctx, nodeSet, sourceSecret); err != nil {
		return err
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

	if nodeSet.Annotations == nil {
		nodeSet.Annotations = map[string]string{}
	}
	nodeSet.Annotations[controllers.AnnotationCosmosignerKeyImported] = controllers.StringValueTrue
	return r.Update(ctx, nodeSet)
}

// undeployCosmosigner removes managed signer resources when cosmosigner is no longer configured.
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
		if err := r.Delete(ctx, obj); err != nil && !errors.IsNotFound(err) {
			logger.Error(err, "failed to delete cosmosigner resource", "name", obj.GetName())
			return err
		}
	}
	return nil
}

// applyCosmosignerObject sets the owner reference and creates or updates a managed object.
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
	obj.SetResourceVersion(existing.GetResourceVersion())
	return r.Update(ctx, obj)
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
