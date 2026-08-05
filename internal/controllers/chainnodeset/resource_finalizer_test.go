package chainnodeset

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	k8sappsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
	"github.com/voluzi/cosmopilot/v2/internal/cosmosigner"
	"github.com/voluzi/cosmopilot/v2/internal/resourcecleanup"
)

func TestReconcileInstallsResourceCleanupFinalizerBeforeGeneratingChildren(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, k8sappsv1.AddToScheme(scheme))
	nodeSet := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: "default", UID: "set-uid"}}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nodeSet, namespace).Build()
	r := &Reconciler{Client: c, Scheme: scheme, opts: &controllers.ControllerRunOptions{}}

	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(nodeSet)})
	require.NoError(t, err)
	assert.True(t, result.Requeue)

	current := &appsv1.ChainNodeSet{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(nodeSet), current))
	assert.Contains(t, current.Finalizers, resourcecleanup.Finalizer)
	assert.Contains(t, current.Finalizers, podDisruptionBudgetFinalizer)
	children := &appsv1.ChainNodeList{}
	require.NoError(t, c.List(context.Background(), children, client.InNamespace("default")))
	assert.Empty(t, children.Items)
}

func TestReconcileDoesNotFinalizeDeletingChainNodeSetAssignedToAnotherWorker(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, k8sappsv1.AddToScheme(scheme))
	now := metav1.NewTime(time.Now())
	nodeSet := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{
		Name: "set", Namespace: "default", UID: "set-uid", DeletionTimestamp: &now,
		Labels:     map[string]string{controllers.LabelWorkerName: "worker-b"},
		Finalizers: []string{resourcecleanup.Finalizer, podDisruptionBudgetFinalizer},
	}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nodeSet).Build()
	r := &Reconciler{Client: c, Scheme: scheme, opts: &controllers.ControllerRunOptions{WorkerName: "worker-a"}}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(nodeSet)})
	require.NoError(t, err)
	current := &appsv1.ChainNodeSet{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(nodeSet), current))
	assert.Contains(t, current.Finalizers, resourcecleanup.Finalizer)
	assert.Contains(t, current.Finalizers, podDisruptionBudgetFinalizer)
}

func TestReconcileKeepsReservationUntilPodDisruptionBudgetCleanupConverges(t *testing.T) {
	scheme := nodeSetCleanupScheme(t)
	require.NoError(t, batchv1.AddToScheme(scheme))
	now := metav1.NewTime(time.Now())
	nodeSet := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{
		Name: "set", Namespace: "default", UID: "set-uid", DeletionTimestamp: &now,
		Finalizers: []string{resourcecleanup.Finalizer, podDisruptionBudgetFinalizer, cosmosigner.ReservationOwnerFinalizer},
	}}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nodeSet.Namespace}}
	reservation := &appsv1.ConsensusKeyReservation{
		ObjectMeta: metav1.ObjectMeta{Name: cosmosigner.ConsensusKeyReservationName("chain-1", nodeSetReservationLifecyclePublicKey), UID: "ckr-uid"},
		Spec: appsv1.ConsensusKeyReservationSpec{
			ChainID: "chain-1", PublicKey: nodeSetReservationLifecyclePublicKey,
			OwnerUID: nodeSet.UID, OwnerKind: "ChainNodeSet", Namespace: nodeSet.Namespace, OwnerName: nodeSet.Name, Claim: "set-validator",
		},
	}
	pdb := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{
		Name: "set-pdb", Namespace: nodeSet.Namespace, UID: "pdb-uid", Finalizers: []string{"test.voluzi.com/hold"},
	}}
	require.NoError(t, controllerutil.SetControllerReference(nodeSet, pdb, scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nodeSet, namespace, reservation, pdb).Build()
	r := &Reconciler{Client: c, APIReader: c, Scheme: scheme, opts: &controllers.ControllerRunOptions{}}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(nodeSet)})
	require.NoError(t, err)
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(reservation), &appsv1.ConsensusKeyReservation{}))
	freshPDB := &policyv1.PodDisruptionBudget{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(pdb), freshPDB))
	assert.False(t, freshPDB.GetDeletionTimestamp().IsZero())
}
