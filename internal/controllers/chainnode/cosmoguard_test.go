package chainnode

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	k8sappsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
)

// ensureGuard drops ensureCosmoGuard's route-pending flag for the tests that only assert on the
// error. Tests that care about the flag call ensureCosmoGuard directly.
func ensureGuard(r *Reconciler, ctx context.Context, cn *appsv1.ChainNode) error {
	_, err := r.ensureCosmoGuard(ctx, cn)
	return err
}

func cosmoGuardTestReconciler(t *testing.T, objs ...client.Object) *Reconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, k8sappsv1.AddToScheme(scheme))
	require.NoError(t, autoscalingv2.AddToScheme(scheme))
	require.NoError(t, networkingv1.AddToScheme(scheme))
	require.NoError(t, policyv1.AddToScheme(scheme))
	require.NoError(t, gwapiv1.Install(scheme))

	return &Reconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build(),
		Scheme: scheme,
		opts:   &controllers.ControllerRunOptions{CosmoGuardImage: "ghcr.io/voluzi/cosmoguard:4.0.2"},
	}
}

type gatewayUnavailableClient struct {
	client.Client
}

func (c gatewayUnavailableClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*gwapiv1.HTTPRoute); ok {
		return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: gwapiv1.GroupVersion.Group, Kind: "HTTPRoute"}}
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func dashboardGatewayConfig(redirect bool) *appsv1.CosmoGuardDashboardConfig {
	httpsSection := "https-dashboard"
	httpSection := "http"
	dashboard := &appsv1.CosmoGuardDashboardConfig{
		Enable: true,
		Gateway: &appsv1.CosmoGuardDashboardGateway{
			Host: "guard.example.com",
			Gateway: appsv1.GatewayRef{
				Name:        "external",
				SectionName: &httpsSection,
			},
		},
	}
	if redirect {
		dashboard.Gateway.HTTPRedirect = &appsv1.GatewayRef{Name: "external", SectionName: &httpSection}
	}
	return dashboard
}

// markChainNodeSetChild makes a ChainNode look like a generated ChainNodeSet member: it stamps the
// controller owner reference IsControlledByChainNodeSet() checks, plus the "nodeset" label a real
// child also carries. Child detection keys off the owner reference, not the (user-settable) label.
func markChainNodeSetChild(cn *appsv1.ChainNode) {
	cn.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: appsv1.GroupVersion.String(),
		Kind:       "ChainNodeSet",
		Name:       "some-set",
		UID:        types.UID("some-set-uid"),
		Controller: ptr.To(true),
	}}
	if cn.Labels == nil {
		cn.Labels = map[string]string{}
	}
	cn.Labels[controllers.LabelChainNodeSet] = "some-set"
}

func guardedChainNode(name string, child bool) *appsv1.ChainNode {
	cn := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: map[string]string{}, UID: types.UID(name + "-uid")},
		Spec: appsv1.ChainNodeSpec{
			Config: &appsv1.Config{
				CosmoGuard: &appsv1.CosmoGuardConfig{
					Enable: true,
					Config: &corev1.ConfigMapKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "rules"},
						Key:                  "cosmoguard.yaml",
					},
				},
			},
		},
	}
	if child {
		markChainNodeSetChild(cn)
	}
	return cn
}

// TestStandaloneGuardCreatesStatefulSetAndService verifies a standalone ChainNode gets a clustered
// guard StatefulSet + client Service + headless peer Service + encryption Secret, pointed at its
// internal Service (static upstream).
func TestStandaloneGuardCreatesStatefulSetAndService(t *testing.T) {
	cn := guardedChainNode("node-0", false)
	r := cosmoGuardTestReconciler(t, cn)

	require.NoError(t, ensureGuard(r, context.Background(), cn))

	sts := &k8sappsv1.StatefulSet{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "node-0-cg"}, sts))

	env := sts.Spec.Template.Spec.Containers[0].Env
	found := false
	for _, e := range env {
		if e.Name == "COSMOGUARD_NODE_HOST" {
			found = true
			// Upstream is the ready-gated main node Service, not the not-ready-publishing "-internal".
			assert.Equal(t, "node-0.ns.svc.cluster.local", e.Value)
		}
	}
	assert.True(t, found, "static upstream host must be injected")

	svc := &corev1.Service{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "node-0-cg"}, svc))

	// Headless peer Service + encryption Secret provisioned for the olric cluster.
	peer := &corev1.Service{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "node-0-cg-peer"}, peer))
	assert.Equal(t, corev1.ClusterIPNone, peer.Spec.ClusterIP)

	secret := &corev1.Secret{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "node-0-cg-cluster"}, secret))
	assert.NotEmpty(t, secret.Data["encryptionKey"])
}

