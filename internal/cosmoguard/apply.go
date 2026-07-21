package cosmoguard

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
)

// ApplyOwned creates or updates obj as a resource owned by owner. It refuses to overwrite a
// resource controlled by a different owner (name collision) and skips no-op writes so steady-state
// reconciles don't churn resourceVersions. The live object is copied back into obj so callers can
// read status (e.g. Deployment ReadyReplicas) after the call.
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
		if err := patch.DefaultAnnotator.SetLastAppliedAnnotation(obj); err != nil {
			return err
		}
		return c.Create(ctx, obj)
	}
	if err != nil {
		return err
	}

	if !metav1.IsControlledBy(existing, owner) {
		return fmt.Errorf("cosmoguard resource %q is managed by another owner; refusing to overwrite it — rename the ChainNode/ChainNodeSet to avoid the name collision", obj.GetName())
	}

	// When autoscaling owns .spec.replicas we submit a nil Replicas. A full Update would reset the
	// live value (the API defaults nil to 1), fighting the HPA on every reconcile — so copy the live
	// replica count forward and let the autoscaler keep control.
	if desired, ok := obj.(*appsv1.StatefulSet); ok && desired.Spec.Replicas == nil {
		if live, ok := existing.(*appsv1.StatefulSet); ok {
			desired.Spec.Replicas = live.Spec.Replicas
		}
	}

	// Cluster IPs are immutable and API-allocated; a full Update that submits the freshly rendered
	// Service (with empty ClusterIP/ClusterIPs) is rejected by the API server. Copy the live
	// allocation forward so Service updates (added ports, changed selector/labels) apply cleanly.
	if desired, ok := obj.(*corev1.Service); ok {
		if live, ok := existing.(*corev1.Service); ok {
			desired.Spec.ClusterIP = live.Spec.ClusterIP
			desired.Spec.ClusterIPs = live.Spec.ClusterIPs
		}
	}

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

// IsServing reports whether the named CosmoGuard StatefulSet has at least one ready replica for its
// observed generation — i.e. it can serve traffic. This intentionally does NOT require every replica
// to be updated: a rolling update (or scale-up) keeps ready replicas serving throughout, so gating
// the Service flip on full rollout would revert an already-guarded Service to raw node pods on every
// routine guard update, briefly bypassing CosmoGuard policy. Ready-replica readiness keeps the flip
// sticky once achieved while still holding off the first flip until the guard is actually serving
// (make-before-break).
func IsServing(ctx context.Context, c client.Client, namespace, name string) (bool, error) {
	sts := &appsv1.StatefulSet{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, sts); err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}

	if sts.Status.ObservedGeneration < sts.Generation {
		return false, nil
	}
	return sts.Status.ReadyReplicas > 0, nil
}

// IsFullyRolledOut reports whether the named CosmoGuard StatefulSet has fully rolled out its current
// generation (every replica updated and at least one ready). Unlike IsServing, this waits for the
// new pod template to be applied — used to gate a GLOBAL route flip, whose Service selector matches a
// per-route pod label that only lands on pods created by the current generation. Flipping such a
// route before the roll completes would select zero (or a stale subset of) guard pods.
func IsFullyRolledOut(ctx context.Context, c client.Client, namespace, name string) (bool, error) {
	sts := &appsv1.StatefulSet{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, sts); err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}

	if sts.Status.ObservedGeneration < sts.Generation {
		return false, nil
	}
	if sts.Spec.Replicas != nil && sts.Status.UpdatedReplicas < *sts.Spec.Replicas {
		return false, nil
	}
	return sts.Status.ReadyReplicas > 0, nil
}
