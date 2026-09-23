package chainnodeset

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	k8sappsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	appsv1 "github.com/voluzi/cosmopilot/v3/api/v1"
	"github.com/voluzi/cosmopilot/v3/internal/chainutils"
	"github.com/voluzi/cosmopilot/v3/internal/controllers"
	"github.com/voluzi/cosmopilot/v3/internal/cosmoguard"
)

func guardedNodeSet() (*appsv1.ChainNodeSet, appsv1.NodeGroupSpec) {
	group := appsv1.NodeGroupSpec{
		Name: "fullnodes",
		Config: &appsv1.Config{
			CosmoGuard: &appsv1.CosmoGuardConfig{
				Enable: true,
				Config: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "rules"},
					Key:                  "cosmoguard.yaml",
				},
			},
		},
	}
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "chain", Namespace: "ns", UID: types.UID("chain-uid")},
		Spec:       appsv1.ChainNodeSetSpec{Nodes: []appsv1.NodeGroupSpec{group}},
	}
	return nodeSet, group
}

type nodeSetGatewayUnavailableClient struct {
	client.Client
}

func (c nodeSetGatewayUnavailableClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if _, ok := obj.(*gwapiv1.HTTPRoute); ok {
		return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: gwapiv1.GroupVersion.Group, Kind: "HTTPRoute"}}
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func (c nodeSetGatewayUnavailableClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if _, ok := list.(*gwapiv1.HTTPRouteList); ok {
		return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: gwapiv1.GroupVersion.Group, Kind: "HTTPRoute"}}
	}
	return c.Client.List(ctx, list, opts...)
}

func nodeSetDashboardGatewayConfig(redirect bool) *appsv1.CosmoGuardDashboardConfig {
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

// TestGroupServiceFlipsToGuardOnlyWhenReady verifies the group Service targets the node pods on raw
// ports until the guard has rolled out, then flips its selector and target ports to the guard.
func TestGroupServiceFlipsToGuardOnlyWhenReady(t *testing.T) {
	nodeSet, group := guardedNodeSet()
	r := newValidatorTestReconciler(t, nodeSet)

	// Not ready: node selector + raw ports.
	svc, err := r.getServiceSpec(nodeSet, group, false)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		controllers.LabelChainNodeSet:      "chain",
		controllers.LabelChainNodeSetGroup: "fullnodes",
	}, svc.Spec.Selector)
	assert.Equal(t, int32(chainutils.RpcPort), svc.Spec.Ports[0].TargetPort.IntVal)

	// Ready: guard selector + guard listener target ports.
	svc, err = r.getServiceSpec(nodeSet, group, true)
	require.NoError(t, err)
	assert.Equal(t, cosmoguard.InstanceLabels(groupCosmoGuardName(nodeSet, group)), svc.Spec.Selector)
	assert.Equal(t, int32(controllers.CosmoGuardRpcPort), svc.Spec.Ports[0].TargetPort.IntVal)
	assert.Equal(t, int32(controllers.CosmoGuardLcdPort), svc.Spec.Ports[1].TargetPort.IntVal)
	assert.Equal(t, int32(controllers.CosmoGuardGrpcPort), svc.Spec.Ports[2].TargetPort.IntVal)
	// Public port numbers are preserved.
	assert.Equal(t, int32(chainutils.RpcPort), svc.Spec.Ports[0].Port)
}