func TestStandaloneGuardCreatesDashboardHTTPRoutes(t *testing.T) {
	cn := guardedChainNode("node-0", false)
	cn.Spec.Config.CosmoGuard.Dashboard = dashboardGatewayConfig(true)
	r := cosmoGuardTestReconciler(t, cn)

	require.NoError(t, ensureGuard(r, context.Background(), cn))

	backend := &gwapiv1.HTTPRoute{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "node-0-cg-dashboard"}, backend))
	require.Len(t, backend.Spec.Rules, 1)
	require.Len(t, backend.Spec.Rules[0].BackendRefs, 1)
	assert.Equal(t, "node-0-cg", string(backend.Spec.Rules[0].BackendRefs[0].Name))
	assert.True(t, metav1.IsControlledBy(backend, cn))

	redirect := &gwapiv1.HTTPRoute{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "node-0-cg-dashboard-http-redirect"}, redirect))
	assert.True(t, metav1.IsControlledBy(redirect, cn))
}

func TestStandaloneDashboardGatewayPreservesIngressWhenGatewayAPIUnavailable(t *testing.T) {
	ctx := context.Background()
	cn := guardedChainNode("node-0", false)
	cn.Spec.Config.CosmoGuard.Dashboard = dashboardGatewayConfig(false)
	r := cosmoGuardTestReconciler(t, cn)

	ingress := guardIngress("node-0-cg-dashboard", "node-0-cg")
	require.NoError(t, controllerutil.SetControllerReference(cn, ingress, r.Scheme))
	require.NoError(t, r.Create(ctx, ingress))
	r.Client = gatewayUnavailableClient{Client: r.Client}

	pending, err := r.ensureCosmoGuard(ctx, cn)
	require.NoError(t, err)
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(ingress), &networkingv1.Ingress{}))
	// Missing CRDs are permanent, not a status we are waiting on: requeueing every few seconds would
	// spin forever without ever converging, so such a cluster keeps the normal reconcile cadence.
	assert.False(t, pending, "an unavailable Gateway API must not trigger the short retry period")
}

func TestStandaloneDashboardGatewayWaitsForAcceptedRouteBeforeDeletingIngress(t *testing.T) {
	ctx := context.Background()
	cn := guardedChainNode("node-0", false)
	cn.Spec.Config.CosmoGuard.Dashboard = dashboardGatewayConfig(false)
	r := cosmoGuardTestReconciler(t, cn)

	ingress := guardIngress("node-0-cg-dashboard", "node-0-cg")
	require.NoError(t, controllerutil.SetControllerReference(cn, ingress, r.Scheme))
	require.NoError(t, r.Create(ctx, ingress))

	require.NoError(t, ensureGuard(r, ctx, cn))
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(ingress), &networkingv1.Ingress{}))

	route := &gwapiv1.HTTPRoute{}
	require.NoError(t, r.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "node-0-cg-dashboard"}, route))
	route.Status.Parents = []gwapiv1.RouteParentStatus{{
		ParentRef:      route.Spec.ParentRefs[0],
		ControllerName: "example.net/gateway-controller",
		Conditions: []metav1.Condition{
			{Type: string(gwapiv1.RouteConditionAccepted), Status: metav1.ConditionTrue, ObservedGeneration: route.Generation},
			{Type: string(gwapiv1.RouteConditionResolvedRefs), Status: metav1.ConditionTrue, ObservedGeneration: route.Generation},
		},
	}}
	require.NoError(t, r.Update(ctx, route))

	require.NoError(t, ensureGuard(r, ctx, cn))
	err := r.Get(ctx, client.ObjectKeyFromObject(ingress), &networkingv1.Ingress{})
	assert.True(t, apierrors.IsNotFound(err))
}

