package cosmoguard

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestApplyOwnedPreservesForeignFinalizer(t *testing.T) {
	const (
		foreign = "tailscale.com/finalizer"
		shared  = "example.com/shared"
		owned   = "cosmopilot.voluzi.com/guard"
	)
	scheme := servingScheme(t)
	require.NoError(t, corev1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	owner := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: "ns", UID: "owner-uid"}}
	p := baseParams()
	key := client.ObjectKey{Namespace: p.Namespace, Name: p.Name}
	var desired []string
	apply := func(t *testing.T) *corev1.Service {
		t.Helper()
		svc := p.Service()
		svc.Finalizers = append([]string(nil), desired...)
		require.NoError(t, ApplyOwned(t.Context(), c, scheme, owner, svc))
		live := &corev1.Service{}
		require.NoError(t, c.Get(t.Context(), key, live))
		return live
	}
	setLiveFinalizers := func(t *testing.T, finalizers ...string) string {
		t.Helper()
		live := &corev1.Service{}
		require.NoError(t, c.Get(t.Context(), key, live))
		live.Finalizers = finalizers
		require.NoError(t, c.Update(t.Context(), live))
		return live.ResourceVersion
	}

	t.Run("no desired finalizers", func(t *testing.T) {
		apply(t)
		resourceVersion := setLiveFinalizers(t, shared, foreign)
		assert.Equal(t, resourceVersion, apply(t).ResourceVersion, "a foreign finalizer must not trigger an Update")
		p.EvmEnabled = true
		live := apply(t)
		assert.ElementsMatch(t, []string{shared, foreign}, live.Finalizers)
		assert.Len(t, live.Spec.Ports, 6)
	})

	t.Run("desired finalizers overlapping the live ones", func(t *testing.T) {
		desired = []string{owned, shared}
		p.EvmEnabled = false
		live := apply(t)
		assert.ElementsMatch(t, []string{owned, shared, foreign}, live.Finalizers)
		assert.Len(t, live.Spec.Ports, 4)
		assert.Equal(t, live.ResourceVersion, apply(t).ResourceVersion, "the merged finalizers must not trigger an Update")
	})

	t.Run("foreign finalizer removed by its controller", func(t *testing.T) {
		resourceVersion := setLiveFinalizers(t, owned, shared)
		assert.Equal(t, resourceVersion, apply(t).ResourceVersion)
		p.EvmEnabled = true
		live := apply(t)
		assert.NotEqual(t, resourceVersion, live.ResourceVersion)
		assert.ElementsMatch(t, []string{owned, shared}, live.Finalizers)
	})

	// The patch maker reports a list-order diff for any live finalizer list that is not sorted, so
	// an order left as found would be rewritten on every reconcile.
	t.Run("unordered live finalizers converge", func(t *testing.T) {
		setLiveFinalizers(t, foreign, shared, owned)
		live := apply(t)
		assert.ElementsMatch(t, []string{owned, shared, foreign}, live.Finalizers)
		assert.Equal(t, live.ResourceVersion, apply(t).ResourceVersion)
	})
}

func TestApplyOwnedServiceAnnotationsConverge(t *testing.T) {
	scheme := servingScheme(t)
	require.NoError(t, corev1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	owner := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: "ns", UID: "owner-uid"}}
	p := baseParams()
	key := client.ObjectKey{Namespace: p.Namespace, Name: p.Name}
	resourceVersion := ""
	for _, tc := range []struct {
		name        string
		annotations map[string]string
		unchanged   bool
	}{
		{name: "create", annotations: map[string]string{"tailscale.com/expose": "true"}},
		{name: "unchanged", annotations: map[string]string{"tailscale.com/expose": "true"}, unchanged: true},
		{name: "change", annotations: map[string]string{"tailscale.com/expose": "false"}},
		{name: "add", annotations: map[string]string{"tailscale.com/expose": "false", "tailscale.com/hostname": "my-chain-rpc"}},
		{name: "remove one", annotations: map[string]string{"tailscale.com/expose": "false"}},
		{name: "remove all"},
		{name: "empty unchanged", unchanged: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p.ServiceAnnotations = tc.annotations
			require.NoError(t, ApplyOwned(t.Context(), c, scheme, owner, p.Service()))
			live := &corev1.Service{}
			require.NoError(t, c.Get(t.Context(), key, live))
			assert.NotContains(t, tc.annotations, "banzaicloud.com/last-applied")
			assert.NotEmpty(t, live.Annotations["banzaicloud.com/last-applied"])
			delete(live.Annotations, "banzaicloud.com/last-applied")
			assert.Len(t, live.Annotations, len(tc.annotations))
			for key, value := range tc.annotations {
				assert.Equal(t, value, live.Annotations[key])
			}
			if tc.unchanged {
				assert.Equal(t, resourceVersion, live.ResourceVersion)
			} else {
				assert.NotEqual(t, resourceVersion, live.ResourceVersion)
			}
			resourceVersion = live.ResourceVersion
		})
	}
}

func servingScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(s))
	return s
}

func TestIsServing(t *testing.T) {
	sts := func(gen, observed int64, replicas, updated, ready int32) *appsv1.StatefulSet {
		return &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "ns", Generation: gen},
			Spec:       appsv1.StatefulSetSpec{Replicas: ptr.To(replicas)},
			Status:     appsv1.StatefulSetStatus{ObservedGeneration: observed, UpdatedReplicas: updated, ReadyReplicas: ready},
		}
	}

	cases := []struct {
		name string
		sts  *appsv1.StatefulSet
		want bool
	}{
		// Mid rolling-update: not all replicas updated yet, but ready replicas are still serving.
		// Must stay "serving" so the guarded Service doesn't revert to raw node pods.
		{"mid-rollout still serving", sts(3, 3, 3, 1, 2), true},
		{"fully rolled out", sts(2, 2, 3, 3, 3), true},
		{"no ready replicas", sts(1, 1, 1, 1, 0), false},
		{"status not yet observed", sts(4, 3, 3, 3, 3), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(servingScheme(t)).WithObjects(tc.sts).Build()
			got, err := IsServing(context.Background(), c, "ns", "g")
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}

	// Missing StatefulSet is not-serving, not an error.
	c := fake.NewClientBuilder().WithScheme(servingScheme(t)).Build()
	got, err := IsServing(context.Background(), c, "ns", "absent")
	require.NoError(t, err)
	assert.False(t, got)
}

func TestIsFullyRolledOut(t *testing.T) {
	sts := func(gen, observed int64, replicas, updated, ready int32) *appsv1.StatefulSet {
		return &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "ns", Generation: gen},
			Spec:       appsv1.StatefulSetSpec{Replicas: ptr.To(replicas)},
			Status:     appsv1.StatefulSetStatus{ObservedGeneration: observed, UpdatedReplicas: updated, ReadyReplicas: ready},
		}
	}
	cases := []struct {
		name string
		sts  *appsv1.StatefulSet
		want bool
	}{
		{"all updated and ready", sts(2, 2, 3, 3, 3), true},
		// Global route must NOT flip while only some updated pods are ready.
		{"updated but only one ready", sts(2, 2, 3, 3, 1), false},
		{"not all updated", sts(2, 2, 3, 1, 1), false},
		{"generation not observed", sts(3, 2, 3, 3, 3), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(servingScheme(t)).WithObjects(tc.sts).Build()
			got, err := IsFullyRolledOut(context.Background(), c, "ns", "g")
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}

	// nil Replicas (HPA owns replicas): falls back to ReadyReplicas > 0.
	nilReplicas := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "g", Namespace: "ns", Generation: 1},
		Spec:       appsv1.StatefulSetSpec{Replicas: nil},
		Status:     appsv1.StatefulSetStatus{ObservedGeneration: 1, ReadyReplicas: 1},
	}
	c := fake.NewClientBuilder().WithScheme(servingScheme(t)).WithObjects(nilReplicas).Build()
	got, err := IsFullyRolledOut(context.Background(), c, "ns", "g")
	require.NoError(t, err)
	assert.True(t, got, "nil replicas -> ready>0 is fully rolled out")

	// Missing StatefulSet -> false, no error.
	got, err = IsFullyRolledOut(context.Background(), fake.NewClientBuilder().WithScheme(servingScheme(t)).Build(), "ns", "absent")
	require.NoError(t, err)
	assert.False(t, got)
}
