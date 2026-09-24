package chainnodeset

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	appsv1 "github.com/voluzi/cosmopilot/v3/api/v1"
	"github.com/voluzi/cosmopilot/v3/internal/chainutils"
	"github.com/voluzi/cosmopilot/v3/internal/controllers"
	"github.com/voluzi/cosmopilot/v3/internal/cosmoguard"
)

func grpcNodeSet(class *string) *appsv1.ChainNodeSet {
	nodeSet, _ := guardedNodeSet()
	nodeSet.Spec.Ingresses = []appsv1.GlobalIngressConfig{{
		Name: "public", Groups: []string{"fullnodes"}, Host: "example.com",
		EnableRPC: true, EnableLCD: true, EnableGRPC: true, IngressClass: class,
	}}
	return nodeSet
}

// reconcileRouting runs the Service and Ingress passes in controller order.
func reconcileRouting(t *testing.T, r *Reconciler, nodeSet *appsv1.ChainNodeSet, guards cosmoGuardReconcile, gatewayApplied bool) {
	t.Helper()
	require.NoError(t, r.ensureServices(context.Background(), nodeSet, guards))
	require.NoError(t, r.ensureIngresses(context.Background(), nodeSet, gatewayApplied))
}

func svcByName(t *testing.T, r *Reconciler, name string) *corev1.Service {
	t.Helper()
	svc := &corev1.Service{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: name}, svc))
	return svc
}

func ingressByName(t *testing.T, r *Reconciler, name string) *networkingv1.Ingress {
	t.Helper()
	ing := &networkingv1.Ingress{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: name}, ing))
	return ing
}

func requireNoService(t *testing.T, r *Reconciler, name string) {
	t.Helper()
	err := r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: name}, &corev1.Service{})
	require.True(t, errors.IsNotFound(err), "service %s must not exist, got %v", name, err)
}

func grpcTargetPort(svc *corev1.Service) intstr.IntOrString {
	for _, p := range svc.Spec.Ports {
		if p.Port == chainutils.GrpcPort {
			return p.TargetPort
		}
	}
	return intstr.IntOrString{}
}

func TestGlobalGrpcIngressTargetsGrpcOnlyServiceWithH2CForTraefik(t *testing.T) {
	nodeSet := grpcNodeSet(ptr.To("traefik"))
	r := newValidatorTestReconciler(t, nodeSet)
	reconcileRouting(t, r, nodeSet, cosmoGuardReconcile{}, true)

	global := svcByName(t, r, "chain-global-public")
	svc := svcByName(t, r, "chain-global-public-grpc")
	assert.Equal(t, "h2c", svc.Annotations[appsv1.TraefikServersSchemeAnnotation])
	assert.Equal(t, scopeGlobalGrpc, svc.Labels[controllers.LabelScope])
	assert.Equal(t, global.Spec.Selector, svc.Spec.Selector)
	require.Len(t, svc.Spec.Ports, 1)
	assert.Equal(t, grpcTargetPort(global), svc.Spec.Ports[0].TargetPort)
	assert.True(t, metav1.IsControlledBy(svc, nodeSet))

	backend := ingressByName(t, r, "chain-global-public-grpc").Spec.Rules[0].HTTP.Paths[0].Backend.Service
	assert.Equal(t, "chain-global-public-grpc", backend.Name)
	assert.Equal(t, int32(chainutils.GrpcPort), backend.Port.Number)

	// RPC/LCD stay on the shared Services, which get no scheme annotation.
	for _, rule := range ingressByName(t, r, "chain-global-public").Spec.Rules {
		assert.Equal(t, "chain-global-public", rule.HTTP.Paths[0].Backend.Service.Name)
	}
	assert.NotContains(t, global.Annotations, appsv1.TraefikServersSchemeAnnotation)
	assert.NotContains(t, svcByName(t, r, "chain-global-public-internal").Annotations, appsv1.TraefikServersSchemeAnnotation)

	// Steady state: another pass neither deletes nor churns it.
	reconcileRouting(t, r, nodeSet, cosmoGuardReconcile{}, true)
	assert.Equal(t, svc.ResourceVersion, svcByName(t, r, "chain-global-public-grpc").ResourceVersion)
}

