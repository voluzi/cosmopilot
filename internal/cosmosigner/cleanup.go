package cosmosigner

import (
	"context"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// DeletePVCs deletes the per-pod raft-state PVCs of a signer instance. StatefulSet PVCs are not
// garbage-collected with the StatefulSet, so they are cleaned up explicitly on teardown. A claim is
// only deleted when it carries the instance labels AND its name matches the exact StatefulSet
// per-pod claim pattern `<dataVolumeName>-<name>-<ordinal>`, so an unrelated claim that happens to
// share the labels or a name prefix (e.g. `data-<name>-backup`) is never deleted. List+Delete uses
// only the list/delete verbs the controllers already hold (no deletecollection).
func DeletePVCs(ctx context.Context, c client.Client, namespace, name string) error {
	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := c.List(ctx, pvcs, client.InNamespace(namespace), client.MatchingLabels(InstanceLabels(name))); err != nil {
		return err
	}
	for i := range pvcs.Items {
		if !isStatefulSetDataPVC(pvcs.Items[i].GetName(), name) {
			continue
		}
		if err := c.Delete(ctx, &pvcs.Items[i]); err != nil && !errors.IsNotFound(err) {
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
