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
// distinguish nil (default class) from an explicit "" (no class). `desiredClass` is the class the
// current spec's template would use; the orphaned-PVC recovery path reports it verbatim, because a
// recreated StatefulSet re-binds the surviving claims by name (its template class governs only new
// ordinals, and the membership is locked) — so the recovered cluster's class is fixed by the existing
// PVs, and recording the desired value keeps an unchanged template from reading as a storage change
// without needing a cluster-scoped StorageClass lookup.
func ReadSignerLock(ctx context.Context, c client.Client, owner metav1.Object, namespace, name string, desiredClass *string) (
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
		// The StatefulSet is gone, but StatefulSet PVCs are not garbage-collected with it, so this
		// owner's per-pod raft-state claims may survive (a manually deleted StatefulSet, or a partial
		// teardown). A fresh StatefulSet would re-bind them, inheriting their raft membership, so recover
		// the lock from those OWNED exact-match claims rather than falling back to the (possibly changed)
		// spec: the surviving claim count is the membership the cluster was formed with.
		pvcs := &corev1.PersistentVolumeClaimList{}
		if err := c.List(ctx, pvcs, client.InNamespace(namespace)); err != nil {
			return 0, "", nil, false, false, err
		}
		owned := 0
		maxOrdinal := -1
		var sample *corev1.PersistentVolumeClaim
		for i := range pvcs.Items {
			pvc := &pvcs.Items[i]
			// An exact-name state PVC WITHOUT this owner's UID label is ambiguous: a fresh StatefulSet
			// would still bind it by name, but its raft membership cannot be attributed to this owner
			// (ApplyOwned blocks on it for exactly this reason). Falling through to record a spec-derived
			// lock while it exists would let a spec that drifted before this reconcile pass validation,
			// and a later manual adoption of the claim would then re-bind old raft state under that wrong
			// lock. Fail closed instead — mirroring the teardown/adopt path — so no lock is recorded
			// until the operator deletes or labels the claim.
			if isAmbiguousLegacyDataPVC(pvc, name) {
				return 0, "", nil, false, false, fmt.Errorf(
					"cosmosigner %q has state PVC %q without an owner-UID label: cannot attribute its raft membership; "+
						"delete the claim or label it with this owner's UID before reconciling",
					name, pvc.GetName())
			}
			ordinal, ok := ownedStatefulSetDataPVCOrdinal(pvc, owner, name)
			if !ok {
				continue
			}
			owned++
			if ordinal > maxOrdinal {
				maxOrdinal = ordinal
			}
			if sample == nil {
				sample = pvc
			}
		}
		if owned > 0 {
			// The recovered replica count is only trustworthy when the surviving ordinals form the
			// complete contiguous set {0..owned-1}. Claim names are distinct and each ordinal is a
			// canonical non-negative integer, so owned == maxOrdinal+1 proves exactly that set. A gap
			// (e.g. {0, 2}) means a claim was deleted from the MIDDLE of the raft membership: a fresh
			// StatefulSet would recreate that ordinal with empty state while re-binding the survivors,
			// forming a cluster with a different membership than the one it was bootstrapped with. That
			// is unsafe to guess through, so fail closed and let the operator resolve the claims rather
			// than record a membership the count cannot prove. (A set truncated from the TOP — a plain
			// scale-down — stays contiguous and is accepted.)
			if owned != maxOrdinal+1 {
				return 0, "", nil, false, false, fmt.Errorf(
					"cosmosigner %q has an incomplete set of orphaned state PVCs (%d claims, highest ordinal %d): "+
						"cannot determine the original raft membership; remove the stale claims or restore the missing ones",
					name, owned, maxOrdinal)
			}
			replicas = int32(owned)
			foundReplicas = true
			if q, ok := sample.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
				storageSize = q.String()
			}
			// Report the DESIRED storage class, not the one read off the PVC. A fresh StatefulSet re-binds
			// these claims by NAME regardless of its template's storageClassName (which only governs claims
			// created for NEW ordinals — and the membership is locked above, so none are), so the class the
			// recovered cluster runs on is fixed by the existing PVs, not the template. Recording the raw
			// PVC class would also misread an omitting template (whose PVC carries the admission-materialised
			// cluster default) as a storage change, and reading it back would need a cluster-scoped
			// StorageClass lookup the operator is not granted. The desired value round-trips against the
			// no-webhook guard while a genuine size change is still caught above.
			storageClass = desiredClass
			foundStorage = true
		}
		// No StatefulSet and no owned claims: a true first rollout, caller uses the spec.
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
