package chainnode

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
	"github.com/voluzi/cosmopilot/v2/internal/cosmosigner"
)

// ensureValidatorConsensusKeyReservation claims the active local or TmKMS consensus key before
// any signing configuration or validator pod is reconciled. ChainNodeSet signer targets are claimed
// by their parent signer preflight and must not create a second child-owned claim.
func (r *Reconciler) ensureValidatorConsensusKeyReservation(ctx context.Context, chainNode *appsv1.ChainNode) (bool, error) {
	if !chainNode.IsValidator() || chainNode.Status.ChainID == "" || chainNode.Spec.Cosmosigner != nil || chainNode.Spec.RemoteSignerTarget {
		return false, nil
	}

	publicKey, verifiedIdentity, err := r.validatorConsensusPublicKey(ctx, chainNode)
	if err != nil {
		return false, err
	}
	if recorded := chainNode.Status.PubKey; recorded != "" {
		onChain := cosmosigner.CanonicalSDKPublicKey(recorded)
		if onChain == "" {
			return false, fmt.Errorf("cannot verify the on-chain validator public key recorded in status")
		}
		if publicKey != onChain {
			conflict := fmt.Errorf("validator signing public key does not match the on-chain public key recorded in status; Cosmopilot does not rotate validator consensus keys")
			return false, r.quiesceValidatorOnReservationConflict(ctx, chainNode, conflict)
		}
	}
	holder := validatorReservationHolder(chainNode)
	if err := cosmosigner.EnsureConsensusKeyReservation(ctx, r.Client, chainNode.Status.ChainID, publicKey, holder); err != nil {
		if errors.Is(err, cosmosigner.ErrConsensusKeyReservationConflict) {
			return false, r.quiesceValidatorOnReservationConflict(ctx, chainNode, err)
		}
		return false, err
	}
	if verifiedIdentity != "" && chainNode.Status.TmKMSReservationIdentity != verifiedIdentity {
		chainNode.Status.TmKMSReservationIdentity = verifiedIdentity
		if err := r.Status().Update(ctx, chainNode); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

func validatorReservationHolder(chainNode *appsv1.ChainNode) cosmosigner.ReservationHolder {
	holder := cosmosigner.ReservationHolder{
		UID: chainNode.GetUID(), Kind: "ChainNode", Namespace: chainNode.GetNamespace(),
		Name: chainNode.GetName(), Claim: chainNode.GetName(),
	}
	if owner := metav1.GetControllerOf(chainNode); owner != nil && owner.Kind == "ChainNodeSet" {
		holder.UID = owner.UID
		holder.Kind = owner.Kind
		holder.Name = owner.Name
	}
	return holder
}

func (r *Reconciler) validatorConsensusPublicKey(ctx context.Context, chainNode *appsv1.ChainNode) (string, string, error) {
	if !chainNode.UsesTmKms() {
		publicKey, err := cosmosigner.PublicKeyFromSecret(ctx, r.Client, chainNode.GetNamespace(), chainNode.Spec.Validator.GetPrivKeySecretName(chainNode))
		return publicKey, "", err
	}

	hashicorp := chainNode.Spec.Validator.TmKMS.Provider.Hashicorp
	if hashicorp == nil {
		return "", "", fmt.Errorf("validator has no supported tmKMS provider configured")
	}
	if strings.TrimSpace(hashicorp.Address) == "" || strings.TrimSpace(hashicorp.Key) == "" {
		return "", "", fmt.Errorf("tmKMS Hashicorp address and key are required")
	}
	identity := chainNode.EffectiveSigningIdentity()

	// Before an uploadGenerated key reaches Vault, the local source Secret is the authoritative key
	// that the TmKMS sidecar will use. Reserving it first closes the create/upload race.
	uploaded := chainNode.Annotations[controllers.AnnotationVaultKeyUploaded] == strconv.FormatBool(true)
	if chainNode.ShouldUploadVaultKey() && !uploaded {
		publicKey, err := cosmosigner.PublicKeyFromSecret(ctx, r.Client, chainNode.GetNamespace(), chainNode.Spec.Validator.GetPrivKeySecretName(chainNode))
		return publicKey, "", err
	}
	if err := requireTmKMSSecret(ctx, r.Client, chainNode.GetNamespace(), "Vault token", hashicorp.TokenSecret); err != nil {
		return "", "", err
	}
	if hashicorp.CertificateSecret != nil {
		if err := requireTmKMSSecret(ctx, r.Client, chainNode.GetNamespace(), "Vault certificate", hashicorp.CertificateSecret); err != nil {
			return "", "", err
		}
	}
	if chainNode.Status.TmKMSReservationIdentity == identity {
		if publicKey := cosmosigner.CanonicalSDKPublicKey(chainNode.Status.PubKey); publicKey != "" {
			return publicKey, "", nil
		}
	}
	publicKey, err := r.fallbackTmKMSPublicKey(ctx, chainNode, hashicorp)
	return publicKey, identity, err
}

func requireTmKMSSecret(ctx context.Context, c client.Client, namespace, purpose string, selector *corev1.SecretKeySelector) error {
	if selector == nil || selector.Name == "" || selector.Key == "" {
		return fmt.Errorf("tmKMS %s secret selector must set both name and key", purpose)
	}
	secret := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: selector.Name}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("tmKMS %s secret %q not found", purpose, selector.Name)
		}
		return err
	}
	if len(secret.Data[selector.Key]) == 0 {
		return fmt.Errorf("tmKMS %s secret %q is missing required key %q", purpose, selector.Name, selector.Key)
	}
	return nil
}

func (r *Reconciler) quiesceValidatorOnReservationConflict(ctx context.Context, chainNode *appsv1.ChainNode, conflict error) error {
	pod, err := r.getChainNodePod(ctx, chainNode)
	if err != nil {
		return fmt.Errorf("%w; failed to inspect the conflicting validator pod: %v", conflict, err)
	}
	if pod == nil {
		return conflict
	}
	if !metav1.IsControlledBy(pod, chainNode) {
		return fmt.Errorf("%w; refusing to delete non-owned pod %s/%s", conflict, pod.Namespace, pod.Name)
	}
	if pod.DeletionTimestamp == nil {
		if err := r.Delete(ctx, pod); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("%w; failed to stop conflicting validator pod %s/%s: %v", conflict, pod.Namespace, pod.Name, err)
		}
	}
	key := types.NamespacedName{Namespace: pod.Namespace, Name: pod.Name}
	if err := wait.PollUntilContextTimeout(ctx, 250*time.Millisecond, timeoutPodDeleted, true, func(ctx context.Context) (bool, error) {
		current := &corev1.Pod{}
		if err := r.Get(ctx, key, current); err != nil {
			return apierrors.IsNotFound(err), client.IgnoreNotFound(err)
		}
		return false, nil
	}); err != nil {
		return fmt.Errorf("%w; timed out stopping conflicting validator pod %s/%s: %v", conflict, pod.Namespace, pod.Name, err)
	}
	return conflict
}