// TestRetainedDashboardIngressFollowsPortChange verifies that when an Ingress-to-Gateway migration
// also changes the dashboard port, the Ingress kept alive while the route is pending is repointed at
// the new port. The guard Service is reconciled to the new port in the same pass, so leaving the
// Ingress on the old numeric port would make the fallback 503 — worse than the gap it prevents.
func TestRetainedDashboardIngressFollowsPortChange(t *testing.T) {
	ctx := context.Background()
	cn := guardedChainNode("node-0", false)
	cn.Spec.Config.CosmoGuard.Dashboard = dashboardGatewayConfig(false)
	cn.Spec.Config.CosmoGuard.Dashboard.Port = ptr.To[int32](9100)
	r := cosmoGuardTestReconciler(t, cn)

	// A live dashboard Ingress from the pre-migration Ingress mode, on the OLD port.
	ingress := guardIngress("node-0-cg-dashboard", "node-0-cg")
	ingress.Spec.Rules[0].HTTP.Paths[0].Backend.Service.Port = networkingv1.ServiceBackendPort{Number: 8080}
	require.NoError(t, controllerutil.SetControllerReference(cn, ingress, r.Scheme))
	require.NoError(t, r.Create(ctx, ingress))

	pending, err := r.ensureCosmoGuard(ctx, cn)
	require.NoError(t, err)
	require.True(t, pending, "route is not accepted yet, so the Ingress is retained")

	live := &networkingv1.Ingress{}
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(ingress), live))
	assert.Equal(t, int32(9100), live.Spec.Rules[0].HTTP.Paths[0].Backend.Service.Port.Number,
		"retained fallback Ingress must follow the guard Service's current dashboard port")

	// The guard Service publishes that port, so the repointed Ingress resolves.
	svc := &corev1.Service{}
	require.NoError(t, r.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "node-0-cg"}, svc))
	found := false
	for _, p := range svc.Spec.Ports {
		if p.Port == 9100 {
			found = true
		}
	}
	assert.True(t, found, "guard Service exposes the new dashboard port")
}

// TestStandaloneDashboardReportsPendingRoutesForRequeue verifies ensureCosmoGuard reports an
// unaccepted dashboard route so Reconcile re-checks soon. Route acceptance arrives as an HTTPRoute
// STATUS update, which no watch here admits, so without this signal the superseded Ingress would
// stay live for a full reconcile period after the Gateway accepts the route.
func TestStandaloneDashboardReportsPendingRoutesForRequeue(t *testing.T) {
	ctx := context.Background()
	cn := guardedChainNode("node-0", false)
	cn.Spec.Config.CosmoGuard.Dashboard = dashboardGatewayConfig(false)
	r := cosmoGuardTestReconciler(t, cn)

	pending, err := r.ensureCosmoGuard(ctx, cn)
	require.NoError(t, err)
	assert.True(t, pending, "an unaccepted route must request a prompt re-check")

	route := &gwapiv1.HTTPRoute{}
	require.NoError(t, r.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "node-0-cg-dashboard"}, route))
	route.Status.Parents = []gwapiv1.RouteParentStatus{{
		ParentRef:      route.Spec.ParentRefs[0],
		ControllerName: "example.net/gateway-controller",
		Conditions: []metav1.Condition{
			{Type: string(gwapiv1.RouteConditionAccepted), Status: metav1.ConditionTrue, ObservedGeneration: route.Generation},
			{Type: string(gwapiv1.RouteConditionResolvedRefs), Status: metav1.ConditionTrue, ObservedGeneration: route.Generation},
		},
	}}
	require.NoError(t, r.Update(ctx, route))

	pending, err = r.ensureCosmoGuard(ctx, cn)
	require.NoError(t, err)
	assert.False(t, pending, "an accepted route falls back to the normal reconcile period")
}

