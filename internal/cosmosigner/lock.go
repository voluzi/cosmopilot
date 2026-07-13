package cosmosigner

import (
	"context"

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
// distinguish nil (default class) from an explicit "" (no class).
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
		// No signer state: true first rollout, caller uses the spec.
	default:
		return 0, "", nil, false, false, stsErr
	}
	return replicas, storageSize, storageClass, foundReplicas, foundStorage, nil
}