// TestGroupGuardScheduling verifies the group guard follows the placement of the pods it fronts: a
// regular group uses the group-level nodeSelector/affinity and the nodes' priority + the group
// Config's ServiceAccount; a validator group uses the validator sub-config's nodeSelector/affinity,
// the validators' priority, and the validator Config's ServiceAccount.
func TestGroupGuardScheduling(t *testing.T) {
	// Regular group: group-level placement, nodes priority, group Config SA.
	regularNodeSet, regularGroup := guardedNodeSet()
	regularGroup.NodeSelector = map[string]string{"pool": "nodes"}
	regularGroup.Affinity = &corev1.Affinity{}
	regularGroup.Config.ServiceAccountName = ptr.To("nodes-sa")
	regularNodeSet.Spec.Nodes = []appsv1.NodeGroupSpec{regularGroup}

	r := newValidatorTestReconciler(t, regularNodeSet)
	r.opts = &controllers.ControllerRunOptions{ReleaseName: "rel"}

	p := r.groupCosmoGuardParams(regularNodeSet, regularGroup)
	assert.Equal(t, map[string]string{"pool": "nodes"}, p.NodeSelector)
	assert.Equal(t, regularGroup.Affinity, p.Affinity)
	assert.Equal(t, "rel-nodes", p.PriorityClassName)
	assert.Equal(t, "nodes-sa", p.ServiceAccountName)

	// Validator group: the pods are rendered from group.Validator, so the guard must follow it.
	valAffinity := &corev1.Affinity{}
	valGroup := appsv1.NodeGroupSpec{
		Name: "validators",
		Validator: &appsv1.NodeSetValidatorConfig{
			NodeSelector: map[string]string{"pool": "validators"},
			Affinity:     valAffinity,
			Config: &appsv1.Config{
				ServiceAccountName: ptr.To("validators-sa"),
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
	valNodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "chain", Namespace: "ns"},
		Spec:       appsv1.ChainNodeSetSpec{Nodes: []appsv1.NodeGroupSpec{valGroup}},
	}

	p = r.groupCosmoGuardParams(valNodeSet, valGroup)
	assert.Equal(t, map[string]string{"pool": "validators"}, p.NodeSelector)
	assert.Equal(t, valAffinity, p.Affinity)
	assert.Equal(t, "rel-validators", p.PriorityClassName)
	assert.Equal(t, "validators-sa", p.ServiceAccountName)
}

// TestZeroInstanceGroupSkipsGuard verifies a group scaled to instances:0 (still CosmoGuard-enabled)
// gets no guard: it is absent from the expected set (so cleanup removes any prior guard), never marked
// ready (so its Service falls back to the empty raw selector), and is not route-guardable.
func TestZeroInstanceGroupSkipsGuard(t *testing.T) {
	nodeSet, group := guardedNodeSet()
	group.Instances = ptr.To(0)
	nodeSet.Spec.Nodes = []appsv1.NodeGroupSpec{group}
	r := newValidatorTestReconciler(t, nodeSet)

	res, err := r.ensureCosmoGuards(context.Background(), nodeSet)
	require.NoError(t, err)

	name := groupCosmoGuardName(nodeSet, group)
	assert.NotContains(t, res.expected, name, "no guard expected for a zero-instance group")
	assert.False(t, res.ready[group.Name], "zero-instance group must not flip its Service to a guard")
	assert.False(t, cosmoGuardRouteGuardable(nodeSet, []string{group.Name}), "route over a zero-instance group is not guardable")
	assert.Empty(t, res.states, "a zero-instance group is not reported in the CosmoGuardReady condition")
}

// TestGuardParamsUseDiscovery verifies a group's guard is configured to discover node pods through
// the headless upstream Service.
func TestGuardParamsUseDiscovery(t *testing.T) {
	nodeSet, group := guardedNodeSet()
	r := newValidatorTestReconciler(t, nodeSet)

	p := r.groupCosmoGuardParams(nodeSet, group)
	assert.Equal(t, "chain-fullnodes-cg-upstream.ns.svc.cluster.local", p.DiscoveryHost)
	assert.Empty(t, p.UpstreamHost)
	assert.Equal(t, "rules", p.ConfigMap.Name)
}

func TestGroupGuardCreatesDashboardHTTPRoutes(t *testing.T) {
	nodeSet, group := guardedNodeSet()
	group.Config.CosmoGuard.Dashboard = nodeSetDashboardGatewayConfig(true)
	nodeSet.Spec.Nodes = []appsv1.NodeGroupSpec{group}
	r := newValidatorTestReconciler(t, nodeSet)

	_, err := r.ensureCosmoGuards(context.Background(), nodeSet)
	require.NoError(t, err)

	backend := &gwapiv1.HTTPRoute{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "chain-fullnodes-cg-dashboard"}, backend))
	require.Len(t, backend.Spec.Rules, 1)
	require.Len(t, backend.Spec.Rules[0].BackendRefs, 1)
	assert.Equal(t, "chain-fullnodes-cg", string(backend.Spec.Rules[0].BackendRefs[0].Name))
	assert.True(t, metav1.IsControlledBy(backend, nodeSet))

	redirect := &gwapiv1.HTTPRoute{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "chain-fullnodes-cg-dashboard-http-redirect"}, redirect))
	assert.True(t, metav1.IsControlledBy(redirect, nodeSet))
}

