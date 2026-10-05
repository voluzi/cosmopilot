package chainnodeset

import (
	"testing"

	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"

	appsv1 "github.com/voluzi/cosmopilot/v5/api/v1"
)

func TestNodeSetPropagatesRestoreToEveryInstance(t *testing.T) {
	group := appsv1.NodeGroupSpec{Name: "restored", Instances: ptr.To(3), SnapshotNodeIndex: ptr.To(0), Persistence: &appsv1.Persistence{
		Restore:   &appsv1.SnapshotRestoreConfig{Snapshot: appsv1.SnapshotRestoreSource{Provider: "gcs", Bucket: "backups", Name: "snapshot.tar"}},
		Snapshots: &appsv1.VolumeSnapshotsConfig{Frequency: "1h"},
	}}
	nodeSet := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{Name: "chain", Namespace: "default"}, Spec: appsv1.ChainNodeSetSpec{Genesis: &appsv1.GenesisConfig{ConfigMap: ptr.To("genesis")}, App: appsv1.AppSpec{Image: "app", App: "appd"}, Nodes: []appsv1.NodeGroupSpec{group}}}
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	r := &Reconciler{Scheme: scheme}
	first, err := r.getNodeSpec(nodeSet, group, 0)
	require.NoError(t, err)
	require.Equal(t, "snapshot.tar", first.Spec.Persistence.Restore.Snapshot.Name)
	first.Spec.Persistence.Restore.Snapshot.Name = "other.tar"
	for i := 1; i < 3; i++ {
		child, err := r.getNodeSpec(nodeSet, group, i)
		require.NoError(t, err)
		require.Equal(t, "snapshot.tar", child.Spec.Persistence.Restore.Snapshot.Name)
		require.Nil(t, child.Spec.Persistence.Snapshots)
	}
	require.Equal(t, "snapshot.tar", nodeSet.Spec.Nodes[0].Persistence.Restore.Snapshot.Name)
}
