package chainnode

import (
	"context"
	"fmt"

	appsv1k8s "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/cometbft"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
	"github.com/voluzi/cosmopilot/v2/internal/cosmosigner"
)

// cosmosignerName is the base name for a standalone ChainNode's managed signer resources.
func cosmosignerName(chainNode *appsv1.ChainNode) string {
	return fmt.Sprintf("%s-signer", chainNode.GetName())
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

	params, err := r.cosmosignerParams(ctx, chainNode)
	if err != nil {
		return err
	}

	cm, err := params.ConfigMap()
	if err != nil {
		return err
	}
	if err := r.applyCosmosignerObject(ctx, chainNode, cm); err != nil {
		return err
	}

	if err := r.ensureService(ctx, r.ownedCosmosignerService(chainNode, params.RaftService())); err != nil {
		return err
	}
	if err := r.ensureService(ctx, r.ownedCosmosignerService(chainNode, params.DiscoveryService())); err != nil {
		return err
	}

	sts, err := params.StatefulSet()
	if err != nil {
		return err
	}
	return r.applyCosmosignerObject(ctx, chainNode, sts)
}

func (r *Reconciler) cosmosignerParams(ctx context.Context, chainNode *appsv1.ChainNode) (cosmosigner.Params, error) {
	c := chainNode.Spec.Cosmosigner
	name := cosmosignerName(chainNode)

	labels := WithChainNodeLabels(chainNode, map[string]string{
		controllers.LabelChainNode: chainNode.GetName(),
	})

	backend, err := r.cosmosignerBackend(ctx, chainNode)
	if err != nil {
		return cosmosigner.Params{}, err
	}

	return cosmosigner.Params{
		Name:             name,
		Namespace:        chainNode.GetNamespace(),
		ChainID:          chainNode.Status.ChainID,
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
			controllers.LabelCosmosignerTarget: name,
		},
	}, nil
}

func (r *Reconciler) cosmosignerBackend(ctx context.Context, chainNode *appsv1.ChainNode) (cosmosigner.Backend, error) {
	c := chainNode.Spec.Cosmosigner
	switch {
	case c.UsesSoftwareBackend():
		secretName := r.cosmosignerSoftwareSecretName(chainNode)
		if err := r.ensureCosmosignerSoftwareKey(ctx, chainNode, secretName); err != nil {
			return cosmosigner.Backend{}, err
		}
		return cosmosigner.Backend{Software: &cosmosigner.SoftwareBackend{SecretName: secretName}}, nil

	case c.UsesVaultBackend():
		v := c.Backend.Vault
		return cosmosigner.Backend{Vault: &cosmosigner.VaultBackend{
			Address:           v.Address,
			KeyName:           v.KeyName,
			Mount:             v.GetVaultMount(),
			Namespace:         derefString(v.Namespace),
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

func (r *Reconciler) cosmosignerSoftwareSecretName(chainNode *appsv1.ChainNode) string {
	if s := chainNode.Spec.Cosmosigner.Backend.Software.PrivateKeySecret; s != nil {
		return *s
	}
	// Default to the validator private-key secret name so a drop-in remote signer reuses the
	// node's existing key when present.
	return fmt.Sprintf("%s-priv-key", chainNode.GetName())
}

func (r *Reconciler) ensureCosmosignerSoftwareKey(ctx context.Context, chainNode *appsv1.ChainNode, name string) error {
	secret := &corev1.Secret{}
	err := r.Get(ctx, client.ObjectKey{Namespace: chainNode.GetNamespace(), Name: name}, secret)
	if err == nil {
		if _, ok := secret.Data[PrivKeyFilename]; ok {
			return nil
		}
	} else if !errors.IsNotFound(err) {
		return err
	}

	key, err := cometbft.GeneratePrivKey()
	if err != nil {
		return err
	}
	spec := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: chainNode.GetNamespace()},
		Data:       map[string][]byte{PrivKeyFilename: key},
	}
	if err := controllerutil.SetControllerReference(chainNode, spec, r.Scheme); err != nil {
		return err
	}
	if err := r.Create(ctx, spec); err != nil && !errors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

func (r *Reconciler) undeployCosmosigner(ctx context.Context, chainNode *appsv1.ChainNode) error {
	name := cosmosignerName(chainNode)
	ns := chainNode.GetNamespace()
	objects := []client.Object{
		&appsv1k8s.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name + "-privval", Namespace: ns}},
	}
	for _, obj := range objects {
		if err := r.Delete(ctx, obj); err != nil && !errors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (r *Reconciler) applyCosmosignerObject(ctx context.Context, chainNode *appsv1.ChainNode, obj client.Object) error {
	if err := controllerutil.SetControllerReference(chainNode, obj, r.Scheme); err != nil {
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

func (r *Reconciler) ownedCosmosignerService(chainNode *appsv1.ChainNode, svc *corev1.Service) *corev1.Service {
	_ = controllerutil.SetControllerReference(chainNode, svc, r.Scheme)
	return svc
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
