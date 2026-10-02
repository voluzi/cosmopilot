package chainnode

import (
	"context"
	"errors"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1 "github.com/voluzi/cosmopilot/v4/api/v1"
	"github.com/voluzi/cosmopilot/v4/internal/cosmosigner"
)

// ensureValidatorConsensusKeyReservation claims the active local consensus key before
// any signing configuration or validator pod is reconciled. ChainNodeSet signer targets are claimed
// by their parent signer preflight and must not create a second child-owned claim.
func (r *Reconciler) ensureValidatorConsensusKeyReservation(ctx context.Context, chainNode *appsv1.ChainNode) (bool, error) {
	if !chainNode.IsValidator() || chainNode.Spec.Cosmosigner != nil || chainNode.Spec.RemoteSignerTarget {
		return false, nil
	}
	if chainNode.Status.ChainID == "" {
		return false, fmt.Errorf("cannot reserve the validator consensus key: chain ID is not established")
	}

	publicKey, err := cosmosigner.PublicKeyFromSecret(ctx, r.Client, chainNode.GetNamespace(), chainNode.Spec.Validator.GetPrivKeySecretName(chainNode))
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
	if err := r.ensureConsensusKeyReservation(ctx, chainNode, chainNode.Status.ChainID, publicKey, holder); err != nil {
		if errors.Is(err, cosmosigner.ErrConsensusKeyReservationConflict) {
			return false, r.quiesceValidatorOnReservationConflict(ctx, chainNode, err)
		}
		return false, err
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
