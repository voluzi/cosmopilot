package controllers

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	appsv1 "github.com/voluzi/cosmopilot/v5/api/v1"
)

func legacySignerScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	return scheme
}

func TestLegacySignerGuardChecksCleanUIDOnce(t *testing.T) {
	node := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: "validator", Namespace: "default", UID: "node-uid"}}
	var gets atomic.Int32
	c := fake.NewClientBuilder().WithScheme(legacySignerScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			gets.Add(1)
			return c.Get(ctx, key, obj, opts...)
		},
	}).Build()
	guard := &LegacySignerGuard{}
	var wg sync.WaitGroup
	errors := make(chan error, 20)
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errors <- guard.RefuseLegacyTmKMS(t.Context(), c, nil, node)
		}()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	// Concurrent first checks of one UID may each read; none may read after a pass.
	afterFirst := gets.Load()
	require.GreaterOrEqual(t, afterFirst, int32(3))
	require.LessOrEqual(t, afterFirst, int32(3*20))
	require.NoError(t, guard.RefuseLegacyTmKMS(t.Context(), c, nil, node))
	require.Equal(t, afterFirst, gets.Load(), "passed nodes must not be read again")
	freshProcess := &LegacySignerGuard{}
	require.NoError(t, freshProcess.RefuseLegacyTmKMS(t.Context(), c, nil, node))
	require.Equal(t, afterFirst+3, gets.Load(), "a fresh process must check authoritatively again")
	recreated := node.DeepCopy()
	recreated.UID = "replacement-uid"
	require.NoError(t, guard.RefuseLegacyTmKMS(t.Context(), c, nil, recreated))
	require.Equal(t, afterFirst+6, gets.Load(), "recreated names must be checked again")
}

func TestLegacySignerGuardRetriesRefusalsAndReadErrors(t *testing.T) {
	for _, refusal := range []bool{false, true} {
		t.Run(fmt.Sprint(refusal), func(t *testing.T) {
			node := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: "validator", Namespace: "default", UID: "node-uid"}}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: node.Name, Namespace: node.Namespace}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "tmkms"}}}}
			var fail atomic.Bool
			fail.Store(!refusal)
			var gets atomic.Int32
			c := fake.NewClientBuilder().WithScheme(legacySignerScheme(t)).WithInterceptorFuncs(interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					gets.Add(1)
					if fail.Load() {
						return errors.New("API unavailable")
					}
					return c.Get(ctx, key, obj, opts...)
				},
			}).Build()
			if refusal {
				require.NoError(t, c.Create(t.Context(), pod))
			}
			guard := &LegacySignerGuard{}
			require.Error(t, guard.RefuseLegacyTmKMS(t.Context(), c, nil, node))
			firstGets := gets.Load()
			require.Error(t, guard.RefuseLegacyTmKMS(t.Context(), c, nil, node))
			require.Greater(t, gets.Load(), firstGets, "refusals and errors must not be recorded")
			fail.Store(false)
			if refusal {
				require.NoError(t, c.Delete(t.Context(), pod))
			}
			require.NoError(t, guard.RefuseLegacyTmKMS(t.Context(), c, nil, node))
			cleanGets := gets.Load()
			require.NoError(t, guard.RefuseLegacyTmKMS(t.Context(), c, nil, node))
			require.Equal(t, cleanGets, gets.Load())
		})
	}
}

