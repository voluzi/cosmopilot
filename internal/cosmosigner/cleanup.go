package cosmosigner

import (
	"context"
	"strconv"
	"strings"

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
	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := c.List(ctx, pvcs, client.InNamespace(namespace), client.MatchingLabels(InstanceLabels(name))); err != nil {
		return err
	}
	for i := range pvcs.Items {
		if !isOwnedStatefulSetDataPVC(&pvcs.Items[i], owner, name) {
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
// label equals owner's UID. Matching is STRICT — a claim without a matching label is never deleted,
// regardless of any point-in-time StatefulSet ownership read (all such reads are racy: the
// StatefulSet can be replaced between the check and the delete). Ambiguous unlabeled claims instead
// BLOCK teardown completion (see isAmbiguousLegacyDataPVC), so no signer ever binds them silently.
func isOwnedStatefulSetDataPVC(pvc *corev1.PersistentVolumeClaim, owner metav1.Object, name string) bool {
	if !isStatefulSetDataPVC(pvc.GetName(), name) {
		return false
	}
	return pvc.GetLabels()[labelOwnerUID] == string(owner.GetUID())
}

// isAmbiguousLegacyDataPVC reports whether pvc is a per-pod raft-state claim of the signer named
// `name` that carries NO owner-UID label. Such claims predate the owner label (an existing
// StatefulSet keeps its original volumeClaimTemplates, since Kubernetes forbids updating them) and
// cannot be attributed to any owner without a race, so they are never deleted NOR treated as gone:
// they block IsTornDown until the operator resolves them (delete the claim, or label it with the
// owning CR's UID). This keeps a recreated signer from silently binding stale raft state whose
// membership is unknown. In practice such claims only exist on pre-release deployments of this
// feature, so the block is a safety net rather than an operational path.
func isAmbiguousLegacyDataPVC(pvc *corev1.PersistentVolumeClaim, name string) bool {
	if !isStatefulSetDataPVC(pvc.GetName(), name) {
		return false
	}
	_, labeled := pvc.GetLabels()[labelOwnerUID]
	return !labeled
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
