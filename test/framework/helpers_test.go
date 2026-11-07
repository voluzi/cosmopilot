package framework

import (
	"testing"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v6/apis/volumesnapshot/v1"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestSimulateSnapshotReadyRejectsMalformedSizeBeforeCreatingContent(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, snapshotv1.AddToScheme(scheme))
	snapshot := &snapshotv1.VolumeSnapshot{ObjectMeta: metav1.ObjectMeta{Name: "snapshot", Namespace: "ns"}}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(snapshot).WithStatusSubresource(snapshot).Build()
	helpers := &TestHelpers{client: reader, ctx: t.Context()}

	require.ErrorContains(t, helpers.SimulateSnapshotReady("ns", "snapshot", "not-a-quantity"), "not-a-quantity")

	contents := &snapshotv1.VolumeSnapshotContentList{}
	require.NoError(t, reader.List(t.Context(), contents))
	require.Empty(t, contents.Items)
}
