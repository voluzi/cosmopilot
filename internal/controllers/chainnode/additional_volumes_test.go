package chainnode

import (
	"context"
	"errors"
	"testing"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v6/apis/volumesnapshot/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	appsv1 "github.com/voluzi/cosmopilot/v3/api/v1"
)

// TestEnsureAdditionalVolumesResize covers the resize paths: an equal size in other units issues no
// update, a shrink is skipped with an event, and a rejected growth does not fail the reconcile.
func TestEnsureAdditionalVolumesResize(t *testing.T) {
	for _, tc := range []struct {
		name         string
		newSize      string
		rejectUpdate bool
		wantSize     string
		wantUpdates  int
		wantEvent    bool
	}{
		{name: "equal size in other units", newSize: "20480Mi", wantSize: "20Gi"},
		{name: "shrink", newSize: "10Gi", wantSize: "20Gi", wantEvent: true},
		{name: "growth", newSize: "30Gi", wantSize: "30Gi", wantUpdates: 1},
		{name: "growth rejected by the storage class", newSize: "30Gi", rejectUpdate: true, wantSize: "20Gi", wantUpdates: 1, wantEvent: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := deletionTestChainNode("node")
			node.Spec.Persistence = &appsv1.Persistence{
				AdditionalVolumes: []appsv1.VolumeSpec{{Name: "extra", Size: "20Gi", Path: "/extra"}},
			}
			scheme := runtime.NewScheme()
			require.NoError(t, appsv1.AddToScheme(scheme))
			require.NoError(t, corev1.AddToScheme(scheme))
			require.NoError(t, snapshotv1.AddToScheme(scheme))
			updates := 0
			reject := false
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).
				WithInterceptorFuncs(interceptor.Funcs{
					Update: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
						if _, ok := obj.(*corev1.PersistentVolumeClaim); ok {
							updates++
							if reject {
								return errors.New("storage class does not allow volume expansion")
							}
						}
						return cl.Update(ctx, obj, opts...)
					},
				}).Build()
			recorder := record.NewFakeRecorder(10)
			r := &Reconciler{Client: c, APIReader: c, Scheme: scheme, recorder: recorder}
			require.NoError(t, r.ensureAdditionalVolumes(context.Background(), node))

			updates, reject = 0, tc.rejectUpdate
			node.Spec.Persistence.AdditionalVolumes[0].Size = tc.newSize
			require.NoError(t, r.ensureAdditionalVolumes(context.Background(), node))

			assert.Equal(t, tc.wantUpdates, updates)
			pvc := &corev1.PersistentVolumeClaim{}
			require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: node.Namespace, Name: "node-extra"}, pvc))
			assert.Zero(t, pvc.Spec.Resources.Requests.Storage().Cmp(resource.MustParse(tc.wantSize)))
			var events []string
			for len(recorder.Events) > 0 {
				events = append(events, <-recorder.Events)
			}
			if tc.wantEvent {
				require.Len(t, events, 1, "expected exactly one %s event", appsv1.ReasonPvcResizeSkipped)
				assert.Contains(t, events[0], appsv1.ReasonPvcResizeSkipped)
			} else {
				assert.Empty(t, events)
			}
		})
	}
}
