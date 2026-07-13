package cosmosigner

import (
	"context"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ReadSignerLock initialises the per-signer Replicas/StateStorageSize/ClassName from the live
// signer state owned by `owner` (the StatefulSet, when present) and falls back to no value when no
// signer state exists yet (a true first rollout). PVC-template data comes from the live
// volumeClaimTemplates (we never need to read PVCs themselves for this — the templates fully describe
// the size and storage class the StatefulSet would re-bind on a recreate).
//
// Anchoring the lock on the live signer state prevents an in-flight roll-out (failed first
// reconcile, or an already-deployed signer whose status was lost) from being "re-locked" to a
// different replica count or PVC template than the one the raft cluster was actually formed with:
// the no-webhook guard's raft-membership and PVC-template immutability invariants compare against
// the spec, so a wrong recorded value would allow the spec to change and re-apply a fresh
// bootstrap membership over the surviving PVCs.
func ReadSignerLock(ctx context.Context, c client.Client, owner metav1.Object, namespace, name string) (
	replicas int32, storageSize, storageClass string, foundReplicas, foundStorage bool, err error,
) {
	sts := &appsv1.StatefulSet{}
	stsErr := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, sts)
	switch {
	case stsErr == nil:
		if metav1.IsControlledBy(sts, owner) {
			if sts.Spec.Replicas != nil {
				replicas = *sts.Spec.Replicas
				foundReplicas = true
			}
			for _, t := range sts.Spec.VolumeClaimTemplates {
				if t.Name == dataVolumeName {
					if t.Spec.StorageClassName == nil {
						storageClass = ""
					} else {
						storageClass = *t.Spec.StorageClassName
					}
					if q, ok := t.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
						storageSize = q.String()
					}
					foundStorage = true
					break
				}
			}
		}
	case errors.IsNotFound(stsErr):
		// No signer state: true first rollout, caller uses the spec.
	default:
		return 0, "", "", false, false, stsErr
	}
	return replicas, storageSize, storageClass, foundReplicas, foundStorage, nil
}
