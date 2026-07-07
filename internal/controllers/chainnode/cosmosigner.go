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
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
	"github.com/voluzi/cosmopilot/v2/internal/cosmosigner"
	"github.com/voluzi/cosmopilot/v2/internal/k8s"
	"github.com/voluzi/cosmopilot/v2/pkg/utils"
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

	params := r.cosmosignerParams(chainNode)

	importPending, err := r.maybeImportCosmosignerKey(ctx, chainNode, params)
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

	// Do not roll out the signer until the node's generated key has been imported into Vault.
	if importPending {
		return nil
	}

	sts, err := params.StatefulSet()
	if err != nil {
		return err
	}
	return r.applyCosmosignerObject(ctx, chainNode, sts)
}

func (r *Reconciler) cosmosignerParams(chainNode *appsv1.ChainNode) cosmosigner.Params {
	c := chainNode.Spec.Cosmosigner
	name := cosmosignerName(chainNode)

	labels := WithChainNodeLabels(chainNode, map[string]string{
		controllers.LabelChainNode: chainNode.GetName(),
	})

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
		Backend:          r.cosmosignerBackend(chainNode),
		Labels:           labels,
		TargetSelector: map[string]string{
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
			Namespace:         derefString(v.Namespace),
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
	if !c.UsesVaultBackend() || !c.Backend.Vault.UploadGenerated {
		return false, nil
	}

	// Fingerprint the Vault target so changing it (address, namespace, mount or key) re-imports
	// rather than leaving the annotation set.
	want := utils.Sha256(fmt.Sprintf("%s\x00%s\x00%s\x00%s", c.Backend.Vault.Address, derefString(c.Backend.Vault.Namespace), c.Backend.Vault.GetVaultMount(), c.Backend.Vault.KeyName))
	if chainNode.Annotations[controllers.AnnotationCosmosignerKeyImported] == want {
		return false, nil
	}

	sourceSecret := r.cosmosignerNodeKeySecret(chainNode)
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

	runner := cosmosigner.JobRunner{Client: r.ClientSet, Scheme: r.Scheme, Owner: chainNode, Params: params}
	if err := runner.ImportKey(ctx, sourceSecret); err != nil {
		r.recorder.Event(chainNode, corev1.EventTypeWarning, appsv1.ReasonUploadFailure,
			controllers.FormatErrorEvent("failed to import cosmosigner key to Vault", err))
		return false, err
	}

	if chainNode.Annotations == nil {
		chainNode.Annotations = map[string]string{}
	}
	chainNode.Annotations[controllers.AnnotationCosmosignerKeyImported] = want
	return false, r.Update(ctx, chainNode)
}

// undeployCosmosigner removes managed signer resources this ChainNode owns.
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
		if err := r.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return err
		}
		if !metav1.IsControlledBy(obj, chainNode) {
			continue
		}
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
	k8s.PreserveImmutableStatefulSetFields(obj, existing)
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
