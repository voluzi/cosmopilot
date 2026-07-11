package cosmosigner

import (
	"context"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// DeletePVCs deletes the per-pod raft-state PVCs of a signer instance owned by owner. StatefulSet
// PVCs are not garbage-collected with the StatefulSet, so they are cleaned up explicitly on teardown.
// A claim is only deleted when it carries the instance labels, its name matches the exact StatefulSet
// per-pod claim pattern `<dataVolumeName>-<name>-<ordinal>`, AND its owner-UID label matches owner —
// so an unrelated claim that happens to share the labels or a name prefix (e.g. `data-<name>-backup`),
// or a same-name signer's claim owned by another CR, is never deleted. List+Delete uses only the
// list/delete verbs the controllers already hold (no deletecollection).
func DeletePVCs(ctx context.Context, c client.Client, owner metav1.Object, namespace, name string) error {
	// Unlabeled legacy claims are only adoptable when no foreign same-name StatefulSet exists —
	// otherwise they may be that (legacy) signer's live raft state.
	foreign, err := foreignSignerHoldsName(ctx, c, owner, namespace, name)
	if err != nil {
		return err
	}
	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := c.List(ctx, pvcs, client.InNamespace(namespace), client.MatchingLabels(InstanceLabels(name))); err != nil {
		return err
	}
	for i := range pvcs.Items {
		if !isOwnedStatefulSetDataPVC(&pvcs.Items[i], owner, name, !foreign) {
			continue
		}
		if err := c.Delete(ctx, &pvcs.Items[i]); err != nil && !errors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// isOwnedStatefulSetDataPVC reports whether pvc is a per-pod raft-state claim of the signer named
// `name` attributable to owner: its name matches the StatefulSet per-pod pattern and its owner-UID
// label equals owner's UID. A claim carrying NO owner-UID label may be adopted as owner's when
// adoptUnlabeled is true (legacy claim created before the label existed — an existing StatefulSet
// keeps its original volumeClaimTemplates, since Kubernetes forbids updating them, so such claims
// can never gain the label); excluding legacy claims entirely would strand their raft state past
// teardown, letting a re-added signer bind it after the guards cleared. Callers must pass
// adoptUnlabeled=false when a same-name StatefulSet owned by a DIFFERENT CR exists: the unlabeled
// claims may then be that (legacy) signer's live raft state, which must never be deleted from here.
func isOwnedStatefulSetDataPVC(pvc *corev1.PersistentVolumeClaim, owner metav1.Object, name string, adoptUnlabeled bool) bool {
	if !isStatefulSetDataPVC(pvc.GetName(), name) {
		return false
	}
	uid, labeled := pvc.GetLabels()[labelOwnerUID]
	if !labeled {
		return adoptUnlabeled
	}
	return uid == string(owner.GetUID())
}

// foreignSignerHoldsName reports whether a StatefulSet with the signer's name exists and is
// controlled by a different owner. Unlabeled legacy PVCs are only adoptable when this is false.
func foreignSignerHoldsName(ctx context.Context, c client.Client, owner metav1.Object, namespace, name string) (bool, error) {
	sts := &appsv1.StatefulSet{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, sts); err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return !metav1.IsControlledBy(sts, owner), nil
}

// isStatefulSetDataPVC reports whether pvcName is exactly `<dataVolumeName>-<stsName>-<ordinal>`,
// the name the StatefulSet controller gives the signer's per-pod state claims.
func isStatefulSetDataPVC(pvcName, stsName string) bool {
	prefix := dataVolumeName + "-" + stsName + "-"
	if !strings.HasPrefix(pvcName, prefix) {
		return false
	}
	ordinal := strings.TrimPrefix(pvcName, prefix)
	if ordinal == "" {
		return false
	}
	// Require a canonical non-negative ordinal (no sign, no leading zeros) — exactly what the
	// StatefulSet controller produces — so names like "data-<name>--1" or "data-<name>-007" never match.
	n, err := strconv.Atoi(ordinal)
	return err == nil && n >= 0 && strconv.Itoa(n) == ordinal
}