func TestGlobalGrpcIngressNginxBehaviourUnchanged(t *testing.T) {
	for _, class := range []*string{nil, ptr.To("nginx")} {
		nodeSet := grpcNodeSet(class)
		r := newValidatorTestReconciler(t, nodeSet)
		reconcileRouting(t, r, nodeSet, cosmoGuardReconcile{}, true)

		ing := ingressByName(t, r, "chain-global-public-grpc")
		assert.Equal(t, map[string]string{"nginx.ingress.kubernetes.io/backend-protocol": "GRPC"}, ing.Annotations)
		assert.Equal(t, "nginx", *ing.Spec.IngressClassName)
		assert.Equal(t, "chain-global-public-grpc", ing.Spec.Rules[0].HTTP.Paths[0].Backend.Service.Name)

		svc := svcByName(t, r, "chain-global-public-grpc")
		assert.NotContains(t, svc.Annotations, appsv1.TraefikServersSchemeAnnotation)
		assert.Equal(t, svcByName(t, r, "chain-global-public").Spec.Selector, svc.Spec.Selector)
		assert.Equal(t, intstr.FromInt32(chainutils.GrpcPort), svc.Spec.Ports[0].TargetPort)
	}

	nodeSet := grpcNodeSet(nil)
	nodeSet.Spec.Ingresses[0].GrpcAnnotations = map[string]string{"custom": "yes"}
	r := newValidatorTestReconciler(t, nodeSet)
	reconcileRouting(t, r, nodeSet, cosmoGuardReconcile{}, true)
	assert.Equal(t, map[string]string{"custom": "yes"}, ingressByName(t, r, "chain-global-public-grpc").Annotations)
}

func TestGlobalGrpcServiceMirrorsInternalBackend(t *testing.T) {
	nodeSet := grpcNodeSet(ptr.To("traefik"))
	nodeSet.Spec.Ingresses[0].UseInternalServices = ptr.To(true)
	r := newValidatorTestReconciler(t, nodeSet)
	reconcileRouting(t, r, nodeSet, cosmoGuardReconcile{}, true)

	internal := svcByName(t, r, "chain-global-public-internal")
	svc := svcByName(t, r, "chain-global-public-grpc")
	assert.Equal(t, internal.Spec.Selector, svc.Spec.Selector)
	assert.Equal(t, internal.Spec.PublishNotReadyAddresses, svc.Spec.PublishNotReadyAddresses)
}

// TestGlobalGrpcServiceFollowsGuardFlip verifies the gRPC-only Service tracks the global Service's
// CosmoGuard flip in the same pass, in both directions.
func TestGlobalGrpcServiceFollowsGuardFlip(t *testing.T) {
	nodeSet := grpcNodeSet(ptr.To("traefik"))
	r := newValidatorTestReconciler(t, nodeSet)

	reconcileRouting(t, r, nodeSet, cosmoGuardReconcile{}, true)
	assert.Equal(t, intstr.FromInt32(chainutils.GrpcPort), svcByName(t, r, "chain-global-public-grpc").Spec.Ports[0].TargetPort)

	reconcileRouting(t, r, nodeSet, cosmoGuardReconcile{fullyReady: map[string]bool{"fullnodes": true}}, true)
	svc := svcByName(t, r, "chain-global-public-grpc")
	assert.Equal(t, cosmoGuardRouteSelector("chain-global-public"), svc.Spec.Selector)
	assert.Equal(t, intstr.FromInt32(controllers.CosmoGuardGrpcPort), svc.Spec.Ports[0].TargetPort)

	// Guard disabled: the route reverts to raw node pods, and so does gRPC.
	nodeSet.Spec.Nodes[0].Config.CosmoGuard.Enable = false
	reconcileRouting(t, r, nodeSet, cosmoGuardReconcile{}, true)
	svc = svcByName(t, r, "chain-global-public-grpc")
	assert.Equal(t, svcByName(t, r, "chain-global-public").Spec.Selector, svc.Spec.Selector)
	assert.Equal(t, intstr.FromInt32(chainutils.GrpcPort), svc.Spec.Ports[0].TargetPort)
}

func TestGlobalGrpcServiceCleanup(t *testing.T) {
	cases := map[string]func(*appsv1.ChainNodeSet){
		"gRPC disabled":  func(ns *appsv1.ChainNodeSet) { ns.Spec.Ingresses[0].EnableGRPC = false },
		"route removed":  func(ns *appsv1.ChainNodeSet) { ns.Spec.Ingresses = nil },
		"services only":  func(ns *appsv1.ChainNodeSet) { ns.Spec.Ingresses[0].ServicesOnly = ptr.To(true) },
		"class switched": func(ns *appsv1.ChainNodeSet) { ns.Spec.Ingresses[0].IngressClass = ptr.To("nginx") },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			nodeSet := grpcNodeSet(ptr.To("traefik"))
			r := newValidatorTestReconciler(t, nodeSet)
			reconcileRouting(t, r, nodeSet, cosmoGuardReconcile{}, true)
			svcByName(t, r, "chain-global-public-grpc")

			mutate(nodeSet)
			reconcileRouting(t, r, nodeSet, cosmoGuardReconcile{}, true)
			if name == "class switched" {
				assert.NotContains(t, svcByName(t, r, "chain-global-public-grpc").Annotations, appsv1.TraefikServersSchemeAnnotation)
				return
			}
			requireNoService(t, r, "chain-global-public-grpc")
		})
	}
}

