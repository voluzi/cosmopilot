package chainnode

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"
	gwapiv1a2 "sigs.k8s.io/gateway-api/apis/v1alpha2"

	appsv1 "github.com/voluzi/cosmopilot/v3/api/v1"
	"github.com/voluzi/cosmopilot/v3/internal/controllers"
)

func routingOwnershipReconciler(t *testing.T) (*Reconciler, *appsv1.ChainNode, *appsv1.ChainNode) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, networkingv1.AddToScheme(scheme))
	require.NoError(t, gwapiv1.Install(scheme))
	require.NoError(t, gwapiv1a2.Install(scheme))
	owner := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: "node", Namespace: "default", UID: "owner-uid"}}
	foreign := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "default", UID: "foreign-uid"}}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	return &Reconciler{Client: c, Scheme: scheme}, owner, foreign
}

func controlled[T client.Object](t *testing.T, r *Reconciler, obj T, owner metav1.Object) T {
	t.Helper()
	if owner != nil {
		require.NoError(t, controllerutil.SetControllerReference(owner, obj, r.Scheme))
	}
	require.NoError(t, r.Create(context.Background(), obj))
	return obj
}

func requireExists(t *testing.T, r *Reconciler, obj client.Object) {
	t.Helper()
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(obj), obj),
		"%T %s must not be deleted", obj, obj.GetName())
}

func requireGone(t *testing.T, r *Reconciler, obj client.Object) {
	t.Helper()
	err := r.Get(context.Background(), client.ObjectKeyFromObject(obj), obj)
	require.True(t, errors.IsNotFound(err), "%T %s must be deleted, got %v", obj, obj.GetName(), err)
}

func routeMeta(name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: "default"}
}

func TestIngressDisabledCleanupKeepsIngressesItDoesNotControl(t *testing.T) {
	r, owner, foreign := routingOwnershipReconciler(t)
	api := controlled(t, r, &networkingv1.Ingress{ObjectMeta: routeMeta("node")}, foreign)
	grpc := controlled(t, r, &networkingv1.Ingress{ObjectMeta: routeMeta("node-grpc")}, nil)

	require.NoError(t, r.ensureIngresses(context.Background(), owner))
	requireExists(t, r, api)
	requireExists(t, r, grpc)
}

func TestIngressDisabledCleanupDeletesOwnedIngresses(t *testing.T) {
	r, owner, _ := routingOwnershipReconciler(t)
	api := controlled(t, r, &networkingv1.Ingress{ObjectMeta: routeMeta("node")}, owner)
	grpc := controlled(t, r, &networkingv1.Ingress{ObjectMeta: routeMeta("node-grpc")}, owner)

	require.NoError(t, r.ensureIngresses(context.Background(), owner))
	requireGone(t, r, api)
	requireGone(t, r, grpc)
}

func TestEnsureIngressRefusesForeignIngress(t *testing.T) {
	r, owner, foreign := routingOwnershipReconciler(t)
	current := &networkingv1.Ingress{ObjectMeta: routeMeta("node")}
	current.Labels = map[string]string{"app": "foreign"}
	controlled(t, r, current, foreign)

	desired := &networkingv1.Ingress{ObjectMeta: routeMeta("node")}
	desired.Labels = map[string]string{"app": "node"}
	require.NoError(t, controllerutil.SetControllerReference(owner, desired, r.Scheme))

	require.ErrorContains(t, r.ensureIngress(context.Background(), desired), "managed by another owner")
	live := &networkingv1.Ingress{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(current), live))
	require.Equal(t, "foreign", live.Labels["app"])
	require.Equal(t, foreign.UID, metav1.GetControllerOf(live).UID)
}

func TestGatewayCleanupKeepsRoutesItDoesNotControl(t *testing.T) {
	r, owner, foreign := routingOwnershipReconciler(t)
	routeLabels := map[string]string{controllers.LabelChainNode: owner.Name, labelGatewayRoute: "true"}
	foreignHTTP := &gwapiv1.HTTPRoute{ObjectMeta: routeMeta("node-rpc")}
	foreignHTTP.Labels = routeLabels
	controlled(t, r, foreignHTTP, foreign)
	ownedHTTP := &gwapiv1.HTTPRoute{ObjectMeta: routeMeta("node-api")}
	ownedHTTP.Labels = routeLabels
	controlled(t, r, ownedHTTP, owner)
	foreignGRPC := controlled(t, r, &gwapiv1.GRPCRoute{ObjectMeta: routeMeta("node-grpc")}, foreign)
	foreignTCP := controlled(t, r, &gwapiv1a2.TCPRoute{ObjectMeta: routeMeta("node-p2p")}, nil)

	require.NoError(t, r.cleanupGatewayRoutes(context.Background(), owner))
	require.NoError(t, r.cleanupTCPRoute(context.Background(), owner))
	requireExists(t, r, foreignHTTP)
	requireGone(t, r, ownedHTTP)
	requireExists(t, r, foreignGRPC)
	requireExists(t, r, foreignTCP)
}

func TestGatewayCleanupDeletesOwnedRoutes(t *testing.T) {
	r, owner, _ := routingOwnershipReconciler(t)
	grpc := controlled(t, r, &gwapiv1.GRPCRoute{ObjectMeta: routeMeta("node-grpc")}, owner)
	tcp := controlled(t, r, &gwapiv1a2.TCPRoute{ObjectMeta: routeMeta("node-p2p")}, owner)

	require.NoError(t, r.cleanupGatewayRoutes(context.Background(), owner))
	require.NoError(t, r.cleanupTCPRoute(context.Background(), owner))
	requireGone(t, r, grpc)
	requireGone(t, r, tcp)
}
