package cosmosigner

import (
	"context"
	"fmt"
	"reflect"

	"github.com/banzaicloud/k8s-objectmatcher/patch"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/voluzi/cosmopilot/v2/internal/k8s"
)

// ApplyOwned creates or updates a cosmosigner-managed object owned by owner. It refuses to
// overwrite an object owned by a different controller (a same-name CR collision), preserves
// StatefulSet fields Kubernetes forbids updating, and skips the write entirely when nothing
// changed (patch-equality), so steady-state reconciles do not churn resourceVersions.
func ApplyOwned(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, obj client.Object) error {
	if err := controllerutil.SetControllerReference(owner, obj, scheme); err != nil {
		return err
	}
	existing, ok := obj.DeepCopyObject().(client.Object)
	if !ok {
		return fmt.Errorf("object is not a client.Object")
	}
	err := c.Get(ctx, client.ObjectKeyFromObject(obj), existing)
	if errors.IsNotFound(err) {
		return c.Create(ctx, obj)
	}
	if err != nil {
		return err
	}
	if !metav1.IsControlledBy(existing, owner) {
		return fmt.Errorf("cosmosigner resource %q is managed by another owner; refusing to overwrite it — rename the ChainNode/ChainNodeSet to avoid the name collision", obj.GetName())
	}

	k8s.PreserveImmutableStatefulSetFields(obj, existing)

	// Skip the write when nothing changed, so steady-state reconciles do not bump
	// resourceVersions (which would re-trigger the owner watch every cycle). The live object is
	// copied back into obj either way, so callers can read current status (e.g. ReadyReplicas).
	patchResult, err := patch.DefaultPatchMaker.Calculate(existing, obj, patch.IgnoreStatusFields())
	if err != nil {
		return err
	}
	if patchResult.IsEmpty() && reflect.DeepEqual(existing.GetLabels(), obj.GetLabels()) {
		reflect.ValueOf(obj).Elem().Set(reflect.ValueOf(existing).Elem())
		return nil
	}

	obj.SetResourceVersion(existing.GetResourceVersion())
	return c.Update(ctx, obj)
}

// ScaleDown scales an existing signer StatefulSet owned by owner to zero replicas and reports
// whether the signer is fully quiesced (no pods left). Used while a key re-import is pending: an
// already-running signer must not keep signing with the previously imported key while the target
// is being re-keyed. The scale-down is asynchronous, so callers must treat quiesced=false as
// "retry later" and NOT proceed with the import (nor re-apply the StatefulSet at full replicas,
// which would cancel the scale-down). A missing or foreign-owned StatefulSet counts as quiesced.
func ScaleDown(ctx context.Context, c client.Client, owner client.Object, namespace, name string) (quiesced bool, err error) {
	sts := &appsv1.StatefulSet{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, sts); err != nil {
		if errors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}
	if !metav1.IsControlledBy(sts, owner) {
		return true, nil
	}
	if sts.Spec.Replicas == nil || *sts.Spec.Replicas != 0 {
		zero := int32(0)
		sts.Spec.Replicas = &zero
		if err := c.Update(ctx, sts); err != nil {
			return false, err
		}
		// Just requested; pods are still terminating.
		return false, nil
	}
	// Already requested zero: quiesced only once the controller reports no replicas left.
	return sts.Status.Replicas == 0, nil
}

// IsRolledOut reports whether the signer StatefulSet's CURRENT generation is fully deployed: the
// controller has observed it, and every desired replica is both updated to the current revision and
// ready. Gating on this (rather than bare ReadyReplicas) prevents treating readiness left over from
// a previous revision as success for a pending change.
func IsRolledOut(ctx context.Context, c client.Client, namespace, name string, desiredReplicas int32) (bool, error) {
	sts := &appsv1.StatefulSet{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, sts); err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return sts.Status.ObservedGeneration == sts.Generation &&
		sts.Status.UpdatedReplicas == desiredReplicas &&
		sts.Status.ReadyReplicas == desiredReplicas, nil
}

// Undeploy removes the managed signer resources for the given base name, deleting only objects the
// owner controls. Each named resource is deleted only when this owner controls it, so a same-name
// resource owned by a different CR (a "<name>-signer" collision) is skipped rather than
// short-circuiting the whole teardown. Owner-scoped PVC cleanup always runs — even when a foreign
// StatefulSet holds the name — so this owner's lingering raft-state claims are never stranded (which
// would deadlock the IsTornDown gate waiting on them).
func Undeploy(ctx context.Context, c client.Client, owner client.Object, namespace, name string) error {
	objects := []client.Object{
		&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name + discoveryServiceSuffix, Namespace: namespace}},
	}
	for _, obj := range objects {
		if err := c.Get(ctx, client.ObjectKeyFromObject(obj), obj); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return err
		}
		if !metav1.IsControlledBy(obj, owner) {
			continue
		}
		if err := c.Delete(ctx, obj); err != nil && !errors.IsNotFound(err) {
			return err
		}
	}

	// StatefulSet PVCs are not garbage-collected with the StatefulSet. DeletePVCs filters on the
	// owner-UID label, so only this owner's claims are removed even when a foreign same-name signer
	// exists.
	return DeletePVCs(ctx, c, owner, namespace, name)
}

// IsTornDown reports whether the signer resources owned by owner are fully gone. Deletion is
// asynchronous — Undeploy only requests removal — so callers that must not act on a half-deleted
// cluster (e.g. clearing the recorded raft membership before allowing a re-add) gate on this.
//
// Only resources owned by owner count. A StatefulSet with the same name owned by ANOTHER CR (a name
// collision) is not ours to wait on — Undeploy skips it too — so it does not block. The per-pod PVCs
// are matched by the owner-UID label, so OUR lingering raft-state claims still gate the clear (even
// when a foreign same-name StatefulSet exists), while the foreign CR's identically-named claims do
// not. A claim already marked for deletion but held by a finalizer still counts as present, since a
// fresh StatefulSet could bind it and inherit stale raft state.
func IsTornDown(ctx context.Context, c client.Client, owner metav1.Object, namespace, name string) (bool, error) {
	foreign := false
	sts := &appsv1.StatefulSet{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, sts); err == nil {
		// A same-name StatefulSet exists. Only OUR StatefulSet blocks teardown completion; a foreign
		// one falls through to the owner-scoped PVC check below (its unlabeled legacy claims are then
		// not attributed to us, mirroring DeletePVCs).
		if metav1.IsControlledBy(sts, owner) {
			return false, nil
		}
		foreign = true
	} else if !errors.IsNotFound(err) {
		return false, err
	}

	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := c.List(ctx, pvcs, client.InNamespace(namespace), client.MatchingLabels(InstanceLabels(name))); err != nil {
		return false, err
	}
	for i := range pvcs.Items {
		if isOwnedStatefulSetDataPVC(&pvcs.Items[i], owner, name, !foreign) {
			return false, nil
		}
	}
	return true, nil
}
