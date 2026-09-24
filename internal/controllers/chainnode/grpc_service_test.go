package chainnode

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	k8sappsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	appsv1 "github.com/voluzi/cosmopilot/v3/api/v1"
	"github.com/voluzi/cosmopilot/v3/internal/chainutils"
	"github.com/voluzi/cosmopilot/v3/internal/controllers"
	"github.com/voluzi/cosmopilot/v3/internal/cosmoguard"
)

// apiBackend builds a multi-port API Service like the ones the gRPC Ingress used to target directly.
func apiBackend(name string, selector map[string]string, grpcTarget int32, publishNotReady bool) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec: corev1.ServiceSpec{
			Selector:                 selector,
			PublishNotReadyAddresses: publishNotReady,
			Ports: []corev1.ServicePort{
				{Name: chainutils.RpcPortName, Port: chainutils.RpcPort, TargetPort: intstr.FromInt32(chainutils.RpcPort)},
				{Name: chainutils.LcdPortName, Port: chainutils.LcdPort, TargetPort: intstr.FromInt32(chainutils.LcdPort)},
				{Name: chainutils.GrpcPortName, Port: chainutils.GrpcPort, TargetPort: intstr.FromInt32(grpcTarget)},
			},
		},
	}
}

func grpcChainNode(class *string) *appsv1.ChainNode {
	return &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "node-0", Namespace: "ns", UID: "node-0-uid"},
		Spec: appsv1.ChainNodeSpec{
			Ingress: &appsv1.IngressConfig{Host: "example.com", EnableRPC: true, EnableLCD: true, EnableGRPC: true, IngressClass: class},
		},
	}
}

func getService(t *testing.T, r *Reconciler, name string) *corev1.Service {
	t.Helper()
	svc := &corev1.Service{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: name}, svc))
	return svc
}

func getIngress(t *testing.T, r *Reconciler, name string) *networkingv1.Ingress {
	t.Helper()
	ing := &networkingv1.Ingress{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: name}, ing))
	return ing
}

func ingressBackend(ing *networkingv1.Ingress) *networkingv1.IngressServiceBackend {
	return ing.Spec.Rules[0].HTTP.Paths[0].Backend.Service
}

func TestGrpcIngressTargetsGrpcOnlyServiceWithH2CForTraefik(t *testing.T) {
	ctx := context.Background()
	cn := grpcChainNode(ptr.To("traefik"))
	selector := map[string]string{"app": "node-0"}
	r := cosmoGuardTestReconciler(t, cn, apiBackend("node-0", selector, chainutils.GrpcPort, false))

	require.NoError(t, r.ensureIngresses(ctx, cn))

	svc := getService(t, r, "node-0-grpc")
	assert.Equal(t, map[string]string{appsv1.TraefikServersSchemeAnnotation: "h2c"}, svc.Annotations)
	assert.Equal(t, selector, svc.Spec.Selector)
	require.Len(t, svc.Spec.Ports, 1)
	assert.Equal(t, int32(chainutils.GrpcPort), svc.Spec.Ports[0].Port)
	assert.Equal(t, intstr.FromInt32(chainutils.GrpcPort), svc.Spec.Ports[0].TargetPort)
	assert.True(t, metav1.IsControlledBy(svc, cn))

	backend := ingressBackend(getIngress(t, r, "node-0-grpc"))
	assert.Equal(t, "node-0-grpc", backend.Name)
	assert.Equal(t, int32(chainutils.GrpcPort), backend.Port.Number)

	// RPC/LCD stay on the shared Service, which gets no scheme annotation.
	api := getIngress(t, r, "node-0")
	for _, rule := range api.Spec.Rules {
		assert.Equal(t, "node-0", rule.HTTP.Paths[0].Backend.Service.Name)
	}
	assert.NotContains(t, getService(t, r, "node-0").Annotations, appsv1.TraefikServersSchemeAnnotation)
}