func TestGroupDashboardGatewayPreservesIngressWhenGatewayAPIUnavailable(t *testing.T) {
	ctx := context.Background()
	nodeSet, group := guardedNodeSet()
	group.Config.CosmoGuard.Dashboard = nodeSetDashboardGatewayConfig(false)
	nodeSet.Spec.Nodes = []appsv1.NodeGroupSpec{group}
	r := newValidatorTestReconciler(t, nodeSet)

	ingress := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "chain-fullnodes-cg-dashboard",
			Namespace: "ns",
			Labels:    map[string]string{controllers.LabelScope: scopeCosmoGuard},
		},
	}
	require.NoError(t, controllerutil.SetControllerReference(nodeSet, ingress, r.Scheme))
	require.NoError(t, r.Create(ctx, ingress))
	r.Client = nodeSetGatewayUnavailableClient{Client: r.Client}

	guards, err := r.ensureCosmoGuards(ctx, nodeSet)
	require.NoError(t, err)
	require.NoError(t, r.cleanupStaleCosmoGuards(ctx, nodeSet, guards.expected, guards.expectedIngress, guards.expectedRoutes))
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(ingress), &networkingv1.Ingress{}))
	// Missing CRDs are permanent, not a status we are waiting on: requeueing every few seconds would
	// spin forever without ever converging, so such a cluster keeps the normal reconcile cadence.
	assert.False(t, guards.routesPending, "an unavailable Gateway API must not trigger the short retry period")
}

// TestGroupRetainedDashboardIngressFollowsPortChange verifies the group's retained fallback Ingress
// is repointed when a Gateway migration also changes the dashboard port. The guard Service is
// reconciled to the new port in the same pass, so an Ingress left on the old numeric port would 503.
func TestGroupRetainedDashboardIngressFollowsPortChange(t *testing.T) {
	ctx := context.Background()
	nodeSet, group := guardedNodeSet()
	group.Config.CosmoGuard.Dashboard = nodeSetDashboardGatewayConfig(false)
	group.Config.CosmoGuard.Dashboard.Port = ptr.To[int32](9100)
	nodeSet.Spec.Nodes = []appsv1.NodeGroupSpec{group}
	r := newValidatorTestReconciler(t, nodeSet)

	ingress := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "chain-fullnodes-cg-dashboard",
			Namespace: "ns",
			Labels:    map[string]string{controllers.LabelScope: scopeCosmoGuard},
		},
		Spec: networkingv1.IngressSpec{
			Rules: []networkingv1.IngressRule{{
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{{
							Backend: networkingv1.IngressBackend{
								Service: &networkingv1.IngressServiceBackend{
									Name: "chain-fullnodes-cg",
									Port: networkingv1.ServiceBackendPort{Number: 8080},
								},
							},
						}},
					},
				},
			}},
		},
	}
	require.NoError(t, controllerutil.SetControllerReference(nodeSet, ingress, r.Scheme))
	require.NoError(t, r.Create(ctx, ingress))

	guards, err := r.ensureCosmoGuards(ctx, nodeSet)
	require.NoError(t, err)
	require.True(t, guards.routesPending, "route is not accepted yet, so the Ingress is retained")

	live := &networkingv1.Ingress{}
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(ingress), live))
	assert.Equal(t, int32(9100), live.Spec.Rules[0].HTTP.Paths[0].Backend.Service.Port.Number,
		"retained fallback Ingress must follow the guard Service's current dashboard port")
}

