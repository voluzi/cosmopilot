package chainnode

import (
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestInternalServiceStateSyncAnnotations(t *testing.T) {
	tests := []struct {
		name      string
		config    *appsv1.Config
		snapshots bool
	}{
		{name: "no config"},
		{name: "snapshots disabled", config: &appsv1.Config{StateSync: &appsv1.StateSyncConfig{SnapshotInterval: 0}}},
		{name: "snapshots enabled", config: &appsv1.Config{StateSync: &appsv1.StateSyncConfig{SnapshotInterval: 100}}, snapshots: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var requests []string
			transport := http.DefaultTransport
			http.DefaultTransport = roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				requests = append(requests, req.URL.String())
				return nil, errors.New("node-utils unavailable")
			})
			t.Cleanup(func() { http.DefaultTransport = transport })

			node := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: "node", Namespace: "default", UID: "node-uid"}}
			node.Spec.Config = tt.config
			stale := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
				Name:            "node-internal",
				Namespace:       node.Namespace,
				OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(node, appsv1.GroupVersion.WithKind("ChainNode"))},
				Annotations: map[string]string{
					controllers.AnnotationStateSyncTrustHeight: "100",
					controllers.AnnotationStateSyncTrustHash:   "hash100",
				},
			}}
			scheme := runtime.NewScheme()
			require.NoError(t, appsv1.AddToScheme(scheme))
			require.NoError(t, corev1.AddToScheme(scheme))
			recorder := record.NewFakeRecorder(10)
			r := &Reconciler{
				Client:   fake.NewClientBuilder().WithScheme(scheme).WithObjects(node, stale).Build(),
				Scheme:   scheme,
				recorder: recorder,
			}

			var versions []string
			for range 2 {
				svc, err := r.getInternalServiceSpec(t.Context(), node)
				require.NoError(t, err)
				require.NoError(t, r.ensureService(t.Context(), svc))
				current := &corev1.Service{}
				require.NoError(t, r.Get(t.Context(), client.ObjectKeyFromObject(stale), current))
				assert.NotContains(t, current.Annotations, controllers.AnnotationStateSyncTrustHeight)
				assert.NotContains(t, current.Annotations, controllers.AnnotationStateSyncTrustHash)
				versions = append(versions, current.ResourceVersion)
			}
			assert.Equal(t, versions[0], versions[1], "second reconcile updated the Service")

			if tt.snapshots {
				assert.NotEmpty(t, requests)
				return
			}
			assert.Empty(t, requests)
			assert.Empty(t, recorder.Events)
		})
	}
}
