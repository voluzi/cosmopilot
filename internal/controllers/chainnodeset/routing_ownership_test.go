package chainnodeset

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	k8sappsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"
	gwapiv1a2 "sigs.k8s.io/gateway-api/apis/v1alpha2"

	appsv1 "github.com/voluzi/cosmopilot/v3/api/v1"
	"github.com/voluzi/cosmopilot/v3/internal/controllers"
)

func routingOwnershipReconciler(t *testing.T) (*Reconciler, *appsv1.ChainNodeSet, *appsv1.ChainNodeSet, *appsv1.ChainNodeSet) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, k8sappsv1.AddToScheme(scheme))
	require.NoError(t, networkingv1.AddToScheme(scheme))
	require.NoError(t, gwapiv1.Install(scheme))
	require.NoError(t, gwapiv1a2.Install(scheme))
	owner := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{Name: "ns", Namespace: "default", UID: "owner-uid"}}
	// A ChainNodeSet with the same name in another namespace carries the same selector labels.
	twin := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{Name: "ns", Namespace: "other", UID: "twin-uid"}}
	foreign := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: "default", UID: "foreign-uid"}}
	return &Reconciler{
		Client:   fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&appsv1.ChainNodeSet{}).Build(),
		Scheme:   scheme,
		recorder: record.NewFakeRecorder(100),
		opts:     &controllers.ControllerRunOptions{},
	}, owner, twin, foreign
}

func createControlled[T client.Object](t *testing.T, r *Reconciler, obj T, owner metav1.Object) T {
	t.Helper()
	if owner != nil {
		require.NoError(t, controllerutil.SetControllerReference(owner, obj, r.Scheme))
	}
	require.NoError(t, r.Create(context.Background(), obj))
	return obj
}

func requireKept(t *testing.T, r *Reconciler, obj client.Object) {
	t.Helper()
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(obj), obj),
		"%T %s/%s must not be deleted", obj, obj.GetNamespace(), obj.GetName())
}

func requireDeleted(t *testing.T, r *Reconciler, obj client.Object) {
	t.Helper()
	err := r.Get(context.Background(), client.ObjectKeyFromObject(obj), obj)
	require.True(t, errors.IsNotFound(err), "%T %s/%s must be deleted, got %v", obj, obj.GetNamespace(), obj.GetName(), err)
}

func routingObjectMeta(namespace, name string, labels map[string]string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: labels}
}

func TestGlobalIngressCleanupOnlyDeletesOwnedIngresses(t *testing.T) {
	r, owner, twin, _ := routingOwnershipReconciler(t)
	global := map[string]string{controllers.LabelChainNodeSet: "ns", controllers.LabelScope: scopeGlobal}
	group := map[string]string{controllers.LabelChainNodeSet: "ns", controllers.LabelScope: scopeGroup}

	ownedGlobal := createControlled(t, r, &networkingv1.Ingress{ObjectMeta: routingObjectMeta("default", "ns-global-old", global)}, owner)
	ownedGroup := createControlled(t, r, &networkingv1.Ingress{ObjectMeta: routingObjectMeta("default", "ns-group-old", group)}, owner)
	unowned := createControlled(t, r, &networkingv1.Ingress{ObjectMeta: routingObjectMeta("default", "user-ingress", global)}, nil)
	twinGlobal := createControlled(t, r, &networkingv1.Ingress{ObjectMeta: routingObjectMeta("other", "ns-global-live", global)}, twin)
	twinGroup := createControlled(t, r, &networkingv1.Ingress{ObjectMeta: routingObjectMeta("other", "ns-group-live", group)}, twin)

	require.NoError(t, r.ensureIngresses(context.Background(), owner, true))
	requireDeleted(t, r, ownedGlobal)
	requireDeleted(t, r, ownedGroup)
	requireKept(t, r, unowned)
	requireKept(t, r, twinGlobal)
	requireKept(t, r, twinGroup)
}