func TestGroupDashboardGatewayWaitsForAcceptedRouteBeforeDeletingIngress(t *testing.T) {
	ctx := context.Background()
	nodeSet, group := guardedNodeSet()
	group.Config.CosmoGuard.Dashboard = nodeSetDashboardGatewayConfig(false)
	nodeSet.Spec.Nodes = []appsv1.NodeGroupSpec{group}
	r := newValidatorTestReconciler(t, nodeSet)

	ingress := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "chain-fullnodes-cg-dashboard",
			Namespace: "ns",
			Labels:    map[string]string{controllers.LabelScope: scopeCosmoGuard},
		},
	}
	require.NoError(t, controllerutil.SetControllerReference(nodeSet, ingress, r.Scheme))
	require.NoError(t, r.Create(ctx, ingress))

	guards, err := r.ensureCosmoGuards(ctx, nodeSet)
	require.NoError(t, err)
	require.NoError(t, r.cleanupStaleCosmoGuards(ctx, nodeSet, guards.expected, guards.expectedIngress, guards.expectedRoutes))
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(ingress), &networkingv1.Ingress{}))
	// Route acceptance arrives as an HTTPRoute STATUS update, which no watch here admits, so the
	// pending flag is what makes Reconcile re-check sooner than the reconcile period.
	assert.True(t, guards.routesPending, "an unaccepted route must request a prompt re-check")

	route := &gwapiv1.HTTPRoute{}
	require.NoError(t, r.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "chain-fullnodes-cg-dashboard"}, route))
	route.Status.Parents = []gwapiv1.RouteParentStatus{{
		ParentRef:      route.Spec.ParentRefs[0],
		ControllerName: "example.net/gateway-controller",
		Conditions: []metav1.Condition{
			{Type: string(gwapiv1.RouteConditionAccepted), Status: metav1.ConditionTrue, ObservedGeneration: route.Generation},
			{Type: string(gwapiv1.RouteConditionResolvedRefs), Status: metav1.ConditionTrue, ObservedGeneration: route.Generation},
		},
	}}
	require.NoError(t, r.Update(ctx, route))

	guards, err = r.ensureCosmoGuards(ctx, nodeSet)
	require.NoError(t, err)
	require.NoError(t, r.cleanupStaleCosmoGuards(ctx, nodeSet, guards.expected, guards.expectedIngress, guards.expectedRoutes))
	err = r.Get(ctx, client.ObjectKeyFromObject(ingress), &networkingv1.Ingress{})
	assert.True(t, apierrors.IsNotFound(err))
	assert.False(t, guards.routesPending, "an accepted route falls back to the normal reconcile period")
}

func TestGroupDashboardSwitchToIngressRemovesHTTPRoutes(t *testing.T) {
	ctx := context.Background()
	nodeSet, group := guardedNodeSet()
	group.Config.CosmoGuard.Dashboard = nodeSetDashboardGatewayConfig(true)
	nodeSet.Spec.Nodes = []appsv1.NodeGroupSpec{group}
	r := newValidatorTestReconciler(t, nodeSet)

	guards, err := r.ensureCosmoGuards(ctx, nodeSet)
	require.NoError(t, err)
	require.NoError(t, r.cleanupStaleCosmoGuards(ctx, nodeSet, guards.expected, guards.expectedIngress, guards.expectedRoutes))

	group.Config.CosmoGuard.Dashboard = &appsv1.CosmoGuardDashboardConfig{
		Enable:  true,
		Ingress: &appsv1.CosmoGuardDashboardIngress{Host: "guard.example.com"},
	}
	nodeSet.Spec.Nodes = []appsv1.NodeGroupSpec{group}
	guards, err = r.ensureCosmoGuards(ctx, nodeSet)
	require.NoError(t, err)
	require.NoError(t, r.cleanupStaleCosmoGuards(ctx, nodeSet, guards.expected, guards.expectedIngress, guards.expectedRoutes))

	require.NoError(t, r.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "chain-fullnodes-cg-dashboard"}, &networkingv1.Ingress{}))
	for _, name := range []string{"chain-fullnodes-cg-dashboard", "chain-fullnodes-cg-dashboard-http-redirect"} {
		err := r.Get(ctx, client.ObjectKey{Namespace: "ns", Name: name}, &gwapiv1.HTTPRoute{})
		assert.True(t, apierrors.IsNotFound(err), "HTTPRoute %s should be removed", name)
	}
}