func TestStandaloneDashboardSwitchToIngressRemovesHTTPRoutes(t *testing.T) {
	ctx := context.Background()
	cn := guardedChainNode("node-0", false)
	cn.Spec.Config.CosmoGuard.Dashboard = dashboardGatewayConfig(true)
	r := cosmoGuardTestReconciler(t, cn)
	require.NoError(t, ensureGuard(r, ctx, cn))

	cn.Spec.Config.CosmoGuard.Dashboard = &appsv1.CosmoGuardDashboardConfig{
		Enable:  true,
		Ingress: &appsv1.CosmoGuardDashboardIngress{Host: "guard.example.com"},
	}
	require.NoError(t, ensureGuard(r, ctx, cn))

	require.NoError(t, r.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "node-0-cg-dashboard"}, &networkingv1.Ingress{}))
	for _, name := range []string{"node-0-cg-dashboard", "node-0-cg-dashboard-http-redirect"} {
		err := r.Get(ctx, client.ObjectKey{Namespace: "ns", Name: name}, &gwapiv1.HTTPRoute{})
		assert.True(t, apierrors.IsNotFound(err), "HTTPRoute %s should be removed", name)
	}
}

func TestStandaloneDashboardDisablingGatewayRemovesHTTPRoutes(t *testing.T) {
	ctx := context.Background()
	cn := guardedChainNode("node-0", false)
	cn.Spec.Config.CosmoGuard.Dashboard = dashboardGatewayConfig(true)
	r := cosmoGuardTestReconciler(t, cn)
	require.NoError(t, ensureGuard(r, ctx, cn))

	cn.Spec.Config.CosmoGuard.Dashboard.Enable = false
	require.NoError(t, ensureGuard(r, ctx, cn))

	for _, name := range []string{"node-0-cg-dashboard", "node-0-cg-dashboard-http-redirect"} {
		err := r.Get(ctx, client.ObjectKey{Namespace: "ns", Name: name}, &gwapiv1.HTTPRoute{})
		assert.True(t, apierrors.IsNotFound(err), "HTTPRoute %s should be removed", name)
	}
}

func TestStandaloneDashboardRejectsForeignHTTPRoute(t *testing.T) {
	ctx := context.Background()
	cn := guardedChainNode("node-0", false)
	cn.Spec.Config.CosmoGuard.Dashboard = dashboardGatewayConfig(false)
	foreign := guardedChainNode("other", false)
	r := cosmoGuardTestReconciler(t, cn, foreign)

	route := &gwapiv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{Name: "node-0-cg-dashboard", Namespace: "ns"}}
	require.NoError(t, controllerutil.SetControllerReference(foreign, route, r.Scheme))
	require.NoError(t, r.Create(ctx, route))

	err := ensureGuard(r, ctx, cn)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "managed by another owner")
}

// TestStandaloneGuardInheritsServiceAccount verifies the standalone guard runs under the node's
// configured ServiceAccount (the in-pod sidecar inherited it; the standalone guard must carry it so
// SA-bound pull secrets / workload identity still apply).
func TestStandaloneGuardInheritsServiceAccount(t *testing.T) {
	cn := guardedChainNode("node-0", false)
	cn.Spec.Config.ServiceAccountName = ptr.To("node-sa")
	r := cosmoGuardTestReconciler(t, cn)

	require.NoError(t, ensureGuard(r, context.Background(), cn))

	sts := &k8sappsv1.StatefulSet{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "node-0-cg"}, sts))
	assert.Equal(t, "node-sa", sts.Spec.Template.Spec.ServiceAccountName)
}

// TestStandaloneGuardInheritsUserLabels verifies the guard pods carry the node's genuine user labels
// (so NetworkPolicies / monitoring cover them) but not cosmopilot-managed selector labels.
func TestStandaloneGuardInheritsUserLabels(t *testing.T) {
	cn := guardedChainNode("node-0", false)
	cn.Labels["team"] = "payments"             // user label -> propagated
	cn.Labels[controllers.LabelChainID] = "c1" // managed selector -> stripped
	r := cosmoGuardTestReconciler(t, cn)

	require.NoError(t, ensureGuard(r, context.Background(), cn))

	sts := &k8sappsv1.StatefulSet{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "node-0-cg"}, sts))
	labels := sts.Spec.Template.Labels
	assert.Equal(t, "payments", labels["team"], "user label propagated to guard pods")
	assert.NotContains(t, labels, controllers.LabelChainID, "managed selector label must not reach guard pods")
}

