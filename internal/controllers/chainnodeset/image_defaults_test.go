package chainnodeset

import (
	"testing"

	"github.com/stretchr/testify/require"
	k8sappsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"

	appsv1 "github.com/voluzi/cosmopilot/v3/api/v1"
	"github.com/voluzi/cosmopilot/v3/internal/controllers"
	"github.com/voluzi/cosmopilot/v3/pkg/images"
)

func TestGeneratedCompanionWorkloadsUsePinnedDefaultImages(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, k8sappsv1.AddToScheme(scheme))
	r := &Reconciler{Scheme: scheme, opts: &controllers.ControllerRunOptions{}}
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "nodes", Namespace: "default", UID: "nodes-uid"},
		Spec: appsv1.ChainNodeSetSpec{
			Cosmoseed: &appsv1.CosmoseedConfig{Instances: ptr.To(1)},
		},
		Status: appsv1.ChainNodeSetStatus{ChainID: "chain-1"},
	}

	statefulSet, err := r.getStatefulSet(nodeSet, "config-hash", nil)
	require.NoError(t, err)
	require.Equal(t, images.DefaultCosmoseedImage, statefulSet.Spec.Template.Spec.Containers[0].Image)

	group := appsv1.NodeGroupSpec{Name: "rpc", Config: &appsv1.Config{
		CosmoGuard: &appsv1.CosmoGuardConfig{Enable: true},
	}}
	require.Equal(t, images.DefaultCosmoGuardImage, r.groupCosmoGuardParams(nodeSet, group).Image)
}