func TestGroupDashboardRemovesStaleRedirectHTTPRoute(t *testing.T) {
	ctx := context.Background()
	nodeSet, group := guardedNodeSet()
	group.Config.CosmoGuard.Dashboard = nodeSetDashboardGatewayConfig(true)
	nodeSet.Spec.Nodes = []appsv1.NodeGroupSpec{group}
	r := newValidatorTestReconciler(t, nodeSet)

	guards, err := r.ensureCosmoGuards(ctx, nodeSet)
	require.NoError(t, err)
	require.NoError(t, r.cleanupStaleCosmoGuards(ctx, nodeSet, guards.expected, guards.expectedIngress, guards.expectedRoutes))

	group.Config.CosmoGuard.Dashboard = nodeSetDashboardGatewayConfig(false)
	nodeSet.Spec.Nodes = []appsv1.NodeGroupSpec{group}
	guards, err = r.ensureCosmoGuards(ctx, nodeSet)
	require.NoError(t, err)
	require.NoError(t, r.cleanupStaleCosmoGuards(ctx, nodeSet, guards.expected, guards.expectedIngress, guards.expectedRoutes))

	err = r.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "chain-fullnodes-cg-dashboard-http-redirect"}, &gwapiv1.HTTPRoute{})
	assert.True(t, apierrors.IsNotFound(err))
}

// TestUpstreamServiceIsHeadless verifies the guard's upstream Service is headless, does NOT publish
// not-ready addresses (so only ready node pods are discoverable), and selects the group's node pods
// on raw ports.
func TestUpstreamServiceIsHeadless(t *testing.T) {
	nodeSet, group := guardedNodeSet()
	r := newValidatorTestReconciler(t, nodeSet)

	svc, err := r.buildGroupCosmoGuardUpstreamService(nodeSet, group)
	require.NoError(t, err)
	assert.Equal(t, corev1.ClusterIPNone, svc.Spec.ClusterIP)
	// Upstream discovery must only surface READY node pods, so the guard never routes client traffic
	// to syncing/upgrading/snapshotting nodes.
	assert.False(t, svc.Spec.PublishNotReadyAddresses)
	assert.Equal(t, map[string]string{
		controllers.LabelChainNodeSet:      "chain",
		controllers.LabelChainNodeSetGroup: "fullnodes",
	}, svc.Spec.Selector)
	assert.Equal(t, int32(chainutils.RpcPort), svc.Spec.Ports[0].TargetPort.IntVal)
}

// TestServiceSelectsGuard verifies the sticky-flip detector recognizes a Service already flipped to
// the guard, so a guarded Service is kept on the guard through transient rollout un-readiness.
func TestServiceSelectsGuard(t *testing.T) {
	flipped := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "chain-fullnodes", Namespace: "ns"},
		Spec:       corev1.ServiceSpec{Selector: cosmoguard.InstanceLabels("chain-fullnodes-cg")},
	}
	raw := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "chain-other", Namespace: "ns"},
		Spec: corev1.ServiceSpec{Selector: map[string]string{
			controllers.LabelChainNodeSet: "chain", controllers.LabelChainNodeSetGroup: "other",
		}},
	}
	r := newValidatorTestReconciler(t, flipped, raw)

	assert.True(t, r.serviceSelectsGuard(context.Background(), "ns", "chain-fullnodes"))
	assert.False(t, r.serviceSelectsGuard(context.Background(), "ns", "chain-other"))
	assert.False(t, r.serviceSelectsGuard(context.Background(), "ns", "missing"))
}

func TestCosmoGuardRouteGuardable(t *testing.T) {
	nodeSet, _ := guardedNodeSet()

	// A guarded .spec.nodes group -> guardable.
	assert.True(t, cosmoGuardRouteGuardable(nodeSet, []string{"fullnodes"}))
	// Validator group, unknown group, mixed, or empty -> NOT guardable (structural; not sticky).
	assert.False(t, cosmoGuardRouteGuardable(nodeSet, []string{appsv1.ReservedValidatorGroupName}))
	assert.False(t, cosmoGuardRouteGuardable(nodeSet, []string{"fullnodes", appsv1.ReservedValidatorGroupName}))
	assert.False(t, cosmoGuardRouteGuardable(nodeSet, []string{"nope"}))
	assert.False(t, cosmoGuardRouteGuardable(nodeSet, nil))
}

