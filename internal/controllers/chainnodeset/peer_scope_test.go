package chainnodeset

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/voluzi/cosmopilot/v3/internal/controllers"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestListChainPeersOnlyUsesOwnNamespace(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	service := func(namespace, name, id string) *corev1.Service {
		return &corev1.Service{ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace, Name: name, Labels: map[string]string{
				controllers.LabelPeer: controllers.StringValueTrue, controllers.LabelChainID: "chain", controllers.LabelNodeID: id,
			},
		}}
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		service("target", "local-peer", "local-id"),
		service("other", "remote-peer", "remote-id"),
	).Build()
	r := &Reconciler{Client: cl}
	peers, err := r.listChainPeers(t.Context(), "target", "chain")
	require.NoError(t, err)
	require.Len(t, peers, 1)
	require.Equal(t, "local-id", peers[0].ID)
	require.Equal(t, "local-peer", peers[0].Address)
}
