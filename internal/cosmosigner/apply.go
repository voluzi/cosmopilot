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

// PreflightDeployable reports (as an error) whether the signer named `name` can be deployed by owner,
// running the SAME blocking checks its deployment performs — a name-collision refusal on EVERY object
// the deployment creates/updates (each is applied with ApplyOwned, which refuses to overwrite an object
// owned by a different controller; the one-shot import pod refuses the same way), plus the foreign/
// ambiguous raft-state PVC guard on a fresh StatefulSet — without applying anything. The ChainNodeSet
// controller calls this BEFORE it retargets child validators to the remote signer, so a signer that a
// later apply would refuse (a same-name ConfigMap/Service/StatefulSet/pod owned by another CR, or a
// stale foreign `data-<signer>-<ordinal>` claim) does not leave a validator with neither its local key
// nor a deployable signer. Objects this owner already controls remain deployable only when their
// immutable shape is compatible with the signer resource that will reuse the name.
//
// usesImportPod must be true only when the signer actually runs the one-shot `<name>-import` pod (a
// Vault uploadGenerated signer). Software, GCP KMS and pre-provisioned Vault signers never create it, so
// checking that name for them would let an unrelated foreign pod block an otherwise-deployable signer on
// every reconcile.
// replicas is the desired StatefulSet replica count; fresh deployment also reserves each deterministic
// `<name>-<ordinal>` pod name before validators are retargeted.
func PreflightDeployable(ctx context.Context, c client.Client, owner client.Object, namespace, name string, replicas int32, usesImportPod bool) error {
	// Objects that need only the same-owner check. Services are handled separately because their
	// headless shape is immutable and must also match before deployment.
	named := []struct {
		kind string
		obj  client.Object
	}{
		{"ConfigMap", &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}},
	}
	if usesImportPod {
		named = append(named, struct {
			kind string
			obj  client.Object
		}{"import pod", &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name + "-" + importJobSuffix}}})
	}
	for _, n := range named {
		if err := ensureNoForeignObject(ctx, c, owner, n.kind, n.obj); err != nil {
			return err
		}
	}
	if err := ensureHeadlessServiceDeployable(ctx, c, owner, namespace, "raft Service", name); err != nil {
		return err
	}
	if err := ensureHeadlessServiceDeployable(ctx, c, owner, namespace, "discovery Service", name+discoveryServiceSuffix); err != nil {
		return err
	}

	sts := &appsv1.StatefulSet{}
	switch err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, sts); {
	case err == nil:
		if !metav1.IsControlledBy(sts, owner) {
			return foreignObjectErr("StatefulSet", name)
		}
		if err := ensureReplicaPodNamesAvailable(ctx, c, namespace, name, replicas, sts); err != nil {
			return err
		}
		return ensureNoForeignDataPVCs(ctx, c, owner, namespace, name)
	case errors.IsNotFound(err):
		// A fresh StatefulSet cannot create a replica while any pod already holds its deterministic
		// <name>-<ordinal> name, regardless of that pod's owner.
		if err := ensureReplicaPodNamesAvailable(ctx, c, namespace, name, replicas, nil); err != nil {
			return err
		}
		// ApplyOwned's Create path runs the PVC guard below.
		return ensureNoForeignDataPVCs(ctx, c, owner, namespace, name)
	default:
		return err
	}
}

func ensureHeadlessServiceDeployable(ctx context.Context, c client.Client, owner client.Object, namespace, kind, name string) error {
	svc := &corev1.Service{}
	switch err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, svc); {
	case errors.IsNotFound(err):
		return nil
	case err != nil:
		return err
	case !metav1.IsControlledBy(svc, owner):
		return foreignObjectErr(kind, name)
	case svc.Spec.ClusterIP != corev1.ClusterIPNone:
		return fmt.Errorf("cosmosigner %s %q is not headless and cannot be converted in place; delete the stale owned Service before deploying the signer", kind, name)
	default:
		return nil
	}
}

func ensureReplicaPodNamesAvailable(ctx context.Context, c client.Client, namespace, name string, replicas int32, sts *appsv1.StatefulSet) error {
	for ordinal := int32(0); ordinal < replicas; ordinal++ {
		podName := fmt.Sprintf("%s-%d", name, ordinal)
		pod := &corev1.Pod{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: podName}, pod); errors.IsNotFound(err) {
			continue
		} else if err != nil {
			return err
		}
		if sts == nil || !metav1.IsControlledBy(pod, sts) {
			return fmt.Errorf("cosmosigner replica pod %q already exists; refusing to create or scale a StatefulSet that cannot start all replicas", podName)
		}
	}
	return nil
}

