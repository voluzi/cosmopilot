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
// label equals owner's UID. Matching is STRICT — an unlabeled claim is never deleted on a guess.
// Legacy claims (created before the label existed) are attributed via AdoptLegacyPVCs, which labels
// them only under a race-free ownership proof.
func isOwnedStatefulSetDataPVC(pvc *corev1.PersistentVolumeClaim, owner metav1.Object, name string) bool {
	if !isStatefulSetDataPVC(pvc.GetName(), name) {
		return false
	}
	return pvc.GetLabels()[labelOwnerUID] == string(owner.GetUID())
}

// AdoptLegacyPVCs stamps the owner-UID label onto unlabeled legacy raft-state claims of the signer
// named `name`, but ONLY while a StatefulSet with that name exists and is controlled by owner: while
// our StatefulSet holds the name, its per-pod claims are necessarily ours (a foreign signer cannot
// hold the same name concurrently), so labeling under that proof is race-free — unlike deciding at
// delete time from a point-in-time "no foreign StatefulSet" read, which could adopt (and delete)
// claims a concurrently-created foreign signer is about to bind. Claims that cannot be proven ours
// are left unlabeled and are never deleted (preserved rather than guessed at). Labeling uses plain
// Updates, so a concurrent writer surfaces as a conflict error and the reconcile retries.
func AdoptLegacyPVCs(ctx context.Context, c client.Client, owner metav1.Object, namespace, name string) error {
	sts := &appsv1.StatefulSet{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, sts); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if !metav1.IsControlledBy(sts, owner) {
		return nil
	}

	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := c.List(ctx, pvcs, client.InNamespace(namespace), client.MatchingLabels(InstanceLabels(name))); err != nil {
		return err
	}
	for i := range pvcs.Items {
		pvc := &pvcs.Items[i]
		if !isStatefulSetDataPVC(pvc.GetName(), name) {
			continue
		}
		if _, labeled := pvc.GetLabels()[labelOwnerUID]; labeled {
			continue
		}
		labels := pvc.GetLabels()
		if labels == nil {
			labels = map[string]string{}
		}
		labels[labelOwnerUID] = string(owner.GetUID())
		pvc.SetLabels(labels)
		if err := c.Update(ctx, pvc); err != nil {
			return err
		}
	}
	return nil
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
