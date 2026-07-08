package cosmosigner

import (
	"context"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// DeletePVCs deletes the per-pod raft-state PVCs of a signer instance. StatefulSet PVCs are not
// garbage-collected with the StatefulSet, so they are cleaned up explicitly on teardown. They are
// matched by both the instance labels and the StatefulSet PVC name prefix (`data-<name>-`) so an
// unrelated claim that happens to share the labels is never deleted. List+Delete uses only the
// list/delete verbs the controllers already hold (no deletecollection).
func DeletePVCs(ctx context.Context, c client.Client, namespace, name string) error {
	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := c.List(ctx, pvcs, client.InNamespace(namespace), client.MatchingLabels(InstanceLabels(name))); err != nil {
		return err
	}
	prefix := "data-" + name + "-"
	for i := range pvcs.Items {
		if !strings.HasPrefix(pvcs.Items[i].GetName(), prefix) {
			continue
		}
		if err := c.Delete(ctx, &pvcs.Items[i]); err != nil && !errors.IsNotFound(err) {
			return err
		}
	}
	return nil
}
