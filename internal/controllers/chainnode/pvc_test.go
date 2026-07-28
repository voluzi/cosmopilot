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
	// APIReader mirrors production: retry reads bypass the cache so each attempt sees the current version.
	reconciler := &Reconciler{Client: conflicting, APIReader: baseClient, Scheme: scheme}

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

// TestUpdatePvcDataHeightLeavesCallerCopyStaleOnFailure documents why ensurePvcUpdates returns early
// when this fails: the caller's PVC still holds the resource version that just lost, so any further
// write in the same pass (notably the resize) would conflict again and abort the reconcile — the very
// failure this path exists to prevent.
func TestUpdatePvcDataHeightLeavesCallerCopyStaleOnFailure(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name:        "node",
		Namespace:   "default",
		Annotations: map[string]string{controllers.AnnotationDataHeight: "100"},
	}}
	baseClient := fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(pvc).Build()
	reconciler := &Reconciler{
		Client:    &pvcUpdateFailingClient{Client: baseClient},
		APIReader: baseClient,
		Scheme:    scheme,
	}
	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "node", Namespace: "default"},
		Status:     appsv1.ChainNodeStatus{LatestHeight: 200},
	}

	before := pvc.DeepCopy()
	require.Error(t, reconciler.updatePvcDataHeight(context.Background(), chainNode, pvc))
	assert.Equal(t, before.ResourceVersion, pvc.ResourceVersion,
		"a failed write must not refresh the caller's copy, so callers must not keep writing to it")
}

// TestUpdatePvcDataHeightReadsUncachedOnRetry models the production failure: a concurrent writer bumps
// the PVC at the API server, but the controller cache has not caught up. If the retry re-read went
// through the cached client it would keep seeing the pre-conflict resource version, resend the same
// losing version, and exhaust every attempt without converging. Reading through APIReader observes the
// current version and the second attempt succeeds.
func TestUpdatePvcDataHeightReadsUncachedOnRetry(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name:        "node",
		Namespace:   "default",
		Annotations: map[string]string{controllers.AnnotationDataHeight: "100"},
	}}
	apiServer := fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(pvc).Build()

	// Simulate a concurrent writer: the stored PVC moves to a new resource version that the cache has
	// not observed yet.
	concurrent := &corev1.PersistentVolumeClaim{}
	require.NoError(t, apiServer.Get(context.Background(), types.NamespacedName{Name: "node", Namespace: "default"}, concurrent))
	concurrent.Labels = map[string]string{"touched-by": "snapshot-flow"}
	require.NoError(t, apiServer.Update(context.Background(), concurrent))

	staleCache := &staleReadClient{Client: apiServer, stale: pvc.DeepCopy()}
	reconciler := &Reconciler{Client: staleCache, APIReader: apiServer, Scheme: scheme}
	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "node", Namespace: "default"},
		Status:     appsv1.ChainNodeStatus{LatestHeight: 200},
	}

	// The caller starts from the stale copy, as it would after a cached Get earlier in the reconcile.
	caller := pvc.DeepCopy()
	require.NoError(t, reconciler.updatePvcDataHeight(context.Background(), chainNode, caller))

	stored := &corev1.PersistentVolumeClaim{}
	require.NoError(t, apiServer.Get(context.Background(), types.NamespacedName{Name: "node", Namespace: "default"}, stored))
	assert.Equal(t, "200", stored.Annotations[controllers.AnnotationDataHeight])
	assert.Equal(t, "snapshot-flow", stored.Labels["touched-by"], "the concurrent writer's change must survive")
}

// staleReadClient serves a frozen copy of the PVC from Get, standing in for a controller cache that has
// not yet observed a concurrent update. Writes go through to the real store, so an Update built from a
// stale read is rejected as a conflict exactly as the API server would.
type staleReadClient struct {
	client.Client
	stale *corev1.PersistentVolumeClaim
}

func (c *staleReadClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	if pvc, ok := obj.(*corev1.PersistentVolumeClaim); ok && key.Name == c.stale.Name {
		c.stale.DeepCopyInto(pvc)
		return nil
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

// TestUpdatePvcDataHeightRejectsReplacedPvc guards against writing the old volume's height onto a
// different volume: a PVC deleted and recreated under the same name between the cached lookup and this
// step would otherwise be stamped with the previous chain height, which is later adopted as PVC state
// and would make a freshly initialized volume look like it already held the old chain data.
func TestUpdatePvcDataHeightRejectsReplacedPvc(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	// What the API server holds now: a different volume that happens to reuse the name.
	replacement := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name:        "node",
		Namespace:   "default",
		UID:         "new-pvc-uid",
		Annotations: map[string]string{controllers.AnnotationDataHeight: "0"},
	}}
	baseClient := fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(replacement).Build()
	reconciler := &Reconciler{Client: baseClient, APIReader: baseClient, Scheme: scheme}

	// What the caller is still holding from earlier in the reconcile: the deleted volume.
	stale := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name:        "node",
		Namespace:   "default",
		UID:         "old-pvc-uid",
		Annotations: map[string]string{controllers.AnnotationDataHeight: "100"},
	}}
	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "node", Namespace: "default"},
		Status:     appsv1.ChainNodeStatus{LatestHeight: 200},
	}

	err := reconciler.updatePvcDataHeight(context.Background(), chainNode, stale)
	require.Error(t, err)
	assert.ErrorContains(t, err, "was replaced")

	stored := &corev1.PersistentVolumeClaim{}
	require.NoError(t, baseClient.Get(context.Background(), types.NamespacedName{Name: "node", Namespace: "default"}, stored))
	assert.Equal(t, "0", stored.Annotations[controllers.AnnotationDataHeight],
		"the replacement volume must not inherit the old volume's height")
}

// TestUpdatePvcDataHeightPropagatesNonConflictErrors pins the distinction ensurePvcUpdates relies on:
// only exhausted optimistic-concurrency conflicts are tolerable there. A timeout or an authorization
// failure must stay a real error so controller-runtime applies its own retry/backoff, rather than being
// silently converted to success.
func TestUpdatePvcDataHeightPropagatesNonConflictErrors(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))

	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name:        "node",
		Namespace:   "default",
		Annotations: map[string]string{controllers.AnnotationDataHeight: "100"},
	}}
	baseClient := fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(pvc).Build()
	reconciler := &Reconciler{
		Client:    &pvcForbiddenClient{Client: baseClient},
		APIReader: baseClient,
		Scheme:    scheme,
	}
	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "node", Namespace: "default"},
		Status:     appsv1.ChainNodeStatus{LatestHeight: 200},
	}

	err := reconciler.updatePvcDataHeight(context.Background(), chainNode, pvc)
	require.Error(t, err)
	assert.False(t, apierrors.IsConflict(err), "a non-conflict failure must not masquerade as a conflict")
	assert.True(t, apierrors.IsForbidden(err))
}

// pvcForbiddenClient rejects PVC writes with a non-conflict error.
type pvcForbiddenClient struct {
	client.Client
}

func (c *pvcForbiddenClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	if _, ok := obj.(*corev1.PersistentVolumeClaim); ok {
		return apierrors.NewForbidden(
			schema.GroupResource{Resource: "persistentvolumeclaims"},
			obj.GetName(),
			assert.AnError,
		)
	}
	return c.Client.Update(ctx, obj, opts...)
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