func TestCosmoGuardRouteReady(t *testing.T) {
	nodeSet, _ := guardedNodeSet()

	// Readiness: every group's guard must be ready.
	assert.False(t, cosmoGuardRouteReady(nodeSet, []string{"fullnodes"}, map[string]bool{"fullnodes": false}))
	assert.True(t, cosmoGuardRouteReady(nodeSet, []string{"fullnodes"}, map[string]bool{"fullnodes": true}))
	assert.False(t, cosmoGuardRouteReady(nodeSet, []string{"a", "b"}, map[string]bool{"a": true, "b": false}))
}

// TestGroupGuardReadinessCondition verifies the ChainNodeSet reports a group guard that is not serving
// yet (its routes are not filtered), records one Warning event for the transition, and clears the
// condition once no group has a guard.
func TestGroupGuardReadinessCondition(t *testing.T) {
	ctx := context.Background()
	nodeSet, group := guardedNodeSet()
	r := newValidatorTestReconciler(t, nodeSet)

	guards, err := r.ensureCosmoGuards(ctx, nodeSet)
	require.NoError(t, err)
	require.NoError(t, r.updateCosmoGuardCondition(ctx, nodeSet, guards.states))

	cond := meta.FindStatusCondition(nodeSet.Status.Conditions, appsv1.ConditionCosmoGuardReady)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, appsv1.ReasonCosmoGuardNotServing, cond.Reason)
	assert.Contains(t, cond.Message, groupCosmoGuardName(nodeSet, group))
	assert.Contains(t, cond.Message, "not filtered")

	stored := &appsv1.ChainNodeSet{}
	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(nodeSet), stored))
	require.NotNil(t, meta.FindStatusCondition(stored.Status.Conditions, appsv1.ConditionCosmoGuardReady))

	require.NoError(t, r.updateCosmoGuardCondition(ctx, nodeSet, guards.states))
	events := r.recorder.(*record.FakeRecorder).Events
	require.Len(t, events, 1, "the transition is recorded once")
	assert.Contains(t, <-events, "Warning "+appsv1.ReasonCosmoGuardNotServing)

	require.NoError(t, r.updateCosmoGuardCondition(ctx, nodeSet, nil))
	assert.Nil(t, meta.FindStatusCondition(nodeSet.Status.Conditions, appsv1.ConditionCosmoGuardReady))
}

// TestGroupGuardWithoutConfigIsReported verifies a group guard skipped for a missing config ConfigMap
// is reported in the condition rather than only logged.
func TestGroupGuardWithoutConfigIsReported(t *testing.T) {
	nodeSet, group := guardedNodeSet()
	group.Config.CosmoGuard.Config = nil
	nodeSet.Spec.Nodes = []appsv1.NodeGroupSpec{group}
	r := newValidatorTestReconciler(t, nodeSet)

	guards, err := r.ensureCosmoGuards(context.Background(), nodeSet)
	require.NoError(t, err)
	require.Equal(t, []controllers.GuardState{{Name: groupCosmoGuardName(nodeSet, group), ConfigMissing: true}}, guards.states)
}

// guardConditionAfterReconcile runs the guard and Service reconciliation and returns the resulting
// CosmoGuardReady condition, as the controller does.
func guardConditionAfterReconcile(t *testing.T, r *Reconciler, nodeSet *appsv1.ChainNodeSet) *metav1.Condition {
	t.Helper()
	ctx := context.Background()
	guards, err := r.ensureCosmoGuards(ctx, nodeSet)
	require.NoError(t, err)
	routes, err := r.ensureServices(ctx, nodeSet, guards)
	require.NoError(t, err)
	require.NoError(t, r.updateCosmoGuardCondition(ctx, nodeSet, append(guards.states, routes.forCondition(true)...)))
	return meta.FindStatusCondition(nodeSet.Status.Conditions, appsv1.ConditionCosmoGuardReady)
}