func TestGrpcIngressNginxBehaviourUnchanged(t *testing.T) {
	ctx := context.Background()
	selector := map[string]string{"app": "node-0"}

	for _, class := range []*string{nil, ptr.To("nginx")} {
		cn := grpcChainNode(class)
		r := cosmoGuardTestReconciler(t, cn, apiBackend("node-0", selector, chainutils.GrpcPort, false))
		require.NoError(t, r.ensureIngresses(ctx, cn))

		ing := getIngress(t, r, "node-0-grpc")
		assert.Equal(t, map[string]string{"nginx.ingress.kubernetes.io/backend-protocol": "GRPC"}, ing.Annotations)
		assert.Equal(t, "nginx", *ing.Spec.IngressClassName)
		assert.Equal(t, "node-0-grpc", ingressBackend(ing).Name)
		assert.Equal(t, int32(chainutils.GrpcPort), ingressBackend(ing).Port.Number)

		svc := getService(t, r, "node-0-grpc")
		assert.Empty(t, svc.Annotations)
		assert.Equal(t, selector, svc.Spec.Selector)
		assert.Equal(t, intstr.FromInt32(chainutils.GrpcPort), svc.Spec.Ports[0].TargetPort)
	}

	// grpcAnnotations still replaces the nginx default on the Ingress.
	cn := grpcChainNode(nil)
	cn.Spec.Ingress.GrpcAnnotations = map[string]string{"custom": "yes"}
	r := cosmoGuardTestReconciler(t, cn, apiBackend("node-0", selector, chainutils.GrpcPort, false))
	require.NoError(t, r.ensureIngresses(ctx, cn))
	assert.Equal(t, map[string]string{"custom": "yes"}, getIngress(t, r, "node-0-grpc").Annotations)
}

func TestGrpcServiceMirrorsInternalBackend(t *testing.T) {
	ctx := context.Background()
	cn := grpcChainNode(ptr.To("traefik"))
	cn.Spec.Ingress.UseInternalServices = ptr.To(true)
	selector := map[string]string{"app": "node-0"}
	r := cosmoGuardTestReconciler(t, cn, apiBackend("node-0-internal", selector, chainutils.GrpcPort, true))

	require.NoError(t, r.ensureIngresses(ctx, cn))

	svc := getService(t, r, "node-0-grpc")
	assert.Equal(t, selector, svc.Spec.Selector)
	assert.True(t, svc.Spec.PublishNotReadyAddresses)
}

func TestGrpcServiceMirrorsServingGuard(t *testing.T) {
	ctx := context.Background()
	cn := guardedChainNode("node-0", false)
	cn.Spec.Ingress = &appsv1.IngressConfig{Host: "example.com", EnableGRPC: true, IngressClass: ptr.To("traefik")}
	guardSelector := cosmoguard.InstanceLabels("node-0-cg")
	r := cosmoGuardTestReconciler(t, cn, servingGuard("node-0-cg"),
		apiBackend("node-0-cg", guardSelector, controllers.CosmoGuardGrpcPort, false))

	require.NoError(t, r.ensureIngresses(ctx, cn))

	svc := getService(t, r, "node-0-grpc")
	assert.Equal(t, guardSelector, svc.Spec.Selector)
	assert.Equal(t, intstr.FromInt32(controllers.CosmoGuardGrpcPort), svc.Spec.Ports[0].TargetPort)
	assert.Equal(t, "h2c", svc.Annotations[appsv1.TraefikServersSchemeAnnotation])
}

func TestGrpcServiceMissingBackendFails(t *testing.T) {
	cn := grpcChainNode(nil)
	r := cosmoGuardTestReconciler(t, cn)
	require.Error(t, r.ensureIngresses(context.Background(), cn), "must requeue rather than point the Ingress at nothing")
}

func TestGrpcServiceAnnotationFollowsIngressClass(t *testing.T) {
	ctx := context.Background()
	cn := grpcChainNode(ptr.To("traefik"))
	r := cosmoGuardTestReconciler(t, cn, apiBackend("node-0", map[string]string{"app": "node-0"}, chainutils.GrpcPort, false))
	require.NoError(t, r.ensureIngresses(ctx, cn))
	require.Contains(t, getService(t, r, "node-0-grpc").Annotations, appsv1.TraefikServersSchemeAnnotation)

	cn.Spec.Ingress.IngressClass = ptr.To("nginx")
	require.NoError(t, r.ensureIngresses(ctx, cn))
	assert.NotContains(t, getService(t, r, "node-0-grpc").Annotations, appsv1.TraefikServersSchemeAnnotation)
}

