package chainnode

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	k8sappsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	appsv1 "github.com/voluzi/cosmopilot/v3/api/v1"
	"github.com/voluzi/cosmopilot/v3/internal/controllers"
	"github.com/voluzi/cosmopilot/v3/internal/cosmosigner"
	"github.com/voluzi/cosmopilot/v3/internal/resourcecleanup"
)

func TestReconcileInstallsResourceCleanupFinalizerBeforeDurableCreation(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, k8sappsv1.AddToScheme(scheme))
	node := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: "node", Namespace: "default", UID: "node-uid"}}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node, namespace).Build()
	r := &Reconciler{Client: c, Scheme: scheme, opts: &controllers.ControllerRunOptions{}}

	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(node)})
	require.NoError(t, err)
	assert.True(t, result.Requeue)

	current := &appsv1.ChainNode{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(node), current))
	assert.Contains(t, current.Finalizers, resourcecleanup.Finalizer)
	secrets := &corev1.SecretList{}
	require.NoError(t, c.List(context.Background(), secrets, client.InNamespace("default")))
	assert.Empty(t, secrets.Items)
}

func TestReconcileDoesNotFinalizeDeletingChainNodeAssignedToAnotherWorker(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, k8sappsv1.AddToScheme(scheme))
	now := metav1.NewTime(time.Now())
	node := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{
		Name: "node", Namespace: "default", UID: "node-uid", DeletionTimestamp: &now,
		Labels: map[string]string{controllers.LabelWorkerName: "worker-b"}, Finalizers: []string{resourcecleanup.Finalizer},
	}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()
	r := &Reconciler{Client: c, Scheme: scheme, opts: &controllers.ControllerRunOptions{WorkerName: "worker-a"}}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(node)})
	require.NoError(t, err)
	current := &appsv1.ChainNode{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(node), current))
	assert.Contains(t, current.Finalizers, resourcecleanup.Finalizer)
}

func TestReconcileKeepsReservationUntilResourceCleanupConverges(t *testing.T) {
	scheme := resourceCleanupScheme(t)
	require.NoError(t, batchv1.AddToScheme(scheme))
	now := metav1.NewTime(time.Now())
	deletePolicy := appsv1.DeletionPolicyDelete
	node := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{
			Name: "validator", Namespace: "default", UID: "node-uid", DeletionTimestamp: &now,
			Finalizers: []string{resourcecleanup.Finalizer, cosmosigner.ReservationOwnerFinalizer},
		},
		Spec: appsv1.ChainNodeSpec{DeletionPolicy: &appsv1.DeletionPolicy{GeneratedKeys: &deletePolicy}},
	}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: node.Namespace}}
	reservation := &appsv1.ConsensusKeyReservation{
		ObjectMeta: metav1.ObjectMeta{Name: cosmosigner.ConsensusKeyReservationName("chain-1", reservationLifecyclePublicKey), UID: "ckr-uid"},
		Spec: appsv1.ConsensusKeyReservationSpec{
			ChainID: "chain-1", PublicKey: reservationLifecyclePublicKey,
			OwnerUID: node.UID, OwnerKind: "ChainNode", Namespace: node.Namespace, OwnerName: node.Name, Claim: node.Name,
		},
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: node.Name + "-priv-key", Namespace: node.Namespace, UID: "secret-uid", Finalizers: []string{"test.voluzi.com/hold"},
	}}
	resourcecleanup.Stamp(secret, resourcecleanup.RootOwnerFor(node), resourcecleanup.ClassGeneratedKeys)
	resourcecleanup.StampResourceOwner(secret, node.UID)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node, namespace, reservation, secret).Build()
	r := &Reconciler{Client: c, APIReader: c, Scheme: scheme, opts: &controllers.ControllerRunOptions{}}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(node)})
	require.NoError(t, err)
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(reservation), &appsv1.ConsensusKeyReservation{}))
	freshSecret := &corev1.Secret{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(secret), freshSecret))
	assert.False(t, freshSecret.GetDeletionTimestamp().IsZero())
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(node), &appsv1.ChainNode{}))
}
