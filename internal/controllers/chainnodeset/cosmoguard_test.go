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
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	appsv1 "github.com/voluzi/cosmopilot/v5/api/v1"
	"github.com/voluzi/cosmopilot/v5/internal/chainutils"
	"github.com/voluzi/cosmopilot/v5/internal/controllers"
	"github.com/voluzi/cosmopilot/v5/internal/cosmoguard"
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

func TestCosmoGuardConfigRollout(t *testing.T) {
	ctx := context.Background()
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "rules", Namespace: "ns"}, Data: map[string]string{"cosmoguard.yaml": "lcd: {rules: [{paths: [/old], action: allow}]}"}}
	ns, group := guardedNodeSet()
	r := newValidatorTestReconciler(t, ns, cm)
	name := groupCosmoGuardName(ns, group)
	apply := func() { _, err := r.ensureCosmoGuards(ctx, ns); require.NoError(t, err) }
	read := func() *k8sappsv1.StatefulSet {
		sts := &k8sappsv1.StatefulSet{}
		require.NoError(t, r.Get(ctx, client.ObjectKey{Namespace: "ns", Name: name}, sts))
		return sts
	}
	change := func(raw string) {
		require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(cm), cm))
		cm.Data["cosmoguard.yaml"] = raw
		require.NoError(t, r.Update(ctx, cm))
		apply()
	}
	apply()
	initial := read()
	require.NotEmpty(t, initial.Annotations["cosmopilot.voluzi.com/cosmoguard-restart-fingerprint"])
	require.Empty(t, initial.Spec.Template.Annotations["cosmopilot.voluzi.com/cosmoguard-restart"])
	t.Run("rules only preserves template", func(t *testing.T) {
		change("lcd: {rules: [{paths: [/new], action: deny}]}")
		assert.Equal(t, initial.Spec.Template, read().Spec.Template)
	})
	t.Run("auth rolls exactly once", func(t *testing.T) {
		change("auth: {enable: true}\nlcd: {rules: [{paths: [/new], action: deny}]}")
		rolled := read()
		require.NotEqual(t, initial.Spec.Template, rolled.Spec.Template)
		require.NotEmpty(t, rolled.Spec.Template.Annotations["cosmopilot.voluzi.com/cosmoguard-restart"])
		apply()
		assert.Equal(t, rolled.ResourceVersion, read().ResourceVersion)
	})
	t.Run("invalid leaves state unchanged", func(t *testing.T) {
		previous := read()
		change("auth: [invalid")
		assert.Equal(t, previous.Spec.Template, read().Spec.Template)
		assert.Equal(t, previous.Annotations, read().Annotations)
	})
	t.Run("upgrade adopts without rollout", func(t *testing.T) {
		live := read()
		for key := range live.Annotations {
			if key != "banzaicloud.com/last-applied" {
				delete(live.Annotations, key)
			}
		}
		require.NoError(t, r.Update(ctx, live))
		change("auth: {enable: false}")
		adopted := read()
		assert.Equal(t, live.Spec.Template, adopted.Spec.Template)
		require.NotEmpty(t, adopted.Annotations["cosmopilot.voluzi.com/cosmoguard-restart-fingerprint"])
	})
}

func TestCosmoGuardConfigMapRequests(t *testing.T) {
	enabled, _ := guardedNodeSet()
	enabled.Spec.Nodes = append(enabled.Spec.Nodes, enabled.Spec.Nodes[0])
	enabled.Spec.Nodes[1].Name = "second"
	disabled := enabled.DeepCopy()
	disabled.Name = "disabled"
	disabled.Spec.Nodes = disabled.Spec.Nodes[:1]
	disabled.Spec.Nodes[0].Config.CosmoGuard.Enable = false
	zero := enabled.DeepCopy()
	zero.Name = "zero"
	zero.Spec.Nodes = zero.Spec.Nodes[:1]
	zero.Spec.Nodes[0].Instances = ptr.To(0)
	unrelated := enabled.DeepCopy()
	unrelated.Name = "unrelated"
	for i := range unrelated.Spec.Nodes {
		unrelated.Spec.Nodes[i].Config.CosmoGuard.Config.Name = "other"
	}
	otherNamespace := enabled.DeepCopy()
	otherNamespace.Name = "other-namespace"
	otherNamespace.Namespace = "elsewhere"
	validator := disabled.DeepCopy()
	validator.Name = "validator"
	validator.Spec.Nodes[0].Validator = &appsv1.NodeSetValidatorConfig{Config: enabled.Spec.Nodes[0].Config.DeepCopy()}
	r := newValidatorTestReconciler(t, enabled, disabled, zero, unrelated, otherNamespace, validator)
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "rules", Namespace: "ns"}}
	requests := r.cosmoGuardConfigMapRequests(context.Background(), cm)
	var names []string
	for _, request := range requests {
		names = append(names, request.Name)
	}
	assert.ElementsMatch(t, []string{enabled.Name, validator.Name}, names)
}