// TestGlobalGrpcServiceCleanupSkipsGlobalService verifies the gRPC cleanup never deletes a global
// API Service that shares the gRPC name (a route named "<route>-grpc").
func TestGlobalGrpcServiceCleanupSkipsGlobalService(t *testing.T) {
	nodeSet := grpcNodeSet(nil)
	nodeSet.Spec.Ingresses[0].EnableGRPC = false
	nodeSet.Spec.Ingresses = append(nodeSet.Spec.Ingresses, appsv1.GlobalIngressConfig{
		Name: "public-grpc", Groups: []string{"fullnodes"}, Host: "other.example.com", EnableRPC: true,
	})
	r := newValidatorTestReconciler(t, nodeSet)
	reconcileRouting(t, r, nodeSet, cosmoGuardReconcile{}, true)
	assert.Equal(t, scopeGlobal, svcByName(t, r, "chain-global-public-grpc").Labels[controllers.LabelScope])
}

// TestGlobalGrpcServiceKeptWhileGatewayMigrationPending verifies that moving a gRPC route to
// gatewayRoutes keeps the old gRPC Ingress and its Service until the routes apply, then removes both.
// The GRPCRoute keeps targeting the global API Service.
func TestGlobalGrpcServiceKeptWhileGatewayMigrationPending(t *testing.T) {
	nodeSet := grpcNodeSet(ptr.To("traefik"))
	r := newValidatorTestReconciler(t, nodeSet)
	reconcileRouting(t, r, nodeSet, cosmoGuardReconcile{}, true)

	nodeSet.Spec.Ingresses = nil
	nodeSet.Spec.GatewayRoutes = []appsv1.GlobalGatewayConfig{{
		Name: "public", Groups: []string{"fullnodes"}, Host: "example.com", EnableGRPC: true,
		Gateway: appsv1.GatewayRef{Name: "external"},
	}}

	reconcileRouting(t, r, nodeSet, cosmoGuardReconcile{}, false)
	ingressByName(t, r, "chain-global-public-grpc")
	svcByName(t, r, "chain-global-public-grpc")

	require.NoError(t, r.ensureServices(context.Background(), nodeSet, cosmoGuardReconcile{}))
	applied, err := r.ensureGatewayRoutes(context.Background(), nodeSet)
	require.NoError(t, err)
	require.True(t, applied)
	require.NoError(t, r.ensureIngresses(context.Background(), nodeSet, applied))
	requireNoService(t, r, "chain-global-public-grpc")
	err = r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "chain-global-public-grpc"}, &networkingv1.Ingress{})
	require.True(t, errors.IsNotFound(err))

	routes := &gwapiv1.GRPCRouteList{}
	require.NoError(t, r.List(context.Background(), routes, client.InNamespace("ns")))
	require.Len(t, routes.Items, 1)
	assert.Equal(t, gwapiv1.ObjectName("chain-global-public"), routes.Items[0].Spec.Rules[0].BackendRefs[0].Name)
}

// TestPreservedGrpcServiceFollowsGuardFlip verifies that while a route migrates to a same-named
// gateway route that cannot apply, the preserved gRPC Ingress's Service still follows a CosmoGuard
// flip of the backend and keeps its Traefik annotation.
func TestPreservedGrpcServiceFollowsGuardFlip(t *testing.T) {
	nodeSet := grpcNodeSet(ptr.To("traefik"))
	r := newValidatorTestReconciler(t, nodeSet)
	reconcileRouting(t, r, nodeSet, cosmoGuardReconcile{}, true)

	nodeSet.Spec.Ingresses = nil
	nodeSet.Spec.GatewayRoutes = []appsv1.GlobalGatewayConfig{{
		Name: "public", Groups: []string{"fullnodes"}, Host: "example.com", EnableGRPC: true,
		Gateway: appsv1.GatewayRef{Name: "external"},
	}}
	reconcileRouting(t, r, nodeSet, cosmoGuardReconcile{fullyReady: map[string]bool{"fullnodes": true}}, false)

	backend := svcByName(t, r, "chain-global-public")
	require.True(t, cosmoguard.SelectsGuard(backend.Spec.Selector), "backend must have flipped")
	svc := svcByName(t, r, "chain-global-public-grpc")
	assert.Equal(t, backend.Spec.Selector, svc.Spec.Selector)
	assert.Equal(t, intstr.FromInt32(controllers.CosmoGuardGrpcPort), svc.Spec.Ports[0].TargetPort)
	assert.Equal(t, "h2c", svc.Annotations[appsv1.TraefikServersSchemeAnnotation])
	assert.Equal(t, scopeGlobalGrpc, svc.Labels[controllers.LabelScope])
}