// servingGuard returns a guard StatefulSet reporting a ready replica (so IsServing is true).
func servingGuard(name string) *k8sappsv1.StatefulSet {
	return &k8sappsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Generation: 1},
		Spec:       k8sappsv1.StatefulSetSpec{Replicas: ptr.To[int32](1)},
		Status:     k8sappsv1.StatefulSetStatus{ObservedGeneration: 1, ReadyReplicas: 1, UpdatedReplicas: 1},
	}
}

// TestAPIServiceName verifies ingress/gateway route targets, readiness-gated: a guarded node targets
// its own guard Service only once the guard is serving; otherwise (not serving, no individual guard,
// or disabled) it targets the raw node Service.
func TestAPIServiceName(t *testing.T) {
	ctx := context.Background()
	standalone := guardedChainNode("node-0", false)

	// Guarded but guard not yet serving -> raw (make-before-break).
	assert.Equal(t, "node-0", cosmoGuardTestReconciler(t).apiServiceName(ctx, standalone))
	// Guarded and serving -> guard.
	assert.Equal(t, "node-0-cg", cosmoGuardTestReconciler(t, servingGuard("node-0-cg")).apiServiceName(ctx, standalone))

	// Child with an individual ingress + serving guard -> its own guard.
	child := guardedChainNode("chain-fullnodes-0", true)
	child.Spec.Ingress = &appsv1.IngressConfig{Host: "0.rpc.example.com"}
	assert.Equal(t, "chain-fullnodes-0-cg", cosmoGuardTestReconciler(t, servingGuard("chain-fullnodes-0-cg")).apiServiceName(ctx, child))

	// Child without an individual ingress -> raw (fronted by the group guard).
	childNoIngress := guardedChainNode("chain-fullnodes-1", true)
	assert.Equal(t, "chain-fullnodes-1", cosmoGuardTestReconciler(t).apiServiceName(ctx, childNoIngress))

	// Disabled -> raw.
	unguarded := guardedChainNode("node-1", false)
	unguarded.Spec.Config.CosmoGuard.Enable = false
	assert.Equal(t, "node-1", cosmoGuardTestReconciler(t).apiServiceName(ctx, unguarded))
}

// TestStandaloneStickyFlipViaGrpcIngress verifies the sticky check inspects the separate "<node>-grpc"
// Ingress: for a gRPC-only exposure the base "<node>" Ingress carries no guard backend, so a guard
// that is momentarily not-serving during a rollout must still be recognized via the gRPC Ingress and
// keep its routes (rather than falling back to the raw node and bypassing CosmoGuard for gRPC).
func TestStandaloneStickyFlipViaGrpcIngress(t *testing.T) {
	ctx := context.Background()
	cn := guardedChainNode("node-0", false)
	cn.Spec.Ingress = &appsv1.IngressConfig{Host: "example.com", EnableGRPC: true}

	grpcIng := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: "node-0-grpc", Namespace: "ns"},
		Spec: networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{{
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{{
							Backend: networkingv1.IngressBackend{
								Service: &networkingv1.IngressServiceBackend{Name: "node-0-cg"},
							},
						}},
					},
				},
			}},
		},
	}

	// Guard not serving, but the gRPC Ingress already targets it -> sticky keeps routes on the guard.
	r := cosmoGuardTestReconciler(t, grpcIng)
	assert.Equal(t, "node-0-cg", r.apiServiceName(ctx, cn))
}

// guardIngress builds an Ingress named `name` whose single HTTP path points at `backend`.
func guardIngress(name, backend string) *networkingv1.Ingress {
	return &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec: networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{{
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{{
							Backend: networkingv1.IngressBackend{
								Service: &networkingv1.IngressServiceBackend{Name: backend},
							},
						}},
					},
				},
			}},
		},
	}
}