// ensureNoForeignObject errors when obj exists and is controlled by a different owner (a same-name
// collision an apply would refuse). A missing object is fine.
func ensureNoForeignObject(ctx context.Context, c client.Client, owner client.Object, kind string, obj client.Object) error {
	switch err := c.Get(ctx, client.ObjectKeyFromObject(obj), obj); {
	case errors.IsNotFound(err):
		return nil
	case err != nil:
		return err
	case !metav1.IsControlledBy(obj, owner):
		return foreignObjectErr(kind, obj.GetName())
	default:
		return nil
	}
}

func foreignObjectErr(kind, name string) error {
	return fmt.Errorf("cosmosigner %s %q is managed by another owner; refusing to deploy over it — rename the ChainNode/ChainNodeSet to avoid the name collision", kind, name)
}

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
		// A FRESH signer StatefulSet must never bind raft-state PVCs left behind by another owner —
		// e.g. a CR deleted and recreated under the same name (new UID), whose StatefulSet was
		// garbage-collected but whose per-pod claims were not. In this branch no same-name StatefulSet
		// exists at all, so any exact-match data claim not attributable to THIS owner is an orphan
		// carrying unknown raft membership/state; refuse to deploy until the operator deletes or
		// relabels it. (Claims owned by this CR are fine: re-binding its own state is the normal
		// restart path, guarded by the replica/storage locks.)
		if sts, isSts := obj.(*appsv1.StatefulSet); isSts {
			if err := ensureNoForeignDataPVCs(ctx, c, owner, sts.GetNamespace(), sts.GetName()); err != nil {
				return err
			}
		}
		if err := patch.DefaultAnnotator.SetLastAppliedAnnotation(obj); err != nil {
			return err
		}
		return c.Create(ctx, obj)
	}
	if err != nil {
		return err
	}
	if !metav1.IsControlledBy(existing, owner) {
		return fmt.Errorf("cosmosigner resource %q is managed by another owner; refusing to overwrite it — rename the ChainNode/ChainNodeSet to avoid the name collision", obj.GetName())
	}
	if sts, isSts := obj.(*appsv1.StatefulSet); isSts {
		if err := ensureNoForeignDataPVCs(ctx, c, owner, sts.GetNamespace(), sts.GetName()); err != nil {
			return err
		}
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

	if err := patch.DefaultAnnotator.SetLastAppliedAnnotation(obj); err != nil {
		return err
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
	// Already requested zero: quiesced only once the StatefulSet controller has observed the
	// scale-down generation and reports no replicas left. A stale zero-valued status from before the
	// controller observed this generation must not let an import proceed while old pods can still be
	// created or terminating.
	return sts.Status.ObservedGeneration >= sts.Generation && sts.Status.Replicas == 0, nil
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
		&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name + "-" + importJobSuffix, Namespace: namespace}},
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
	importPod := &corev1.Pod{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name + "-" + importJobSuffix}, importPod); err == nil {
		if metav1.IsControlledBy(importPod, owner) {
			return false, nil
		}
	} else if !errors.IsNotFound(err) {
		return false, err
	}

	sts := &appsv1.StatefulSet{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, sts); err == nil {
		// A same-name StatefulSet exists. Only OUR StatefulSet blocks teardown completion; a foreign
		// one falls through to the owner-scoped PVC check below.
		if metav1.IsControlledBy(sts, owner) {
			return false, nil
		}
	} else if !errors.IsNotFound(err) {
		return false, err
	}

	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := c.List(ctx, pvcs, client.InNamespace(namespace)); err != nil {
		return false, err
	}
	for i := range pvcs.Items {
		// Our own claims block until deleted. AMBIGUOUS legacy claims (no owner-UID label — cannot be
		// attributed to any owner without a race) also block: treating them as gone would let a
		// recreated signer bind stale raft state with unknown membership. They are never deleted
		// automatically; the operator resolves them by deleting or labeling the claim.
		if isOwnedStatefulSetDataPVC(&pvcs.Items[i], owner, name) || isAmbiguousLegacyDataPVC(&pvcs.Items[i], name) {
			return false, nil
		}
	}
	return true, nil
}
