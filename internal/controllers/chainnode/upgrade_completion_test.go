package chainnode

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	appsv1 "github.com/voluzi/cosmopilot/v3/api/v1"
	"github.com/voluzi/cosmopilot/v3/internal/controllers"
)

func TestCompleteUpgradePreservesImageAcrossVPAObjectUpdate(t *testing.T) {
	ctx := t.Context()
	scheme := nodeUtilsAuthTestScheme(t)
	node := nodeUtilsAuthTestNode()
	node.Spec.App.Image = "repo/app:v1"
	node.Spec.VPA = &appsv1.VerticalAutoscalingConfig{Enabled: true, ResetVpaAfterNodeUpgrade: true}
	node.Annotations = map[string]string{controllers.AnnotationVPAResources: `{"requests":{"cpu":"2"}}`}
	node.Status.LatestHeight = 99
	node.Status.AppImage = "repo/app:v1"
	node.Status.AppVersion = "v1"
	node.Status.Upgrades = []appsv1.Upgrade{{
		Height: 100,
		Source: appsv1.OnChainUpgrade,
		Name:   "v2",
		Image:  "repo/app:v2",
		Status: appsv1.UpgradeOnGoing,
	}}
	backing := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(node).WithObjects(node).Build()
	r := &Reconciler{Client: backing, Scheme: scheme}

	require.NoError(t, r.completeUpgrade(ctx, node, &node.Status.Upgrades[0], "failed to reset VPA after upgrade"))

	stored := &appsv1.ChainNode{}
	require.NoError(t, backing.Get(ctx, client.ObjectKeyFromObject(node), stored))
	assert.Equal(t, int64(99), stored.Status.LatestHeight)
	assert.Equal(t, "repo/app:v2", stored.Status.AppImage)
	assert.Equal(t, "v2", stored.Status.AppVersion)
	require.Len(t, stored.Status.Upgrades, 1)
	assert.Equal(t, appsv1.UpgradeCompleted, stored.Status.Upgrades[0].Status)
	assert.Equal(t, "repo/app:v2", stored.GetAppImage())
	assert.NotContains(t, stored.Annotations, controllers.AnnotationVPAResources)
	assert.NotEmpty(t, stored.Annotations[controllers.AnnotationVPALastCPUScale])
	assert.NotEmpty(t, stored.Annotations[controllers.AnnotationVPALastMemoryScale])
}
