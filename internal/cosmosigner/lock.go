package cosmosigner

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ReadSignerLock initialises the per-signer Replicas/StateStorageSize/ClassName from the live signer
// state owned by `owner` (the StatefulSet, when present). It falls back to no value (found=false) when
// no signer state exists yet (a true first rollout) or when the live state is transient. PVC-template
// data comes from the live volumeClaimTemplates, which fully describe the size and storage class the
// StatefulSet would re-bind on a recreate.
//
// Anchoring the lock on the live signer state prevents an in-flight roll-out (failed first reconcile,
// or an already-deployed signer whose status was lost) from being "re-locked" to a different replica
// count or PVC template than the one the raft cluster was actually formed with: the no-webhook guard's
// raft-membership and PVC-template immutability invariants compare against the spec, so a wrong
// recorded value would allow the spec to change and re-apply a fresh bootstrap membership over the
// surviving PVCs.
//
// The storage class is returned as a *string so a template that OMITS storageClassName (the normal
// "use the cluster default" case) round-trips as nil rather than "": callers and the no-webhook guard
// distinguish nil (default class) from an explicit "" (no class). When the StatefulSet is gone but its
// raft-state PVCs survive, the lock cannot be reconstructed from the claims alone, so it fails closed
// rather than adopting an unverifiable membership.
func ReadSignerLock(ctx context.Context, c client.Client, owner metav1.Object, namespace, name string) (
	replicas int32, storageSize string, storageClass *string, foundReplicas, foundStorage bool, err error,
) {
	sts := &appsv1.StatefulSet{}
	stsErr := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, sts)
	switch {
	case stsErr == nil:
		if metav1.IsControlledBy(sts, owner) {
			// A signer scaled to zero is transiently quiesced (a Vault re-import in flight, or a
			// failed one): spec.replicas == 0 is NOT the raft membership the cluster was formed with,
			// and the CRD forbids replicas == 0, so recording it would wedge every later comparison.
			// Leave foundReplicas false so the caller falls back to the (immutable) desired count.
			if sts.Spec.Replicas != nil && *sts.Spec.Replicas > 0 {
				replicas = *sts.Spec.Replicas
				foundReplicas = true
			}
			for _, t := range sts.Spec.VolumeClaimTemplates {
				if t.Name == dataVolumeName {
					storageClass = t.Spec.StorageClassName
					if q, ok := t.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
						storageSize = q.String()
					}
					foundStorage = true
					break
				}
			}
		}
	case errors.IsNotFound(stsErr):
		// The StatefulSet is gone but its per-pod raft-state PVCs are not garbage-collected with it, so
		// this owner's `data-<name>-<ordinal>` claims (or unlabeled legacy ones) may survive. Their raft
		// logs were bootstrapped with a membership that CANNOT be reconstructed from the claims alone: a
		// surviving subset of ordinals is indistinguishable from a smaller original cluster (a lone
		// `data-<name>-0` looks identical whether the raft cluster was 1 replica or the truncated remains
		// of 3), and an unlabeled claim cannot even be attributed to this owner. Recording any lock while
		// such a claim exists would let a recreated StatefulSet re-bind that state under a membership it
		// was never formed with (breaking quorum) or let a drifted spec pass validation, so fail closed
		// the moment one is found and let the operator resolve it (delete the claims, or restore the
		// StatefulSet). A namespace with no surviving state claim is a true first rollout.
		pvcs := &corev1.PersistentVolumeClaimList{}
		if err := c.List(ctx, pvcs, client.InNamespace(namespace)); err != nil {
			return 0, "", nil, false, false, err
		}
		for i := range pvcs.Items {
			pvc := &pvcs.Items[i]
			_, owned := ownedStatefulSetDataPVCOrdinal(pvc, owner, name)
			if owned || isAmbiguousLegacyDataPVC(pvc, name) {
				return 0, "", nil, false, false, fmt.Errorf(
					"cosmosigner %q has an orphaned raft-state PVC %q but no StatefulSet or recorded lock: its raft "+
						"membership cannot be reconstructed from the claim alone; delete the stale claims or restore the "+
						"signer before reconciling", name, pvc.GetName())
			}
		}
		// No StatefulSet and no surviving state claim: a true first rollout, caller uses the spec.
	default:
		return 0, "", nil, false, false, stsErr
	}
	return replicas, storageSize, storageClass, foundReplicas, foundStorage, nil
}

// ownedStatefulSetDataPVCOrdinal returns the pod ordinal of a per-pod raft-state claim that belongs to
// `owner` and the signer named `name`, or ok=false when the claim is not such an owned data PVC.
func ownedStatefulSetDataPVCOrdinal(pvc *corev1.PersistentVolumeClaim, owner metav1.Object, name string) (int, bool) {
	if pvc.GetLabels()[labelOwnerUID] != string(owner.GetUID()) {
		return 0, false
	}
	return statefulSetDataPVCOrdinal(pvc.GetName(), name)
}
