package chainnode

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/cosmosigner"
)

func (r *Reconciler) prepareConsensusKeyReservationOwner(ctx context.Context, chainNode *appsv1.ChainNode) (bool, error) {
	root, managedChild, err := r.consensusKeyReservationRoot(ctx, chainNode)
	if err != nil {
		return false, err
	}
	if managedChild {
		if !chainNode.IsValidator() || chainNode.Spec.RemoteSignerTarget {
			return false, nil
		}
		return cosmosigner.EnsureConsensusKeyReservationOwnerFinalizer(ctx, r.reservationReader(), r.Client, root)
	}

	needsFinalizer := chainNode.IsValidator() || chainNode.Spec.Cosmosigner != nil ||
		chainNode.Status.CosmosignerPublicKey != "" || chainNode.Status.TmKMSReservationIdentity != ""
	if !needsFinalizer {
		needsFinalizer, err = cosmosigner.HasConsensusKeyReservationsForOwner(ctx, r.reservationReader(), root)
		if err != nil {
			return false, err
		}
	}
	if !needsFinalizer {
		return false, nil
	}
	return cosmosigner.EnsureConsensusKeyReservationOwnerFinalizer(ctx, r.reservationReader(), r.Client, root)
}

func (r *Reconciler) ensureConsensusKeyReservation(ctx context.Context, chainNode *appsv1.ChainNode, chainID, publicKey string, holder cosmosigner.ReservationHolder) error {
	result, err := cosmosigner.EnsureConsensusKeyReservationWithResult(ctx, r.reservationReader(), r.Client, chainID, publicKey, holder)
	if err != nil {
		if (errors.Is(err, cosmosigner.ErrConsensusKeyReservationRecoveryBlocked) ||
			errors.Is(err, cosmosigner.ErrConsensusKeyReservationConflict)) && r.recorder != nil {
			r.recorder.Eventf(chainNode, corev1.EventTypeWarning, appsv1.ReasonConsensusKeyReservationBlocked, "%v", err)
		}
		return err
	}
	if result.RecoveredReservation != "" && r.recorder != nil {
		r.recorder.Eventf(chainNode, corev1.EventTypeNormal, appsv1.ReasonConsensusKeyReservationRecovered,
			"Recovered stale ConsensusKeyReservation %s after confirming owner UID %s has no signing path", result.RecoveredReservation, holder.UID)
	}
	return nil
}

func (r *Reconciler) consensusKeyReservationRoot(ctx context.Context, chainNode *appsv1.ChainNode) (client.Object, bool, error) {
	owner := metav1.GetControllerOf(chainNode)
	if owner == nil || owner.Kind != "ChainNodeSet" {
		return chainNode, false, nil
	}
	root := &appsv1.ChainNodeSet{}
	if err := r.reservationReader().Get(ctx, client.ObjectKey{Namespace: chainNode.GetNamespace(), Name: owner.Name}, root); err != nil {
		return nil, true, err
	}
	if root.GetUID() != owner.UID {
		return nil, true, fmt.Errorf("ChainNodeSet root %s/%s changed UID from %q to %q", root.GetNamespace(), root.GetName(), owner.UID, root.GetUID())
	}
	return root, true, nil
}

func (r *Reconciler) finalizeConsensusKeyReservationOwner(ctx context.Context, chainNode *appsv1.ChainNode) (bool, error) {
	if !controllerutil.ContainsFinalizer(chainNode, cosmosigner.ReservationOwnerFinalizer) {
		hasReservations, err := cosmosigner.HasConsensusKeyReservationsForOwner(ctx, r.reservationReader(), chainNode)
		if err != nil || !hasReservations {
			return !hasReservations, err
		}
	}
	pathsGone, err := cosmosigner.FinalizeConsensusKeySigningPaths(ctx, r.reservationReader(), r.Client, chainNode, chainNode.GetNamespace())
	if err != nil || !pathsGone {
		return false, err
	}
	pathsGone, err = r.finalizeOwnedSigningPods(ctx, chainNode)
	if err != nil || !pathsGone {
		return false, err
	}
	released, done, err := cosmosigner.ReleaseConsensusKeyReservations(ctx, r.reservationReader(), r.Client, chainNode)
	if err != nil || !done {
		return false, err
	}
	for _, name := range released {
		if r.recorder != nil {
			r.recorder.Eventf(chainNode, corev1.EventTypeNormal, appsv1.ReasonConsensusKeyReservationReleased,
				"ConsensusKeyReservation %s released after signing-path teardown", name)
		}
	}
	return true, r.removeConsensusKeyReservationFinalizer(ctx, chainNode)
}

func (r *Reconciler) finalizeOwnedSigningPods(ctx context.Context, chainNode *appsv1.ChainNode) (bool, error) {
	pods := &corev1.PodList{}
	if err := r.reservationReader().List(ctx, pods, client.InNamespace(chainNode.GetNamespace())); err != nil {
		return false, err
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		owner := metav1.GetControllerOf(pod)
		owned := owner != nil && owner.UID == chainNode.GetUID()
		if isChainNodeSigningPodName(pod.GetName(), chainNode.GetName()) && !owned {
			return false, fmt.Errorf("refusing to release consensus-key reservations while signing-path pod %s/%s is not owned by ChainNode UID %q", pod.GetNamespace(), pod.GetName(), chainNode.GetUID())
		}
		if !owned {
			continue
		}
		if pod.GetUID() == "" {
			return false, fmt.Errorf("owned pod %s/%s has no UID; refusing an unguarded delete", pod.GetNamespace(), pod.GetName())
		}
		uid := pod.GetUID()
		if pod.GetDeletionTimestamp().IsZero() {
			if err := r.Delete(ctx, pod, client.Preconditions{UID: &uid}); err != nil && !apierrors.IsNotFound(err) {
				return false, err
			}
		}
		remaining := &corev1.Pod{}
		if err := r.reservationReader().Get(ctx, client.ObjectKeyFromObject(pod), remaining); err == nil {
			return false, nil
		} else if !apierrors.IsNotFound(err) {
			return false, err
		}
	}
	return true, nil
}

func (r *Reconciler) removeConsensusKeyReservationFinalizer(ctx context.Context, chainNode *appsv1.ChainNode) error {
	fresh := &appsv1.ChainNode{}
	if err := r.reservationReader().Get(ctx, client.ObjectKeyFromObject(chainNode), fresh); err != nil {
		return client.IgnoreNotFound(err)
	}
	if fresh.GetUID() != chainNode.GetUID() {
		return fmt.Errorf("ChainNode %s/%s changed UID while removing reservation finalizer", chainNode.GetNamespace(), chainNode.GetName())
	}
	if !controllerutil.ContainsFinalizer(fresh, cosmosigner.ReservationOwnerFinalizer) {
		return nil
	}
	controllerutil.RemoveFinalizer(fresh, cosmosigner.ReservationOwnerFinalizer)
	return r.Update(ctx, fresh)
}