// TestDashboardRouteDoesNotTriggerStickyAPIFlip verifies the guard's own dashboard HTTPRoute is not
// mistaken for a flipped API route. The dashboard route also backs onto the guard Service (on the
// dashboard port) and is applied unconditionally rather than gated on guard readiness, so counting it
// would retarget live RPC/LCD/gRPC traffic to a guard with no ready endpoints.
func TestDashboardRouteDoesNotTriggerStickyAPIFlip(t *testing.T) {
	ctx := context.Background()
	cn := guardedChainNode("node-0", false)
	cn.Spec.Gateway = &appsv1.GatewayConfig{Host: "rpc.example.com"}
	cn.Spec.Config.CosmoGuard.Dashboard = dashboardGatewayConfig(true)
	r := cosmoGuardTestReconciler(t, cn)

	// Applies the dashboard routes; the guard StatefulSet has no ready replicas in the fake client.
	_, err := r.ensureCosmoGuard(ctx, cn)
	require.NoError(t, err)
	require.NoError(t, r.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "node-0-cg-dashboard"}, &gwapiv1.HTTPRoute{}))

	assert.False(t, r.standaloneRouteTargetsGuard(ctx, cn),
		"a dashboard route must not count as evidence the API routes were flipped")
	assert.Equal(t, "node-0", r.apiServiceName(ctx, cn),
		"API routes must stay on the raw node Service until the guard is serving")

	// A genuine API route onto the guard does keep the flip sticky.
	apiRoute := &gwapiv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{Name: "node-0-rpc", Namespace: "ns"},
		Spec: gwapiv1.HTTPRouteSpec{
			Rules: []gwapiv1.HTTPRouteRule{{
				BackendRefs: []gwapiv1.HTTPBackendRef{{
					BackendRef: gwapiv1.BackendRef{BackendObjectReference: gwapiv1.BackendObjectReference{
						Name: gwapiv1.ObjectName("node-0-cg"),
					}},
				}},
			}},
		},
	}
	require.NoError(t, controllerutil.SetControllerReference(cn, apiRoute, r.Scheme))
	require.NoError(t, r.Create(ctx, apiRoute))
	assert.True(t, r.standaloneRouteTargetsGuard(ctx, cn), "a real API route still keeps the flip sticky")
}

// TestStandaloneRouteTargetsGuardChecksBothTypes verifies the sticky check inspects the old route type
// during an Ingress<->Gateway migration: a node whose Spec now points at Gateway but whose live guarded
// backend is still on the old Ingress is recognized as targeting the guard (so the flip stays sticky and
// the new routes aren't created on the raw Service before the old guarded ones are torn down).
func TestStandaloneRouteTargetsGuardChecksBothTypes(t *testing.T) {
	cn := guardedChainNode("node-0", false)
	cn.Spec.Gateway = &appsv1.GatewayConfig{Host: "rpc.example.com"} // migrated to Gateway
	ing := guardIngress("node-0", "node-0-cg")                       // old guarded Ingress still live

	r := cosmoGuardTestReconciler(t, ing)
	assert.True(t, r.standaloneRouteTargetsGuard(context.Background(), cn),
		"old guarded Ingress must keep the flip sticky even though Spec points at Gateway")
}

// TestFinalizeDefersUndeployWhileRouteTargetsGuard verifies the guard is not torn down while a live
// route still points at it (e.g. a Gateway migration whose routes could not be applied because the CRDs
// are missing, leaving the old guarded Ingress as a fallback), and is torn down once it no longer is.
func TestFinalizeDefersUndeployWhileRouteTargetsGuard(t *testing.T) {
	ctx := context.Background()
	cn := guardedChainNode("node-0", false)
	r := cosmoGuardTestReconciler(t, cn)
	require.NoError(t, ensureGuard(r, ctx, cn))

	// Disable CosmoGuard, but a live Ingress still references the guard Service.
	cn.Spec.Config.CosmoGuard.Enable = false
	require.NoError(t, r.Create(ctx, guardIngress("node-0", "node-0-cg")))

	// Finalize must NOT delete the guard while that Ingress points at it.
	require.NoError(t, r.finalizeCosmoGuard(ctx, cn, true))
	require.NoError(t, r.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "node-0-cg"}, &k8sappsv1.StatefulSet{}),
		"guard must survive while a live route still targets it")

	// Once the route no longer targets the guard, finalize tears it down.
	require.NoError(t, r.Delete(ctx, guardIngress("node-0", "node-0-cg")))
	require.NoError(t, r.finalizeCosmoGuard(ctx, cn, true))
	err := r.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "node-0-cg"}, &k8sappsv1.StatefulSet{})
	assert.Error(t, err, "guard torn down once no route references it")
}

