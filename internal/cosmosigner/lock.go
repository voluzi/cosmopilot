package cosmosigner

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// defaultStorageClassAnnotation marks the cluster's default StorageClass (the class the API server
// materialises into a PVC that omits storageClassName).
const defaultStorageClassAnnotation = "storageclass.kubernetes.io/is-default-class"

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
// current spec's template would use; it lets the orphaned-PVC recovery path report the desired value
// (rather than the admission-materialised default) when the two are equivalent, so an unchanged
// template is not misread as a storage change.
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
		// teardown). A fresh StatefulSet would re-bind them, inheriting their raft membership and
		// size/class, so recover the lock from those OWNED exact-match claims rather than falling back
		// to the (possibly changed) spec: the surviving claim count is the membership the cluster was
		// formed with, and each claim carries its own size/class.
		pvcs := &corev1.PersistentVolumeClaimList{}
		if err := c.List(ctx, pvcs, client.InNamespace(namespace)); err != nil {
			return 0, "", nil, false, false, err
		}
		owned := 0
		maxOrdinal := -1
		var sample *corev1.PersistentVolumeClaim
		for i := range pvcs.Items {
			ordinal, ok := ownedStatefulSetDataPVCOrdinal(&pvcs.Items[i], owner, name)
			if !ok {
				continue
			}
			owned++
			if ordinal > maxOrdinal {
				maxOrdinal = ordinal
			}
			if sample == nil {
				sample = &pvcs.Items[i]
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
			// A PVC created from a template that OMITTED storageClassName carries the admission-filled
			// cluster default class, while the desired template still omits it. Reporting the raw default
			// class would make the no-webhook guard reject an unchanged template as a storage change, so
			// map the recovered class back to the desired value when the two are equivalent under
			// default-class semantics; report the raw class only when it genuinely differs.
			recovered, err := reconcileRecoveredStorageClass(ctx, c, sample.Spec.StorageClassName, desiredClass)
			if err != nil {
				return 0, "", nil, false, false, err
			}
			storageClass = recovered
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

// reconcileRecoveredStorageClass maps the storage class read off an orphaned PVC back to the desired
// template's class when the two are equivalent under Kubernetes default-class semantics: a template
// that omits storageClassName binds a PVC carrying the admission-filled cluster default, so reporting
// that default verbatim would later read as a storage change against the still-omitted template. It
// returns `desiredClass` when they match exactly, or when `desiredClass` omits the class and the PVC's
// class is the cluster default; otherwise it returns the PVC's class unchanged (a genuine difference
// the guard must catch).
func reconcileRecoveredStorageClass(ctx context.Context, c client.Client, pvcClass, desiredClass *string) (*string, error) {
	if ptr.Equal(pvcClass, desiredClass) {
		return desiredClass, nil
	}
	if (desiredClass == nil || *desiredClass == "") && pvcClass != nil && *pvcClass != "" {
		def, err := defaultStorageClassName(ctx, c)
		if err != nil {
			return nil, err
		}
		if def != "" && *pvcClass == def {
			return desiredClass, nil
		}
	}
	return pvcClass, nil
}

// defaultStorageClassName returns the name of the cluster's default StorageClass (annotated
// is-default-class=true), or "" when none is marked default.
func defaultStorageClassName(ctx context.Context, c client.Client) (string, error) {
	list := &storagev1.StorageClassList{}
	if err := c.List(ctx, list); err != nil {
		return "", err
	}
	for i := range list.Items {
		if list.Items[i].Annotations[defaultStorageClassAnnotation] == "true" {
			return list.Items[i].Name, nil
		}
	}
	return "", nil
}
