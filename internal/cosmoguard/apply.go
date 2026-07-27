package cosmoguard

import (
	"context"
	"fmt"
	"reflect"

	"github.com/banzaicloud/k8s-objectmatcher/patch"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/voluzi/cosmopilot/v2/internal/controllers"
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

// RouteState reports the outcome of applying a dashboard HTTPRoute.
type RouteState int

const (
	// RouteUnavailable means the Gateway API CRDs are not installed, so no route was applied. This
	// is a permanent property of the cluster, not a transient wait — callers must keep the legacy
	// Ingress serving and must NOT poll for a status that will never arrive.
	RouteUnavailable RouteState = iota
	// RoutePending means the route was applied but its parent has not yet reported the current
	// generation as accepted with references resolved. Acceptance arrives as a status update, which
	// no watch admits, so callers re-check shortly.
	RoutePending
	// RouteAccepted means every configured parent accepted the current route generation.
	RouteAccepted
)

// Ready reports whether the route is serving, i.e. accepted by every configured parent.
func (s RouteState) Ready() bool { return s == RouteAccepted }

// ApplyOwnedHTTPRoute creates or updates a dashboard HTTPRoute owned by owner, reporting whether the
// configured parent has accepted the current route generation. Gateway API may be optional in a
// cluster; a missing CRD yields RouteUnavailable rather than RoutePending, so callers can keep the
// legacy Ingress without polling for an acceptance that can never arrive.
func ApplyOwnedHTTPRoute(ctx context.Context, c client.Client, scheme *runtime.Scheme, owner client.Object, route *gwapiv1.HTTPRoute) (RouteState, error) {
	if err := controllerutil.SetControllerReference(owner, route, scheme); err != nil {
		return RouteUnavailable, err
	}

	existing := &gwapiv1.HTTPRoute{}
	err := c.Get(ctx, client.ObjectKeyFromObject(route), existing)
	if err == nil && !metav1.IsControlledBy(existing, owner) {
		return RouteUnavailable, fmt.Errorf("cosmoguard resource %q is managed by another owner; refusing to overwrite it — rename the ChainNode/ChainNodeSet to avoid the name collision", route.GetName())
	}
	if err != nil && !errors.IsNotFound(err) {
		if controllers.IsCRDNotInstalled(err) {
			return RouteUnavailable, nil
		}
		return RouteUnavailable, err
	}

	reconciled, err := controllers.EnsureHTTPRoute(ctx, c, route)
	if err != nil {
		return RouteUnavailable, err
	}
	if !reconciled {
		// EnsureHTTPRoute reports not-reconciled only when the Gateway API CRDs are missing.
		return RouteUnavailable, nil
	}

	current := &gwapiv1.HTTPRoute{}
	if err := c.Get(ctx, client.ObjectKeyFromObject(route), current); err != nil {
		if controllers.IsCRDNotInstalled(err) {
			return RouteUnavailable, nil
		}
		if errors.IsNotFound(err) {
			return RoutePending, nil
		}
		return RouteUnavailable, err
	}
	if httpRouteReady(current) {
		return RouteAccepted, nil
	}
	return RoutePending, nil
}

// RepointDashboardIngressPort updates a retained dashboard Ingress so its Service backend port
// matches the guard's current dashboard port.
//
// During an Ingress-to-Gateway migration the Ingress is deliberately kept serving until the
// replacement HTTPRoute is accepted. But the guard Service and StatefulSet are reconciled to the
// desired spec earlier in the same pass, so if the dashboard PORT changed alongside the migration,
// the Service no longer publishes the old port and the retained Ingress would point at a port that
// does not exist — a fallback that 503s, which is worse than the exposure gap it exists to avoid.
// Rewriting just the backend port keeps it usable until the route takes over.
//
// The Ingress is left untouched when it is absent or owned by someone else.
func RepointDashboardIngressPort(ctx context.Context, c client.Client, owner client.Object, namespace, name string, port int32) error {
	ingress := &networkingv1.Ingress{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, ingress); err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if !metav1.IsControlledBy(ingress, owner) {
		return nil
	}

	changed := false
	for i := range ingress.Spec.Rules {
		http := ingress.Spec.Rules[i].HTTP
		if http == nil {
			continue
		}
		for j := range http.Paths {
			svc := http.Paths[j].Backend.Service
			// A named backend port resolves through the Service, which renames nothing across a port
			// change, so only numeric backends can go stale.
			if svc == nil || svc.Port.Name != "" || svc.Port.Number == port {
				continue
			}
			svc.Port.Number = port
			changed = true
		}
	}
	if !changed {
		return nil
	}

	if err := patch.DefaultAnnotator.SetLastAppliedAnnotation(ingress); err != nil {
		return err
	}
	return c.Update(ctx, ingress)
}

func httpRouteReady(route *gwapiv1.HTTPRoute) bool {
	if len(route.Spec.ParentRefs) == 0 {
		return false
	}
	for _, desired := range route.Spec.ParentRefs {
		ready := false
		for _, parent := range route.Status.Parents {
			if parentReferencesEqual(route.GetNamespace(), desired, parent.ParentRef) && routeParentReady(route.GetGeneration(), parent.Conditions) {
				ready = true
				break
			}
		}
		if !ready {
			return false
		}
	}
	return true
}

func parentReferencesEqual(namespace string, a, b gwapiv1.ParentReference) bool {
	defaultGroup := gwapiv1.Group(gwapiv1.GroupName)
	defaultKind := gwapiv1.Kind("Gateway")
	defaultNamespace := gwapiv1.Namespace(namespace)
	return valueOr(a.Group, defaultGroup) == valueOr(b.Group, defaultGroup) &&
		valueOr(a.Kind, defaultKind) == valueOr(b.Kind, defaultKind) &&
		valueOr(a.Namespace, defaultNamespace) == valueOr(b.Namespace, defaultNamespace) &&
		a.Name == b.Name && optionalValuesEqual(a.SectionName, b.SectionName) && optionalValuesEqual(a.Port, b.Port)
}

func routeParentReady(generation int64, conditions []metav1.Condition) bool {
	accepted := false
	resolved := false
	for _, condition := range conditions {
		if condition.ObservedGeneration != generation || condition.Status != metav1.ConditionTrue {
			continue
		}
		switch condition.Type {
		case string(gwapiv1.RouteConditionAccepted):
			accepted = true
		case string(gwapiv1.RouteConditionResolvedRefs):
			resolved = true
		}
	}
	return accepted && resolved
}

func valueOr[T comparable](value *T, fallback T) T {
	if value == nil {
		return fallback
	}
	return *value
}

func optionalValuesEqual[T comparable](a, b *T) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
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
	if sts.Spec.Replicas != nil {
		// Require every replica updated AND ready before flipping a global route, so traffic isn't
		// routed to a partially-available guard set (e.g. one ready pod out of several).
		return sts.Status.UpdatedReplicas >= *sts.Spec.Replicas && sts.Status.ReadyReplicas >= *sts.Spec.Replicas, nil
	}
	return sts.Status.ReadyReplicas > 0, nil
}