// TestFinalizeStoppedPathTearsDownDespiteRoute verifies the stopped-node path (deferWhileRouted=false)
// tears the guard down even while a stale route still points at it — route reconciliation is skipped
// for stopped nodes, so a route deferral would never clear and the guard would leak.
func TestFinalizeStoppedPathTearsDownDespiteRoute(t *testing.T) {
	ctx := context.Background()
	cn := guardedChainNode("node-0", false)
	r := cosmoGuardTestReconciler(t, cn)
	require.NoError(t, ensureGuard(r, ctx, cn))

	cn.Spec.Config.CosmoGuard.Enable = false
	require.NoError(t, r.Create(ctx, guardIngress("node-0", "node-0-cg")))

	// Stopped path: defer disabled -> tears down despite the stale route.
	require.NoError(t, r.finalizeCosmoGuard(ctx, cn, false))
	err := r.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "node-0-cg"}, &k8sappsv1.StatefulSet{})
	assert.Error(t, err, "stopped-path finalize tears down the guard regardless of stale routes")
}

// TestDisableAutoscalingRemovesHPA verifies the standalone guard deletes its HPA when autoscaling is
// turned off, so it stops driving the StatefulSet's replica count.
func TestDisableAutoscalingRemovesHPA(t *testing.T) {
	cn := guardedChainNode("node-0", false)
	cn.Spec.Config.CosmoGuard.Autoscaling = &appsv1.CosmoGuardAutoscalingConfig{Enable: true, MaxReplicas: 5}
	r := cosmoGuardTestReconciler(t, cn)

	require.NoError(t, ensureGuard(r, context.Background(), cn))
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "node-0-cg"}, &autoscalingv2.HorizontalPodAutoscaler{}))

	cn.Spec.Config.CosmoGuard.Autoscaling.Enable = false
	require.NoError(t, ensureGuard(r, context.Background(), cn))
	err := r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "node-0-cg"}, &autoscalingv2.HorizontalPodAutoscaler{})
	assert.Error(t, err, "HPA should be removed when autoscaling is disabled")
}

// TestFinalizeTearsDownGuardWhenNodeBecomesChild verifies that moving a standalone guarded node into
// a ChainNodeSet removes its now-orphaned per-node guard on the next finalize.
func TestFinalizeTearsDownGuardWhenNodeBecomesChild(t *testing.T) {
	cn := guardedChainNode("node-0", false)
	r := cosmoGuardTestReconciler(t, cn)
	require.NoError(t, ensureGuard(r, context.Background(), cn))
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "node-0-cg"}, &k8sappsv1.StatefulSet{}))

	// The node joins a ChainNodeSet; ensure no longer manages a guard and finalize tears the old one down.
	markChainNodeSetChild(cn)
	require.NoError(t, ensureGuard(r, context.Background(), cn))
	require.NoError(t, r.finalizeCosmoGuard(context.Background(), cn, true))
	err := r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "node-0-cg"}, &k8sappsv1.StatefulSet{})
	assert.Error(t, err, "standalone guard should be removed once the node is a ChainNodeSet child")
}

// TestChildWithIndividualIngressGetsGuard verifies a ChainNodeSet child that declares its own
// individual ingress gets a per-node guard, and its API routes target that guard — preserving the
// old sidecar behavior where individually-exposed nodes were guarded.
func TestChildWithIndividualIngressGetsGuard(t *testing.T) {
	cn := guardedChainNode("chain-fullnodes-0", true)
	cn.Spec.Ingress = &appsv1.IngressConfig{Host: "0.rpc.example.com"}
	r := cosmoGuardTestReconciler(t, cn)

	// The child manages its own guard (created here) even though it's a ChainNodeSet member.
	require.NoError(t, ensureGuard(r, context.Background(), cn))
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "chain-fullnodes-0-cg"}, &k8sappsv1.StatefulSet{}))

	// Removing the individual ingress tears the per-node guard back down.
	cn.Spec.Ingress = nil
	require.NoError(t, ensureGuard(r, context.Background(), cn))
	require.NoError(t, r.finalizeCosmoGuard(context.Background(), cn, true))
	assert.Equal(t, "chain-fullnodes-0", r.apiServiceName(context.Background(), cn))
	err := r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "chain-fullnodes-0-cg"}, &k8sappsv1.StatefulSet{})
	assert.Error(t, err, "per-node guard should be removed once the individual ingress is gone")
}

