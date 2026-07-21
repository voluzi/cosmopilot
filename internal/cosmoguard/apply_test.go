package cosmoguard

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

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
