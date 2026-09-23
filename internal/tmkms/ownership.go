package tmkms

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/voluzi/cosmopilot/v3/internal/k8s"
)

// Resource classes for the durable objects this package generates. They are attribution only: no
// deletion policy finalizes them, so the identity Secret and the signing state PVC keep outliving the
// ChainNode exactly as they did before they were stamped.
const (
	ClassIdentity = "tmkmsIdentity"
	ClassState    = "tmkmsState"
)

// Attribution records and checks which root a durable object belongs to. It is supplied by the
// controller, which owns the attribution scheme.
type Attribution interface {
	// Stamp attributes object to the root for class, reporting whether anything changed.
	Stamp(object metav1.Object, class string) bool
	// Attributed reports whether object carries any attribution, and whether that attribution names
	// this root (by identity, so a recreated root with a new UID still matches) and class.
	Attributed(object metav1.Object, class string) (stamped, owned bool)
}

// claims reports whether an existing identity Secret or state PVC belongs to this KMS. Deterministic
// names are not proof of ownership. Cosmopilot never stamped these objects before, so an unstamped one
// with no controller is adopted only when the owner's own pod mounts it or it has the exact shape
// Cosmopilot creates.
func (kms *KMS) claims(ctx context.Context, object metav1.Object, class string, legacyShape bool) (bool, error) {
	if kms.Config.Attribution == nil {
		return false, fmt.Errorf("tmKMS %q has no resource attribution configured", kms.Name)
	}
	if stamped, owned := kms.Config.Attribution.Attributed(object, class); stamped {
		return owned, nil
	}
	if metav1.GetControllerOf(object) != nil {
		return false, nil
	}
	if legacyShape {
		return true, nil
	}
	return kms.mountedByOwnerPod(ctx, class)
}

// stamp records that the object belongs to this KMS's root, reporting whether anything changed.
func (kms *KMS) stamp(object metav1.Object, class string) bool {
	if kms.Config.Attribution == nil {
		return false
	}
	return kms.Config.Attribution.Stamp(object, class)
}

func (kms *KMS) mountedByOwnerPod(ctx context.Context, class string) (bool, error) {
	pod, err := kms.Client.CoreV1().Pods(kms.Owner.GetNamespace()).Get(ctx, kms.Owner.GetName(), metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	if !metav1.IsControlledBy(pod, kms.Owner) {
		return false, nil
	}
	for _, volume := range pod.Spec.Volumes {
		switch {
		case class == ClassIdentity && volume.Secret != nil && volume.Secret.SecretName == kms.Name:
			return true, nil
		case class == ClassState && volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == kms.Name:
			return true, nil
		}
	}
	return false, nil
}

// hasIdentitySecretShape matches the identity Secret Cosmopilot has always created.
func hasIdentitySecretShape(secret *corev1.Secret) bool {
	_, hasKey := secret.Data[identityKeyName]
	return secret.Immutable != nil && *secret.Immutable && hasKey && len(secret.Data) == 1 &&
		len(secret.StringData) == 0 && (secret.Type == "" || secret.Type == corev1.SecretTypeOpaque)
}

// hasStatePVCShape matches the signing state PVC Cosmopilot has always created.
func hasStatePVCShape(pvc *corev1.PersistentVolumeClaim) bool {
	want := resource.MustParse(tmkmsPvcSize)
	got, ok := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	return ok && got.Cmp(want) == 0 &&
		len(pvc.Spec.AccessModes) == 1 && pvc.Spec.AccessModes[0] == corev1.ReadWriteOnce &&
		pvc.Spec.DataSource == nil && pvc.Spec.DataSourceRef == nil
}

func notOwnedError(kind, name string) error {
	return fmt.Errorf("tmKMS %s %q exists but is not managed by this ChainNode; refusing to use it "+
		"(remove the conflicting object or rename the ChainNode)", kind, name)
}

// replaceHelperPod removes a leftover helper pod from a previous attempt before a new one is created.
// A same-name pod that this owner does not control is left alone and reported.
func (kms *KMS) replaceHelperPod(ctx context.Context, name string) error {
	existing, err := kms.Client.CoreV1().Pods(kms.Owner.GetNamespace()).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if !metav1.IsControlledBy(existing, kms.Owner) {
		return fmt.Errorf("tmKMS helper pod %q is managed by another owner; refusing to replace it "+
			"(remove the conflicting pod or rename the ChainNode)", name)
	}
	if err := k8s.NewPodHelper(kms.Client, nil, existing).DeleteWithUIDPrecondition(ctx); err != nil &&
		!apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
		return err
	}
	return nil
}