// TestNodeSetChildSkipsStandaloneGuard verifies a ChainNodeSet child never creates its own guard
// (the group's guard, managed by the set, fronts it).
func TestNodeSetChildSkipsStandaloneGuard(t *testing.T) {
	cn := guardedChainNode("chain-fullnodes-0", true)
	r := cosmoGuardTestReconciler(t, cn)

	require.NoError(t, ensureGuard(r, context.Background(), cn))

	dep := &k8sappsv1.Deployment{}
	err := r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "chain-fullnodes-0-cg"}, dep)
	assert.Error(t, err, "no standalone guard should be created for a nodeset child")
}

// TestStandaloneGuardManagedUsesOwnerRef verifies child detection keys off the ChainNodeSet
// controller owner reference, not the user-settable "nodeset" label. Regression for S4_0W: a
// standalone node carrying a stray label must still get its own guard.
func TestStandaloneGuardManagedUsesOwnerRef(t *testing.T) {
	r := &Reconciler{}

	// Guard disabled -> never managed.
	off := guardedChainNode("node-0", false)
	off.Spec.Config.CosmoGuard.Enable = false
	assert.False(t, r.standaloneGuardManaged(off))

	// Standalone node -> managed.
	assert.True(t, r.standaloneGuardManaged(guardedChainNode("node-0", false)))

	// A stray "nodeset" label but no owner reference must not suppress the guard (the S4_0W bug).
	strayLabel := guardedChainNode("node-0", false)
	strayLabel.Labels[controllers.LabelChainNodeSet] = "some-set"
	assert.True(t, r.standaloneGuardManaged(strayLabel), "stray nodeset label must not skip the guard")

	// A real child (owner ref) without individual routes -> not managed (the group guard fronts it).
	assert.False(t, r.standaloneGuardManaged(guardedChainNode("chain-fullnodes-0", true)))

	// A real child that declares its own individual ingress/gateway -> managed.
	childIngress := guardedChainNode("chain-fullnodes-0", true)
	childIngress.Spec.Ingress = &appsv1.IngressConfig{Host: "0.rpc.example.com"}
	assert.True(t, r.standaloneGuardManaged(childIngress))

	childGateway := guardedChainNode("chain-fullnodes-0", true)
	childGateway.Spec.Gateway = &appsv1.GatewayConfig{Host: "0.rpc.example.com"}
	assert.True(t, r.standaloneGuardManaged(childGateway))
}

// TestDisableGuardUndeploys verifies disabling CosmoGuard removes the previously-created guard.
func TestDisableGuardUndeploys(t *testing.T) {
	cn := guardedChainNode("node-0", false)
	cn.Spec.Config.CosmoGuard.Dashboard = dashboardGatewayConfig(true)
	r := cosmoGuardTestReconciler(t, cn)
	require.NoError(t, ensureGuard(r, context.Background(), cn))

	// Confirm it was created first, then disable and reconcile again.
	sts := &k8sappsv1.StatefulSet{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "node-0-cg"}, sts))

	// Disable, then finalize (teardown runs after routes are retargeted, not in ensureCosmoGuard).
	cn.Spec.Config.CosmoGuard.Enable = false
	require.NoError(t, ensureGuard(r, context.Background(), cn))
	require.NoError(t, r.finalizeCosmoGuard(context.Background(), cn, true))

	err := r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "node-0-cg"}, &k8sappsv1.StatefulSet{})
	assert.Error(t, err, "guard statefulset should be removed when disabled")
	err = r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "node-0-cg-peer"}, &corev1.Service{})
	assert.Error(t, err, "peer service should be removed when disabled")
	err = r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "node-0-cg-cluster"}, &corev1.Secret{})
	assert.Error(t, err, "encryption secret should be removed when disabled")
	for _, name := range []string{"node-0-cg-dashboard", "node-0-cg-dashboard-http-redirect"} {
		err = r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: name}, &gwapiv1.HTTPRoute{})
		assert.True(t, apierrors.IsNotFound(err), "dashboard HTTPRoute %s should be removed when disabled", name)
	}
}
