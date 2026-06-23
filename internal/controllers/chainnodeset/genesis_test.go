package chainnodeset

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/chainutils"
)

func newGenesisTestReconciler(t *testing.T, objs ...client.Object) *Reconciler {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&appsv1.ChainNodeSet{}).
		WithObjects(objs...).
		Build()

	return &Reconciler{
		Client:   cl,
		Scheme:   scheme,
		recorder: record.NewFakeRecorder(100),
	}
}

func TestEnsureGenesisNilSpecUsesGeneratedConfigMapName(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: types.UID("nodeset-uid")},
		Spec:       appsv1.ChainNodeSetSpec{},
		Status:     appsv1.ChainNodeSetStatus{ChainID: "test-chain"},
	}
	genesisCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-chain-genesis",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: appsv1.GroupVersion.String(),
				Kind:       ChainNodeKind,
				Name:       "test-nodeset-validators-0",
				UID:        types.UID("validator-uid"),
			}},
		},
		Data: map[string]string{chainutils.GenesisFilename: "{}"},
	}

	r := newGenesisTestReconciler(t, nodeSet, genesisCM)
	require.NoError(t, r.ensureGenesis(context.Background(), nil, nodeSet))

	got := &corev1.ConfigMap{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-chain-genesis"}, got))
	require.Len(t, got.OwnerReferences, 1)
	require.Equal(t, nodeSet.UID, got.OwnerReferences[0].UID)
}