// TestGlobalRouteNotFilteredWhileGuardPartlyRolledOut verifies a global route is reported as not
// filtered while the group guard serves on some replicas only: the group Service flips on the first
// ready replica, but a global route Service flips only once every replica is up.
func TestGlobalRouteNotFilteredWhileGuardPartlyRolledOut(t *testing.T) {
	ctx := context.Background()
	nodeSet, group := guardedNodeSet()
	group.Config.CosmoGuard.Replicas = ptr.To[int32](2)
	nodeSet.Spec.Nodes = []appsv1.NodeGroupSpec{group}
	nodeSet.Spec.Ingresses = []appsv1.GlobalIngressConfig{{Name: "public", Groups: []string{group.Name}}}
	r := newValidatorTestReconciler(t, nodeSet)

	_, err := r.ensureCosmoGuards(ctx, nodeSet)
	require.NoError(t, err)
	sts := &k8sappsv1.StatefulSet{}
	require.NoError(t, r.Get(ctx, client.ObjectKey{Namespace: "ns", Name: groupCosmoGuardName(nodeSet, group)}, sts))
	sts.Status = k8sappsv1.StatefulSetStatus{ObservedGeneration: sts.Generation, Replicas: 2, ReadyReplicas: 1, UpdatedReplicas: 2}
	require.NoError(t, r.Status().Update(ctx, sts))

	cond := guardConditionAfterReconcile(t, r, nodeSet)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	route := nodeSet.Spec.Ingresses[0].GetName(nodeSet)
	assert.Contains(t, cond.Message, "public route "+route+" has not switched to its guard yet")
	assert.Contains(t, cond.Message, "not filtered")
	assert.NotContains(t, cond.Message, "CosmoGuard "+groupCosmoGuardName(nodeSet, group),
		"the group guard itself is serving")

	sts.Status.ReadyReplicas = 2
	require.NoError(t, r.Status().Update(ctx, sts))
	events := r.recorder.(*record.FakeRecorder).Events
	for len(events) > 0 {
		<-events
	}
	cond = guardConditionAfterReconcile(t, r, nodeSet)
	assert.Equal(t, metav1.ConditionTrue, cond.Status, cond.Message)
	require.Len(t, events, 1, "recovery is recorded once")
	assert.Contains(t, <-events, "Normal "+appsv1.ReasonCosmoGuardServing)

	require.NoError(t, r.updateCosmoGuardCondition(ctx, nodeSet, nil))
	assert.Empty(t, events, "removing the condition records no event")
}

// TestGroupGuardDownAfterFlipKeepsRoutesOnGuard verifies a group guard that stops serving after its
// Service has flipped is reported as keeping traffic on the guard rather than unfiltered.
func TestGroupGuardDownAfterFlipKeepsRoutesOnGuard(t *testing.T) {
	nodeSet, group := guardedNodeSet()
	r := newValidatorTestReconciler(t, nodeSet)
	flipped := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: group.GetServiceName(nodeSet), Namespace: "ns"},
		Spec:       corev1.ServiceSpec{Selector: cosmoGuardGroupSelector(nodeSet, group)},
	}
	require.NoError(t, controllerutil.SetControllerReference(nodeSet, flipped, r.Scheme))
	require.NoError(t, r.Create(context.Background(), flipped))

	cond := guardConditionAfterReconcile(t, r, nodeSet)
	require.NotNil(t, cond)
	assert.Equal(t, appsv1.ReasonCosmoGuardNotServing, cond.Reason)
	assert.Contains(t, cond.Message, "stay on the guard")
	assert.NotContains(t, cond.Message, "not filtered")
}

// TestGlobalRouteThroughInternalServicesIsNotReported verifies a route that bypasses CosmoGuard by
// configuration (useInternalServices) is not reported as a guarded route.
func TestGlobalRouteThroughInternalServicesIsNotReported(t *testing.T) {
	nodeSet, group := guardedNodeSet()
	nodeSet.Spec.Ingresses = []appsv1.GlobalIngressConfig{{Name: "public", Groups: []string{group.Name}, UseInternalServices: ptr.To(true)}}
	r := newValidatorTestReconciler(t, nodeSet)

	cond := guardConditionAfterReconcile(t, r, nodeSet)
	require.NotNil(t, cond)
	assert.NotContains(t, cond.Message, nodeSet.Spec.Ingresses[0].GetName(nodeSet))
}

