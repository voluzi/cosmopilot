package chainnode

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
)

func TestUpdatePvcDataHeightRetriesOnConflict(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name:        "node",
		Namespace:   "default",
		Annotations: map[string]string{controllers.AnnotationDataHeight: "100"},
	}}
	baseClient := fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(pvc).Build()
	conflicting := &conflictOnceClient{Client: baseClient}
	reconciler := &Reconciler{Client: conflicting, Scheme: scheme}

	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "node", Namespace: "default"},
		Status:     appsv1.ChainNodeStatus{LatestHeight: 200},
	}

	require.NoError(t, reconciler.updatePvcDataHeight(context.Background(), chainNode, pvc))
	assert.Equal(t, 1, conflicting.conflicts)

	stored := &corev1.PersistentVolumeClaim{}
	require.NoError(t, baseClient.Get(context.Background(), types.NamespacedName{Name: "node", Namespace: "default"}, stored))
	assert.Equal(t, "200", stored.Annotations[controllers.AnnotationDataHeight])
	// The caller's copy is refreshed so later writes in the same reconcile do not conflict on a stale version.
	assert.Equal(t, stored.ResourceVersion, pvc.ResourceVersion)
}

func TestUpdatePvcDataHeightSurfacesExhaustedConflicts(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name:        "node",
		Namespace:   "default",
		Annotations: map[string]string{controllers.AnnotationDataHeight: "100"},
	}}
	baseClient := fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(pvc).Build()
	reconciler := &Reconciler{Client: &pvcUpdateFailingClient{Client: baseClient}, Scheme: scheme}

	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "node", Namespace: "default"},
		Status:     appsv1.ChainNodeStatus{LatestHeight: 200},
	}

	err := reconciler.updatePvcDataHeight(context.Background(), chainNode, pvc)
	require.Error(t, err)
	assert.True(t, apierrors.IsConflict(err))
}

// conflictOnceClient rejects the first Update with a conflict, mimicking a concurrent write to the PVC.
type conflictOnceClient struct {
	client.Client
	conflicts int
}

func (c *conflictOnceClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if c.conflicts == 0 {
		c.conflicts++
		return apierrors.NewConflict(
			schema.GroupResource{Resource: "persistentvolumeclaims"},
			obj.GetName(),
			assert.AnError,
		)
	}
	return c.Client.Update(ctx, obj, opts...)
}

// pvcUpdateFailingClient rejects every PVC Update with a conflict, so retries are exhausted.
type pvcUpdateFailingClient struct {
	client.Client
}

func (c *pvcUpdateFailingClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if _, ok := obj.(*corev1.PersistentVolumeClaim); ok {
		return apierrors.NewConflict(
			schema.GroupResource{Resource: "persistentvolumeclaims"},
			obj.GetName(),
			assert.AnError,
		)
	}
	return c.Client.Update(ctx, obj, opts...)
}