func TestLegacySignerGuardListsOncePerSet(t *testing.T) {
	for _, childCount := range []int{1, 30} {
		t.Run(fmt.Sprint(childCount), func(t *testing.T) {
			set := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{Name: "chain", Namespace: "default", UID: "set-uid"}}
			objects := []client.Object{set}
			for i := range childCount {
				objects = append(objects, &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{
					Name: fmt.Sprintf("validator-%d", i), Namespace: set.Namespace, UID: types.UID(fmt.Sprintf("child-%d", i)),
					OwnerReferences: []metav1.OwnerReference{{APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNodeSet", Name: set.Name, UID: set.UID, Controller: ptr.To(true)}},
				}})
			}
			var lists, gets atomic.Int32
			c := fake.NewClientBuilder().WithScheme(legacySignerScheme(t)).WithObjects(objects...).WithInterceptorFuncs(interceptor.Funcs{
				List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
					lists.Add(1)
					options := &client.ListOptions{}
					for _, opt := range opts {
						opt.ApplyToList(options)
					}
					if options.Namespace != set.Namespace {
						return fmt.Errorf("list must be scoped to namespace %q", set.Namespace)
					}
					return c.List(ctx, list, opts...)
				},
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
					gets.Add(1)
					return c.Get(ctx, key, obj, opts...)
				},
			}).Build()
			guard := &LegacySignerGuard{}
			require.NoError(t, guard.RefuseLegacyTmKMSChildren(t.Context(), c, nil, set))
			require.EqualValues(t, 4, lists.Load(), "request count must not grow with child count")
			require.Zero(t, gets.Load(), "children must be checked from namespace lists")
			require.NoError(t, guard.RefuseLegacyTmKMSChildren(t.Context(), c, nil, set))
			require.EqualValues(t, 4, lists.Load(), "a passed set must not be checked again")
			for _, obj := range objects[1:] {
				require.NoError(t, guard.RefuseLegacyTmKMS(t.Context(), c, nil, obj.(*appsv1.ChainNode)))
			}
			require.Zero(t, gets.Load(), "child passes must be shared between controllers")
			freshProcess := &LegacySignerGuard{}
			require.NoError(t, freshProcess.RefuseLegacyTmKMSChildren(t.Context(), c, nil, set))
			require.EqualValues(t, 8, lists.Load(), "a fresh process must list again")
		})
	}
}

func TestLegacySignerGuardSkipsSnapshotsWhenChildrenAlreadyPassed(t *testing.T) {
	set := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{Name: "chain", Namespace: "default", UID: "set-uid"}}
	child := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: "validator", Namespace: set.Namespace, UID: "child-uid",
		OwnerReferences: []metav1.OwnerReference{{APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNodeSet", Name: set.Name, UID: set.UID, Controller: ptr.To(true)}},
	}}
	var lists atomic.Int32
	c := fake.NewClientBuilder().WithScheme(legacySignerScheme(t)).WithObjects(set, child).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			lists.Add(1)
			return c.List(ctx, list, opts...)
		},
	}).Build()
	guard := &LegacySignerGuard{}
	require.NoError(t, guard.RefuseLegacyTmKMS(t.Context(), c, nil, child))
	require.NoError(t, guard.RefuseLegacyTmKMSChildren(t.Context(), c, nil, set))
	require.EqualValues(t, 1, lists.Load(), "only the child inventory is listed when every child already passed")
}

func TestLegacySignerGuardDoesNotRecordRefusedSet(t *testing.T) {
	set := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{Name: "chain", Namespace: "default", UID: "set-uid"}}
	child := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: "validator", Namespace: set.Namespace, UID: "child-uid",
		OwnerReferences: []metav1.OwnerReference{{APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNodeSet", Name: set.Name, UID: set.UID, Controller: ptr.To(true)}},
	}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: child.Name, Namespace: child.Namespace}, Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "tmkms"}}}}
	var lists atomic.Int32
	c := fake.NewClientBuilder().WithScheme(legacySignerScheme(t)).WithObjects(set, child, pod).WithInterceptorFuncs(interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			lists.Add(1)
			return c.List(ctx, list, opts...)
		},
	}).Build()
	guard := &LegacySignerGuard{}
	require.ErrorContains(t, guard.RefuseLegacyTmKMSChildren(t.Context(), c, nil, set), "tmkms container")
	require.ErrorContains(t, guard.RefuseLegacyTmKMSChildren(t.Context(), c, nil, set), "tmkms container")
	require.EqualValues(t, 8, lists.Load(), "every child must pass before a set is recorded")
	require.NoError(t, c.Delete(t.Context(), pod))
	require.NoError(t, guard.RefuseLegacyTmKMSChildren(t.Context(), c, nil, set))
	require.EqualValues(t, 12, lists.Load(), "refused sets must be checked again after migration")
	require.NoError(t, guard.RefuseLegacyTmKMSChildren(t.Context(), c, nil, set))
	require.EqualValues(t, 12, lists.Load())
}