func TestServiceCleanupOnlyDeletesOwnedServices(t *testing.T) {
	r, owner, twin, foreign := routingOwnershipReconciler(t)
	group := map[string]string{
		controllers.LabelChainNodeSet: "ns", controllers.LabelScope: scopeGroup, controllers.LabelChainNodeSetGroup: "gone",
	}
	global := map[string]string{
		controllers.LabelChainNodeSet: "ns", controllers.LabelScope: scopeGlobal, controllers.LabelGlobalIngress: "gone",
	}

	ownedGroup := createControlled(t, r, &corev1.Service{ObjectMeta: routingObjectMeta("default", "ns-gone", group)}, owner)
	ownedGlobal := createControlled(t, r, &corev1.Service{ObjectMeta: routingObjectMeta("default", "ns-global-gone", global)}, owner)
	foreignSvc := createControlled(t, r, &corev1.Service{ObjectMeta: routingObjectMeta("default", "user-svc", group)}, foreign)
	twinGroup := createControlled(t, r, &corev1.Service{ObjectMeta: routingObjectMeta("other", "ns-gone", group)}, twin)
	twinGlobal := createControlled(t, r, &corev1.Service{ObjectMeta: routingObjectMeta("other", "ns-global-gone", global)}, twin)

	require.NoError(t, r.ensureServices(context.Background(), owner, cosmoGuardReconcile{}))
	requireDeleted(t, r, ownedGroup)
	requireDeleted(t, r, ownedGlobal)
	requireKept(t, r, foreignSvc)
	requireKept(t, r, twinGroup)
	requireKept(t, r, twinGlobal)
}

func TestSeedCleanupOnlyDeletesOwnedResources(t *testing.T) {
	r, owner, _, foreign := routingOwnershipReconciler(t)
	seed := map[string]string{controllers.LabelApp: controllers.CosmoseedName, controllers.LabelChainNodeSet: "ns"}

	foreignSet := createControlled(t, r, &k8sappsv1.StatefulSet{ObjectMeta: routingObjectMeta("default", "ns-seed", nil)}, foreign)
	foreignIngress := createControlled(t, r, &networkingv1.Ingress{ObjectMeta: routingObjectMeta("default", "ns-seed", nil)}, nil)
	ownedRoute := createControlled(t, r, &gwapiv1.HTTPRoute{ObjectMeta: routingObjectMeta("default", "ns-seed", nil)}, owner)
	ownedTCP := createControlled(t, r, &gwapiv1a2.TCPRoute{ObjectMeta: routingObjectMeta("default", "ns-seed-0-p2p", seed)}, owner)
	foreignTCP := createControlled(t, r, &gwapiv1a2.TCPRoute{ObjectMeta: routingObjectMeta("default", "user-p2p", seed)}, nil)
	ownedSvc := createControlled(t, r, &corev1.Service{ObjectMeta: routingObjectMeta("default", "ns-seed-0", seed)}, owner)
	foreignSvc := createControlled(t, r, &corev1.Service{ObjectMeta: routingObjectMeta("default", "user-seed", seed)}, foreign)

	require.NoError(t, r.maybeCleanupSeedNodes(context.Background(), owner))
	requireKept(t, r, foreignSet)
	requireKept(t, r, foreignIngress)
	requireDeleted(t, r, ownedRoute)
	requireDeleted(t, r, ownedTCP)
	requireKept(t, r, foreignTCP)
	requireDeleted(t, r, ownedSvc)
	requireKept(t, r, foreignSvc)
}

func TestSeedStatefulSetRefusesForeignStatefulSet(t *testing.T) {
	r, owner, _, foreign := routingOwnershipReconciler(t)
	current := &k8sappsv1.StatefulSet{ObjectMeta: routingObjectMeta("default", "ns-seed", map[string]string{"app": "foreign"})}
	createControlled(t, r, current, foreign)

	desired := &k8sappsv1.StatefulSet{ObjectMeta: routingObjectMeta("default", "ns-seed", map[string]string{"app": "seed"})}
	require.NoError(t, controllerutil.SetControllerReference(owner, desired, r.Scheme))

	require.ErrorContains(t, r.ensureStatefulSet(context.Background(), desired), "managed by another owner")
	live := &k8sappsv1.StatefulSet{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(current), live))
	require.Equal(t, "foreign", live.Labels["app"])
	require.Equal(t, foreign.UID, metav1.GetControllerOf(live).UID)
}