func TestGrpcServiceCleanup(t *testing.T) {
	ctx := context.Background()
	backend := apiBackend("node-0", map[string]string{"app": "node-0"}, chainutils.GrpcPort, false)

	t.Run("gRPC disabled", func(t *testing.T) {
		cn := grpcChainNode(nil)
		r := cosmoGuardTestReconciler(t, cn, backend.DeepCopy())
		require.NoError(t, r.ensureIngresses(ctx, cn))
		getService(t, r, "node-0-grpc")

		cn.Spec.Ingress.EnableGRPC = false
		require.NoError(t, r.ensureIngresses(ctx, cn))
		requireGone(t, r, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "node-0-grpc", Namespace: "ns"}})
		requireGone(t, r, &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "node-0-grpc", Namespace: "ns"}})
		requireExists(t, r, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "node-0", Namespace: "ns"}})
	})

	t.Run("ingress removed", func(t *testing.T) {
		cn := grpcChainNode(nil)
		r := cosmoGuardTestReconciler(t, cn, backend.DeepCopy())
		require.NoError(t, r.ensureIngresses(ctx, cn))

		cn.Spec.Ingress = nil
		require.NoError(t, r.ensureIngresses(ctx, cn))
		requireGone(t, r, &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "node-0-grpc", Namespace: "ns"}})
	})

	t.Run("foreign service kept", func(t *testing.T) {
		cn := grpcChainNode(nil)
		cn.Spec.Ingress.EnableGRPC = false
		foreign := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "ns", UID: "other-uid"}}
		r := cosmoGuardTestReconciler(t, cn, backend.DeepCopy())
		svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "node-0-grpc", Namespace: "ns"}}
		require.NoError(t, controllerutil.SetControllerReference(foreign, svc, r.Scheme))
		require.NoError(t, r.Create(ctx, svc))

		require.NoError(t, r.ensureIngresses(ctx, cn))
		requireExists(t, r, svc)
	})
}

// TestStickyFlipViaGrpcOnlyService verifies the sticky guard check follows the gRPC Ingress through
// the gRPC-only Service: it counts only when that Service selects guard pods.
func TestStickyFlipViaGrpcOnlyService(t *testing.T) {
	ctx := context.Background()
	cn := guardedChainNode("node-0", false)
	cn.Spec.Ingress = &appsv1.IngressConfig{Host: "example.com", EnableGRPC: true}

	grpcSvc := func(selector map[string]string) *corev1.Service {
		svc := apiBackend("node-0-grpc", selector, chainutils.GrpcPort, false)
		svc.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNode", Name: cn.Name, UID: cn.UID, Controller: ptr.To(true),
		}}
		return svc
	}

	// The gRPC-only Service selects the guard -> sticky keeps routes on the guard.
	r := cosmoGuardTestReconciler(t, guardIngress("node-0-grpc", "node-0-grpc"), grpcSvc(cosmoguard.InstanceLabels("node-0-cg")))
	assert.Equal(t, "node-0-cg", r.apiServiceName(ctx, cn))

	// The gRPC-only Service selects the raw node -> no flip.
	r = cosmoGuardTestReconciler(t, guardIngress("node-0-grpc", "node-0-grpc"), grpcSvc(map[string]string{"app": "node-0"}))
	assert.Equal(t, "node-0", r.apiServiceName(ctx, cn))

	// A guard-selecting Service this node does not control is not evidence of a flip.
	foreign := grpcSvc(cosmoguard.InstanceLabels("node-0-cg"))
	foreign.OwnerReferences = nil
	r = cosmoGuardTestReconciler(t, guardIngress("node-0-grpc", "node-0-grpc"), foreign)
	assert.Equal(t, "node-0", r.apiServiceName(ctx, cn))
}

// TestGatewayGrpcRouteUnchanged verifies the Gateway API path keeps targeting the API Service and
// creates no gRPC-only Service.
func TestGatewayGrpcRouteUnchanged(t *testing.T) {
	ctx := context.Background()
	cn := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "node-0", Namespace: "ns", UID: "node-0-uid"},
		Spec: appsv1.ChainNodeSpec{
			Gateway: &appsv1.GatewayConfig{Host: "example.com", EnableGRPC: true},
		},
	}
	r := cosmoGuardTestReconciler(t, cn, apiBackend("node-0", map[string]string{"app": "node-0"}, chainutils.GrpcPort, false))

	require.NoError(t, r.ensureGatewayRoutes(ctx, cn))

	routes := &gwapiv1.GRPCRouteList{}
	require.NoError(t, r.List(ctx, routes, client.InNamespace("ns")))
	require.Len(t, routes.Items, 1)
	ref := routes.Items[0].Spec.Rules[0].BackendRefs[0]
	assert.Equal(t, gwapiv1.ObjectName("node-0"), ref.Name)
	assert.Equal(t, gwapiv1.PortNumber(chainutils.GrpcPort), *ref.Port)

	err := r.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "node-0-grpc"}, &corev1.Service{})
	assert.True(t, client.IgnoreNotFound(err) == nil && err != nil, "gateway path must not create a gRPC-only Service")
}

