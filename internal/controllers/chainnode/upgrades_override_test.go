package chainnode

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	appsv1 "github.com/voluzi/cosmopilot/v3/api/v1"
	"github.com/voluzi/cosmopilot/v3/internal/controllers"
	"github.com/voluzi/cosmopilot/v3/pkg/nodeutils"
)

func pinnedNodeAtUpgradeHeight() *appsv1.ChainNode {
	return &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "pinned", Namespace: "default"},
		Spec: appsv1.ChainNodeSpec{
			App:           appsv1.AppSpec{Image: "repo/app", Version: ptr.To("v1"), App: "appd"},
			OverrideImage: ptr.To("repo/app:pinned"),
		},
		Status: appsv1.ChainNodeStatus{
			LatestHeight: 500,
			Upgrades: []appsv1.Upgrade{
				{Height: 500, Image: "repo/app:v2", Status: appsv1.UpgradeScheduled},
			},
		},
	}
}

// A pinned node must not merely suppress the upgrade in the operator: node-utils halts the
// application for any upgrade still `scheduled` at or below the current height, so an entry left
// scheduled would halt every recreated pod and spin a stop/recreate loop.
func TestSkipUpgradeForOverrideStopsNodeUtilsRetriggering(t *testing.T) {
	scheme := gcpImportTestScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&appsv1.ChainNode{}).
		WithObjects(pinnedNodeAtUpgradeHeight()).
		Build()
	r := &Reconciler{Client: c, Scheme: scheme, opts: &controllers.ControllerRunOptions{}}

	chainNode := &appsv1.ChainNode{}
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: "pinned", Namespace: "default"}, chainNode))

	require.NoError(t, r.skipUpgradeForOverride(context.Background(), chainNode))

	// The upgrade is no longer scheduled, so node-utils will not halt the app for it again.
	require.Len(t, chainNode.Status.Upgrades, 1)
	assert.Equal(t, appsv1.UpgradeSkipped, chainNode.Status.Upgrades[0].Status)

	// And the config node-utils actually reads reflects that.
	cm := &corev1.ConfigMap{}
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: "pinned-upgrades", Namespace: "default"}, cm))

	var published struct {
		Upgrades []appsv1.Upgrade `json:"upgrades"`
	}
	require.NoError(t, json.Unmarshal([]byte(cm.Data[upgradesConfigFile]), &published))
	require.Len(t, published.Upgrades, 1)
	assert.NotEqual(t, nodeutils.UpgradeScheduled, string(published.Upgrades[0].Status),
		"an upgrade left scheduled would make node-utils halt the app again")

	// Once skipped, the upgrade counts as reached: removing the override moves the node onto it.
	chainNode.Spec.OverrideImage = nil
	assert.Equal(t, "repo/app:v2", chainNode.GetAppImage())
}

func TestSkipUpgradeForOverrideIsNoopWithoutScheduledUpgrade(t *testing.T) {
	stored := pinnedNodeAtUpgradeHeight()
	stored.Status.Upgrades[0].Status = appsv1.UpgradeCompleted
	scheme := gcpImportTestScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&appsv1.ChainNode{}).
		WithObjects(stored).
		Build()
	r := &Reconciler{Client: c, Scheme: scheme, opts: &controllers.ControllerRunOptions{}}

	chainNode := &appsv1.ChainNode{}
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: "pinned", Namespace: "default"}, chainNode))

	require.NoError(t, r.skipUpgradeForOverride(context.Background(), chainNode))
	assert.Equal(t, appsv1.UpgradeCompleted, chainNode.Status.Upgrades[0].Status)
}

// node-utils halts for any scheduled upgrade at or *below* the current height, so a pinned node that
// has already advanced past the upgrade height must still have it skipped. An exact-height lookup
// would miss this and leave node-utils halting every recreated pod.
func TestSkipUpgradeForOverrideSkipsUpgradesBelowCurrentHeight(t *testing.T) {
	stored := pinnedNodeAtUpgradeHeight()
	stored.Status.LatestHeight = 900
	stored.Status.Upgrades = []appsv1.Upgrade{
		{Height: 500, Image: "repo/app:v2", Status: appsv1.UpgradeScheduled},
		{Height: 800, Image: "repo/app:v3", Status: appsv1.UpgradeScheduled},
		{Height: 1500, Image: "repo/app:v4", Status: appsv1.UpgradeScheduled},
	}
	scheme := gcpImportTestScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&appsv1.ChainNode{}).
		WithObjects(stored).
		Build()
	r := &Reconciler{Client: c, Scheme: scheme, opts: &controllers.ControllerRunOptions{}}

	chainNode := &appsv1.ChainNode{}
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: "pinned", Namespace: "default"}, chainNode))

	require.NoError(t, r.skipUpgradeForOverride(context.Background(), chainNode))

	// Both upgrades at or below the current height are skipped; the future one is untouched so it
	// still applies once the override is removed.
	assert.Equal(t, appsv1.UpgradeSkipped, chainNode.Status.Upgrades[0].Status)
	assert.Equal(t, appsv1.UpgradeSkipped, chainNode.Status.Upgrades[1].Status)
	assert.Equal(t, appsv1.UpgradeScheduled, chainNode.Status.Upgrades[2].Status)

	cm := &corev1.ConfigMap{}
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: "pinned-upgrades", Namespace: "default"}, cm))
	var published struct {
		Upgrades []appsv1.Upgrade `json:"upgrades"`
	}
	require.NoError(t, json.Unmarshal([]byte(cm.Data[upgradesConfigFile]), &published))
	for _, u := range published.Upgrades {
		if u.Height <= 900 {
			assert.NotEqual(t, nodeutils.UpgradeScheduled, string(u.Status),
				"upgrade at height %d would still halt the pinned pod", u.Height)
		}
	}
}

// A node that is already Syncing/StateSyncing/Running when cosmopilot is upgraded never hits a phase
// transition, so the recorded image must be backfilled in the steady state too — syncing can last
// hours, and .status.appImage is documented as the authoritative record of what is running.
func TestSyncRecordedAppImageBackfillsWithoutPhaseChange(t *testing.T) {
	stored := pinnedNodeAtUpgradeHeight()
	stored.Spec.OverrideImage = nil
	stored.Status.Phase = appsv1.PhaseChainNodeSyncing
	stored.Status.AppImage = ""
	stored.Status.AppVersion = ""
	scheme := gcpImportTestScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&appsv1.ChainNode{}).
		WithObjects(stored).
		Build()
	r := &Reconciler{Client: c, Scheme: scheme, opts: &controllers.ControllerRunOptions{}}

	chainNode := &appsv1.ChainNode{}
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: "pinned", Namespace: "default"}, chainNode))

	require.NoError(t, r.syncRecordedAppImage(context.Background(), chainNode))
	assert.Equal(t, "repo/app:v1", chainNode.Status.AppImage)
	assert.Equal(t, "v1", chainNode.Status.AppVersion)
	assert.Equal(t, appsv1.PhaseChainNodeSyncing, chainNode.Status.Phase, "backfill must not change phase")

	persisted := &appsv1.ChainNode{}
	require.NoError(t, c.Get(context.Background(),
		types.NamespacedName{Name: "pinned", Namespace: "default"}, persisted))
	assert.Equal(t, "repo/app:v1", persisted.Status.AppImage)

	// Idempotent: a second call with nothing to change issues no write.
	require.NoError(t, r.syncRecordedAppImage(context.Background(), chainNode))
	assert.Equal(t, "repo/app:v1", chainNode.Status.AppImage)
}