func TestCosmoGuardRulesOnlyAfterEnvironmentChange(t *testing.T) {
	cases := []struct {
		name          string
		change        func(*appsv1.Config)
		credentialKey string
	}{
		{name: "EVM enable", change: func(cfg *appsv1.Config) { cfg.EvmEnabled = ptr.To(true) }},
		{name: "dashboard enable", change: func(cfg *appsv1.Config) { cfg.CosmoGuard.Dashboard.Enable = true }},
		{name: "dashboard port", change: func(cfg *appsv1.Config) { cfg.CosmoGuard.Dashboard.Port = ptr.To(int32(8081)) }},
		{name: "dashboard username Secret rotation", credentialKey: "user"},
		{name: "dashboard password Secret rotation", credentialKey: "password"},
	}
	for _, tc := range cases {
		for _, simultaneous := range []bool{false, true} {
			sequence := "then rules only"
			if simultaneous {
				sequence = "with restart-required file change"
			}
			t.Run(tc.name+" "+sequence, func(t *testing.T) {
				ctx := context.Background()
				ns, group := guardedNodeSet()
				group.Config.CosmoGuard.Dashboard = &appsv1.CosmoGuardDashboardConfig{
					Enable: tc.name != "dashboard enable",
					BasicAuth: &appsv1.CosmoGuardDashboardAuth{
						Username: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "dashboard-auth"}, Key: "user"},
						Password: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "dashboard-auth"}, Key: "password"},
					},
				}
				credentials := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "dashboard-auth", Namespace: "ns"}, Data: map[string][]byte{"user": []byte("test-user"), "password": []byte("initial-password")}}
				cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "rules", Namespace: "ns"}, Data: map[string]string{"cosmoguard.yaml": "lcd: {rules: [{paths: [/old], action: allow}]}"}}
				r := newValidatorTestReconciler(t, ns, cm, credentials)
				apply := func() { _, err := r.ensureCosmoGuards(ctx, ns); require.NoError(t, err) }
				read := func() *k8sappsv1.StatefulSet {
					sts := &k8sappsv1.StatefulSet{}
					require.NoError(t, r.Get(ctx, client.ObjectKey{Namespace: "ns", Name: groupCosmoGuardName(ns, group)}, sts))
					return sts
				}
				changeFile := func(raw string) {
					require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(cm), cm))
					cm.Data["cosmoguard.yaml"] = raw
					require.NoError(t, r.Update(ctx, cm))
				}
				apply()
				initial := read()
				apply()
				require.Equal(t, initial.ResourceVersion, read().ResourceVersion, "unchanged file and environment must not write")
				if tc.credentialKey != "" {
					require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(credentials), credentials))
					credentials.Data[tc.credentialKey] = []byte("rotated-credential")
					require.NoError(t, r.Update(ctx, credentials))
				} else {
					require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(ns), ns))
					tc.change(ns.Spec.Nodes[0].Config)
					require.NoError(t, r.Update(ctx, ns))
				}
				if simultaneous {
					changeFile("auth: {enable: true}\nlcd: {rules: [{paths: [/old], action: allow}]}")
				}
				apply()
				refreshed := read()
				assert.NotEqual(t, initial.Spec.Template, refreshed.Spec.Template)
				assert.NotEmpty(t, refreshed.Spec.Template.Annotations[controllers.AnnotationCosmoGuardRestart])
				assert.NotEqual(t, initial.Annotations[controllers.AnnotationCosmoGuardConfigDigest], refreshed.Annotations[controllers.AnnotationCosmoGuardConfigDigest])
				if tc.credentialKey != "" {
					require.Equal(t, initial.Spec.Template.Spec, refreshed.Spec.Template.Spec, "Secret rotation changes only the rollout marker")
				}
				apply()
				require.Equal(t, refreshed.ResourceVersion, read().ResourceVersion, "environment changes must request only one rollout")
				auth := ""
				if simultaneous {
					auth = "auth: {enable: true}\n"
				}
				changeFile(auth + "lcd: {rules: [{paths: [/new], action: deny}]}")
				apply()
				assert.Equal(t, refreshed.Spec.Template, read().Spec.Template, "rules-only edits must not roll after environment changes")
				converged := read()
				apply()
				assert.Equal(t, converged.ResourceVersion, read().ResourceVersion)
			})
		}
	}
}
