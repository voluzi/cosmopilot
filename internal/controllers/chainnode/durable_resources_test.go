package chainnode

import (
	"context"
	"testing"

	snapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v6/apis/volumesnapshot/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/cometbft"
	"github.com/voluzi/cosmopilot/v2/internal/resourcecleanup"
)

func TestGeneratedKeySecretsCarryStableAttribution(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*appsv1.ChainNode)
		ensure    func(context.Context, *Reconciler, *appsv1.ChainNode) error
		secret    func(*appsv1.ChainNode) string
	}{
		{
			name: "node key",
			ensure: func(ctx context.Context, r *Reconciler, node *appsv1.ChainNode) error {
				return r.ensureNodeKey(ctx, node)
			},
			secret: func(node *appsv1.ChainNode) string { return node.Name },
		},
		{
			name: "consensus key",
			configure: func(node *appsv1.ChainNode) {
				node.Spec.Validator = &appsv1.ValidatorConfig{}
			},
			ensure: func(ctx context.Context, r *Reconciler, node *appsv1.ChainNode) error {
				return r.ensureSigningKey(ctx, node)
			},
			secret: func(node *appsv1.ChainNode) string { return node.Spec.Validator.GetPrivKeySecretName(node) },
		},
		{
			name: "account key",
			configure: func(node *appsv1.ChainNode) {
				node.Spec.Validator = &appsv1.ValidatorConfig{Init: &appsv1.GenesisInitConfig{ChainID: "chain-1"}}
			},
			ensure: func(ctx context.Context, r *Reconciler, node *appsv1.ChainNode) error {
				return r.ensureAccount(ctx, node)
			},
			secret: func(node *appsv1.ChainNode) string { return node.Spec.Validator.GetAccountSecretName(node) },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := deletionTestChainNode("node-" + tt.name)
			if tt.configure != nil {
				tt.configure(node)
			}
			r := newDurableResourceReconciler(t, node)

			require.NoError(t, tt.ensure(context.Background(), r, node))
			secret := &corev1.Secret{}
			require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: node.Namespace, Name: tt.secret(node)}, secret))
			assert.True(t, resourcecleanup.IsAttributed(secret, resourcecleanup.RootOwnerFor(node), resourcecleanup.ClassGeneratedKeys))
			assert.Nil(t, metav1.GetControllerOf(secret))
		})
	}
}

func TestExistingUnownedNodeKeySecretRemainsAmbiguous(t *testing.T) {
	node := deletionTestChainNode("node")
	_, key, err := cometbft.GenerateNodeKey()
	require.NoError(t, err)
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: node.Name, Namespace: node.Namespace}, Data: map[string][]byte{nodeKeyFilename: key}}
	r := newDurableResourceReconciler(t, node, secret)

	require.NoError(t, r.ensureNodeKey(context.Background(), node))
	current := &corev1.Secret{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(secret), current))
	assert.False(t, resourcecleanup.IsAttributed(current, resourcecleanup.RootOwnerFor(node), resourcecleanup.ClassGeneratedKeys))
	assert.Empty(t, current.OwnerReferences)
}

func TestGeneratedDataVolumesCarryStableAttribution(t *testing.T) {
	node := deletionTestChainNode("node")
	node.Spec.Persistence = &appsv1.Persistence{
		RestoreFromSnapshot: &appsv1.PvcSnapshot{Name: "snapshot"},
		AdditionalVolumes:   []appsv1.VolumeSpec{{Name: "wasm", Size: "1Gi", Path: "/wasm", DeleteWithNode: ptr.To(true)}},
	}
	snapshot := &snapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snapshot", Namespace: node.Namespace},
		Status:     &snapshotv1.VolumeSnapshotStatus{},
	}
	r := newDurableResourceReconciler(t, node, snapshot)

	require.NoError(t, r.ensureAdditionalVolumes(context.Background(), node))
	_, _, err := r.ensureDataVolume(context.Background(), nil, node)
	require.NoError(t, err)

	for _, name := range []string{node.Name, node.Name + "-wasm"} {
		pvc := &corev1.PersistentVolumeClaim{}
		require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: node.Namespace, Name: name}, pvc))
		assert.True(t, resourcecleanup.IsAttributed(pvc, resourcecleanup.RootOwnerFor(node), resourcecleanup.ClassDataVolumes))
		assert.Nil(t, metav1.GetControllerOf(pvc))
	}
}

func deletionTestChainNode(name string) *appsv1.ChainNode {
	return &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID(name + "-uid")}}
}

func newDurableResourceReconciler(t *testing.T, objects ...client.Object) *Reconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, snapshotv1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&appsv1.ChainNode{}).WithObjects(objects...).Build()
	return &Reconciler{Client: c, APIReader: c, Scheme: scheme, recorder: record.NewFakeRecorder(100)}
}