// TestFinalizeDefersUndeployWhileGrpcServiceSelectsGuard verifies a disabled guard is kept while the
// gRPC Ingress still reaches it through the gRPC-only Service, and torn down once that Service no
// longer selects guard pods.
func TestFinalizeDefersUndeployWhileGrpcServiceSelectsGuard(t *testing.T) {
	ctx := context.Background()
	cn := guardedChainNode("node-0", false)
	r := cosmoGuardTestReconciler(t, cn)
	require.NoError(t, ensureGuard(r, ctx, cn))

	cn.Spec.Config.CosmoGuard.Enable = false
	grpcSvc := apiBackend("node-0-grpc", cosmoguard.InstanceLabels("node-0-cg"), controllers.CosmoGuardGrpcPort, false)
	require.NoError(t, controllerutil.SetControllerReference(cn, grpcSvc, r.Scheme))
	require.NoError(t, r.Create(ctx, grpcSvc))
	require.NoError(t, r.Create(ctx, guardIngress("node-0-grpc", "node-0-grpc")))

	require.NoError(t, r.finalizeCosmoGuard(ctx, cn, true))
	require.NoError(t, r.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "node-0-cg"}, &k8sappsv1.StatefulSet{}),
		"guard must survive while the gRPC-only Service still selects it")

	grpcSvc.Spec.Selector = map[string]string{"app": "node-0"}
	require.NoError(t, r.Update(ctx, grpcSvc))
	require.NoError(t, r.finalizeCosmoGuard(ctx, cn, true))
	err := r.Get(ctx, client.ObjectKey{Namespace: "ns", Name: "node-0-cg"}, &k8sappsv1.StatefulSet{})
	assert.Error(t, err, "guard torn down once the gRPC-only Service no longer selects it")
}

// routesUnavailableClient reports the Gateway API CRDs as missing for the API route kinds.
type routesUnavailableClient struct {
	client.Client
}

func routeKindMissing(obj runtime.Object) error {
	switch obj.(type) {
	case *gwapiv1.HTTPRoute, *gwapiv1.HTTPRouteList, *gwapiv1.GRPCRoute, *gwapiv1.GRPCRouteList:
		return &meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: gwapiv1.GroupVersion.Group, Kind: "GRPCRoute"}}
	}
	return nil
}

func (c routesUnavailableClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if err := routeKindMissing(obj); err != nil {
		return err
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func (c routesUnavailableClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if err := routeKindMissing(list); err != nil {
		return err
	}
	return c.Client.List(ctx, list, opts...)
}

func (c routesUnavailableClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	if err := routeKindMissing(obj); err != nil {
		return err
	}
	return c.Client.Create(ctx, obj, opts...)
}

// TestSwitchToGatewayRemovesGrpcService verifies moving from Ingress to Gateway API removes the gRPC
// Ingress and its gRPC-only Service once the routes apply, and keeps both while they cannot.
func TestSwitchToGatewayRemovesGrpcService(t *testing.T) {
	ctx := context.Background()
	backend := apiBackend("node-0", map[string]string{"app": "node-0"}, chainutils.GrpcPort, false)
	grpcSvc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "node-0-grpc", Namespace: "ns"}}
	grpcIng := &networkingv1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: "node-0-grpc", Namespace: "ns"}}
	toGateway := func(cn *appsv1.ChainNode) {
		cn.Spec.Ingress = nil
		cn.Spec.Gateway = &appsv1.GatewayConfig{Host: "example.com", EnableGRPC: true}
	}

	t.Run("routes applied", func(t *testing.T) {
		cn := grpcChainNode(nil)
		r := cosmoGuardTestReconciler(t, cn, backend.DeepCopy())
		require.NoError(t, r.ensureIngresses(ctx, cn))
		requireExists(t, r, grpcSvc.DeepCopy())

		toGateway(cn)
		require.NoError(t, r.ensureGatewayRoutes(ctx, cn))
		requireGone(t, r, grpcSvc.DeepCopy())
		requireGone(t, r, grpcIng.DeepCopy())
	})

	t.Run("gateway API missing", func(t *testing.T) {
		cn := grpcChainNode(nil)
		r := cosmoGuardTestReconciler(t, cn, backend.DeepCopy())
		require.NoError(t, r.ensureIngresses(ctx, cn))

		toGateway(cn)
		r.Client = routesUnavailableClient{Client: r.Client}
		require.NoError(t, r.ensureGatewayRoutes(ctx, cn))
		requireExists(t, r, grpcSvc.DeepCopy())
		requireExists(t, r, grpcIng.DeepCopy())
	})
}

func TestGrpcServiceSteadyStateNoChurn(t *testing.T) {
	ctx := context.Background()
	for _, class := range []*string{nil, ptr.To("traefik")} {
		cn := grpcChainNode(class)
		r := cosmoGuardTestReconciler(t, cn, apiBackend("node-0", map[string]string{"app": "node-0"}, chainutils.GrpcPort, false))
		require.NoError(t, r.ensureIngresses(ctx, cn))
		before := getService(t, r, "node-0-grpc").ResourceVersion
		require.NoError(t, r.ensureIngresses(ctx, cn))
		assert.Equal(t, before, getService(t, r, "node-0-grpc").ResourceVersion)
	}
}