// TestGlobalRouteSpanningUnguardedGroupIsReportedBypassed verifies a global route over a guarded group
// and an unguarded one, which selects raw node pods forever, is reported instead of hidden behind a
// serving group guard.
func TestGlobalRouteSpanningUnguardedGroupIsReportedBypassed(t *testing.T) {
	ctx := context.Background()
	nodeSet, group := guardedNodeSet()
	plain := appsv1.NodeGroupSpec{Name: "archive"}
	nodeSet.Spec.Nodes = []appsv1.NodeGroupSpec{group, plain}
	nodeSet.Spec.Ingresses = []appsv1.GlobalIngressConfig{{Name: "public", Groups: []string{group.Name, plain.Name}}}
	r := newValidatorTestReconciler(t, nodeSet)

	_, err := r.ensureCosmoGuards(ctx, nodeSet)
	require.NoError(t, err)
	sts := &k8sappsv1.StatefulSet{}
	require.NoError(t, r.Get(ctx, client.ObjectKey{Namespace: "ns", Name: groupCosmoGuardName(nodeSet, group)}, sts))
	sts.Status = k8sappsv1.StatefulSetStatus{ObservedGeneration: sts.Generation, Replicas: 1, ReadyReplicas: 1, UpdatedReplicas: 1}
	require.NoError(t, r.Status().Update(ctx, sts))

	cond := guardConditionAfterReconcile(t, r, nodeSet)
	require.NotNil(t, cond)
	assert.Equal(t, appsv1.ReasonCosmoGuardBypassed, cond.Reason)
	assert.Contains(t, cond.Message, nodeSet.Spec.Ingresses[0].GetName(nodeSet)+" also spans groups without CosmoGuard")
}

// TestSwitchedGlobalRouteStaysReadyDuringGuardScaleOut verifies a global route already on its guard is
// not reported as down while a scale-out leaves some replicas not ready: the full-rollout gate only
// holds back the first switch.
func TestSwitchedGlobalRouteStaysReadyDuringGuardScaleOut(t *testing.T) {
	ctx := context.Background()
	nodeSet, group := guardedNodeSet()
	group.Config.CosmoGuard.Replicas = ptr.To[int32](3)
	nodeSet.Spec.Nodes = []appsv1.NodeGroupSpec{group}
	nodeSet.Spec.Ingresses = []appsv1.GlobalIngressConfig{{Name: "public", Groups: []string{group.Name}}}
	r := newValidatorTestReconciler(t, nodeSet)

	route := nodeSet.Spec.Ingresses[0].GetName(nodeSet)
	switched := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: route, Namespace: "ns"},
		Spec:       corev1.ServiceSpec{Selector: cosmoGuardRouteSelector(route)},
	}
	require.NoError(t, controllerutil.SetControllerReference(nodeSet, switched, r.Scheme))
	require.NoError(t, r.Create(ctx, switched))

	_, err := r.ensureCosmoGuards(ctx, nodeSet)
	require.NoError(t, err)
	sts := &k8sappsv1.StatefulSet{}
	require.NoError(t, r.Get(ctx, client.ObjectKey{Namespace: "ns", Name: groupCosmoGuardName(nodeSet, group)}, sts))
	sts.Status = k8sappsv1.StatefulSetStatus{ObservedGeneration: sts.Generation, Replicas: 3, ReadyReplicas: 2, UpdatedReplicas: 3}
	require.NoError(t, r.Status().Update(ctx, sts))

	cond := guardConditionAfterReconcile(t, r, nodeSet)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status, cond.Message)
}

// TestGatewayRoutesNotAppliedAreReportedNotSwitched verifies gateway routes are not reported as guarded
// when the Gateway API routes could not be applied: traffic may still use a preserved legacy Ingress.
func TestGatewayRoutesNotAppliedAreReportedNotSwitched(t *testing.T) {
	routes := routeGuardStates{
		ingress: []controllers.GuardState{{Name: "ingress", Route: true, Serving: true, Routed: true}},
		gateway: []controllers.GuardState{
			{Name: "gateway", Route: true, Serving: true, Routed: true},
			{Name: "mixed", Route: true, Bypassed: true},
		},
	}
	assert.Equal(t, append(routes.ingress, routes.gateway...), routes.forCondition(true))
	assert.Equal(t, []controllers.GuardState{
		{Name: "ingress", Route: true, Serving: true, Routed: true},
		{Name: "gateway", Route: true},
		{Name: "mixed", Route: true, Bypassed: true},
	}, routes.forCondition(false))
}
