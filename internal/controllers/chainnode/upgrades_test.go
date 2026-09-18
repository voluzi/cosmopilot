package chainnode

import (
	"encoding/json"
	"strings"
	"testing"

	upgradetypes "github.com/cosmos/cosmos-sdk/x/upgrade/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	appsv1 "github.com/voluzi/cosmopilot/v3/api/v1"
	"github.com/voluzi/cosmopilot/v3/pkg/nodeutils"
)

func TestUpgradeFromPlanPreservesPlanName(t *testing.T) {
	upgrade := upgradeFromPlan(&upgradetypes.Plan{
		Name:   "v2",
		Height: 100,
		Info:   `{"binaries":{"docker":"repo/app:v2"}}`,
	})

	assert.Equal(t, "v2", upgrade.Name)
	assert.Equal(t, int64(100), upgrade.Height)
	assert.Equal(t, "repo/app:v2", upgrade.Image)
}

func TestGetUpgrade(t *testing.T) {
	r := &Reconciler{}

	tests := []struct {
		name      string
		chainNode *appsv1.ChainNode
		required  nodeutils.RequiredUpgrade
		want      *appsv1.Upgrade
	}{
		{
			name: "finds matching scheduled upgrade",
			chainNode: &appsv1.ChainNode{
				Status: appsv1.ChainNodeStatus{
					Upgrades: []appsv1.Upgrade{
						{Height: 100, Image: "myapp:v1", Status: appsv1.UpgradeScheduled},
						{Height: 200, Image: "myapp:v2", Status: appsv1.UpgradeScheduled},
						{Height: 300, Image: "myapp:v3", Status: appsv1.UpgradeScheduled},
					},
				},
			},
			required: nodeutils.RequiredUpgrade{Height: 200},
			want:     &appsv1.Upgrade{Height: 200, Image: "myapp:v2", Status: appsv1.UpgradeScheduled},
		},
		{
			name: "no matching upgrade",
			chainNode: &appsv1.ChainNode{
				Status: appsv1.ChainNodeStatus{
					Upgrades: []appsv1.Upgrade{
						{Height: 100, Image: "myapp:v1", Status: appsv1.UpgradeScheduled},
						{Height: 200, Image: "myapp:v2", Status: appsv1.UpgradeScheduled},
					},
				},
			},
			required: nodeutils.RequiredUpgrade{Height: 150},
			want:     nil,
		},
		{
			name: "empty upgrades list",
			chainNode: &appsv1.ChainNode{
				Status: appsv1.ChainNodeStatus{
					Upgrades: []appsv1.Upgrade{},
				},
			},
			required: nodeutils.RequiredUpgrade{Height: 100},
			want:     nil,
		},
		{
			name: "finds scheduled upgrade at height",
			chainNode: &appsv1.ChainNode{
				Status: appsv1.ChainNodeStatus{
					Upgrades: []appsv1.Upgrade{
						{Height: 100, Image: "myapp:v1", Status: appsv1.UpgradeScheduled},
					},
				},
			},
			required: nodeutils.RequiredUpgrade{Height: 100},
			want:     &appsv1.Upgrade{Height: 100, Image: "myapp:v1", Status: appsv1.UpgradeScheduled},
		},
		{
			name: "ignores completed upgrades",
			chainNode: &appsv1.ChainNode{
				Status: appsv1.ChainNodeStatus{
					Upgrades: []appsv1.Upgrade{
						{Height: 100, Image: "myapp:v1", Status: appsv1.UpgradeCompleted},
						{Height: 200, Image: "myapp:v2", Status: appsv1.UpgradeScheduled},
					},
				},
			},
			required: nodeutils.RequiredUpgrade{Height: 100},
			want:     nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := r.getUpgrade(tt.chainNode, tt.required)
			if tt.want == nil {
				if got != nil {
					t.Errorf("getUpgrade() = %v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Errorf("getUpgrade() = nil, want %v", tt.want)
				return
			}
			if got.Height != tt.want.Height || got.Image != tt.want.Image || got.Status != tt.want.Status {
				t.Errorf("getUpgrade() = {Height: %d, Image: %s, Status: %s}, want {Height: %d, Image: %s, Status: %s}",
					got.Height, got.Image, got.Status, tt.want.Height, tt.want.Image, tt.want.Status)
			}
		})
	}
}

func TestGetUpgradeUsesExplicitTargetAtPreviousCommittedHeight(t *testing.T) {
	r := &Reconciler{}
	chainNode := &appsv1.ChainNode{Status: appsv1.ChainNodeStatus{
		LatestHeight: 99,
		Upgrades: []appsv1.Upgrade{
			{Height: 99, Name: "unrelated", Image: "repo/app:old", Source: appsv1.ManualUpgrade, Status: appsv1.UpgradeScheduled},
			{Height: 100, Name: "v2", Image: "repo/app:v2", Source: appsv1.OnChainUpgrade, Status: appsv1.UpgradeScheduled},
		},
	}}

	got := r.getUpgrade(chainNode, nodeutils.RequiredUpgrade{Height: 100, Source: nodeutils.OnChainUpgrade, Name: "v2"})
	require.NotNil(t, got)
	assert.Equal(t, "repo/app:v2", got.Image)
}

func TestGetUpgradeAllowsNamelessForcedOnChainEntry(t *testing.T) {
	r := &Reconciler{}
	chainNode := &appsv1.ChainNode{Status: appsv1.ChainNodeStatus{Upgrades: []appsv1.Upgrade{{
		Height: 100,
		Image:  "repo/app:v2",
		Source: appsv1.OnChainUpgrade,
		Status: appsv1.UpgradeScheduled,
	}}}}

	got := r.getUpgrade(chainNode, nodeutils.RequiredUpgrade{Height: 100, Source: nodeutils.OnChainUpgrade, Name: "v2"})
	require.NotNil(t, got)
	assert.Equal(t, "repo/app:v2", got.Image)
}

func TestAddOrUpdateUpgradeDoesNotSkipExistingLateUpgrade(t *testing.T) {
	got := AddOrUpdateUpgrade([]appsv1.Upgrade{{
		Height: 100,
		Name:   "v2",
		Image:  "repo/app:v2",
		Source: appsv1.ManualUpgrade,
		Status: appsv1.UpgradeScheduled,
	}}, appsv1.Upgrade{
		Height: 100,
		Name:   "v2",
		Image:  "repo/app:v2",
		Source: appsv1.ManualUpgrade,
		Status: appsv1.UpgradeScheduled,
	})

	assert.Equal(t, appsv1.UpgradeScheduled, got[0].Status)
}

func TestApplyUpgradeStatusPreservesHeightWhenObservationUnknown(t *testing.T) {
	chainNode := &appsv1.ChainNode{Status: appsv1.ChainNodeStatus{LatestHeight: 99}}
	r := &Reconciler{}

	require.NoError(t, r.applyUpgradeStatus(t.Context(), chainNode, nodeutils.UpgradeStatus{
		RequiredUpgrade: &nodeutils.RequiredUpgrade{Height: 100, Source: nodeutils.ManualUpgrade},
	}))
	assert.Equal(t, int64(99), chainNode.Status.LatestHeight)
}

func TestApplyUpgradeStatusPreservesHeightWhenObservationIsStale(t *testing.T) {
	chainNode := &appsv1.ChainNode{Status: appsv1.ChainNodeStatus{LatestHeight: 99}}
	r := &Reconciler{}
	latestHeight := int64(98)

	require.NoError(t, r.applyUpgradeStatus(t.Context(), chainNode, nodeutils.UpgradeStatus{
		LatestHeight: &latestHeight,
	}))
	assert.Equal(t, int64(99), chainNode.Status.LatestHeight)
}

func TestSelectedUpgradeImageFeedsPodAndConfigGenerationWithStaleHeight(t *testing.T) {
	tests := []struct {
		name          string
		latestHeight  int64
		status        appsv1.UpgradePhase
		recordedImage string
	}{
		{name: "completed", latestHeight: 99, status: appsv1.UpgradeCompleted, recordedImage: "repo/app:v2"},
		{name: "ongoing", latestHeight: 98, status: appsv1.UpgradeOnGoing},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			chainNode := &appsv1.ChainNode{
				Spec: appsv1.ChainNodeSpec{
					App:    appsv1.AppSpec{App: "appd", Image: "repo/app", Version: ptr.To("v1")},
					Config: &appsv1.Config{},
				},
				Status: appsv1.ChainNodeStatus{
					LatestHeight: tt.latestHeight,
					AppImage:     tt.recordedImage,
					Upgrades: []appsv1.Upgrade{{
						Height: 100,
						Image:  "repo/app:v2",
						Status: tt.status,
					}},
				},
			}

			cacheKey, err := configGenerationCacheKey(chainNode)
			require.NoError(t, err)
			assert.True(t, strings.HasPrefix(cacheKey, "repo/app:v2:"))
			container := (&Reconciler{}).buildAppContainer(chainNode, nil, "/ready", corev1.ResourceRequirements{}, nil)
			assert.Equal(t, "repo/app:v2", container.Image)
		})
	}
}

func TestEnsureUpgradesKeepsFirstLateManualUpgradeActionableAfterStatusRoundTrip(t *testing.T) {
	original := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "node", Namespace: "default"},
		Spec: appsv1.ChainNodeSpec{App: appsv1.AppSpec{
			App:      "appd",
			Image:    "repo/app",
			Upgrades: []appsv1.UpgradeSpec{{Height: 90, Image: "repo/app:v2"}},
		}},
		Status: appsv1.ChainNodeStatus{Phase: appsv1.PhaseChainNodeRunning, LatestHeight: 100},
	}
	body, err := json.Marshal(original)
	require.NoError(t, err)
	restored := &appsv1.ChainNode{}
	require.NoError(t, json.Unmarshal(body, restored))
	require.Nil(t, restored.Status.Upgrades)

	scheme := gcpImportTestScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&appsv1.ChainNode{}).
		WithObjects(restored).
		Build()
	r := &Reconciler{Client: c, Scheme: scheme}
	require.NoError(t, r.ensureUpgrades(t.Context(), restored, false))

	persisted := &appsv1.ChainNode{}
	require.NoError(t, c.Get(t.Context(), types.NamespacedName{Name: "node", Namespace: "default"}, persisted))
	require.Len(t, persisted.Status.Upgrades, 1)
	assert.Equal(t, appsv1.UpgradeScheduled, persisted.Status.Upgrades[0].Status)
	config := &corev1.ConfigMap{}
	require.NoError(t, c.Get(t.Context(), types.NamespacedName{Name: "node-upgrades", Namespace: "default"}, config))
	assert.Contains(t, config.Data[upgradesConfigFile], `"status":"scheduled"`)
}

func TestEnsureUpgradesAppliesHistoricalImageDuringFreshBootstrap(t *testing.T) {
	tests := []struct {
		name        string
		phase       appsv1.ChainNodePhase
		persistence *appsv1.Persistence
		stateSync   *bool
	}{
		{
			name:        "volume snapshot",
			persistence: &appsv1.Persistence{RestoreFromSnapshot: &appsv1.PvcSnapshot{Name: "snapshot"}},
		},
		{
			name:      "state sync",
			phase:     appsv1.PhaseChainNodeInitData,
			stateSync: ptr.To(true),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := &appsv1.ChainNode{
				ObjectMeta: metav1.ObjectMeta{Name: "node", Namespace: "default"},
				Spec: appsv1.ChainNodeSpec{
					App: appsv1.AppSpec{
						App:      "appd",
						Image:    "repo/app",
						Version:  ptr.To("v1"),
						Upgrades: []appsv1.UpgradeSpec{{Height: 100, Image: "repo/app:v2"}},
					},
					Persistence:      tt.persistence,
					StateSyncRestore: tt.stateSync,
				},
				Status: appsv1.ChainNodeStatus{Phase: tt.phase, LatestHeight: 200},
			}
			scheme := gcpImportTestScheme(t)
			c := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(&appsv1.ChainNode{}).
				WithObjects(node).
				Build()
			r := &Reconciler{Client: c, Scheme: scheme}

			require.NoError(t, r.ensureUpgrades(t.Context(), node, false))
			require.Len(t, node.Status.Upgrades, 1)
			assert.Equal(t, appsv1.UpgradeSkipped, node.Status.Upgrades[0].Status)
			assert.Equal(t, "repo/app:v2", node.GetAppImage())
		})
	}
}

func TestAddOrUpdateUpgradeRefreshesSameHeightGovernancePlan(t *testing.T) {
	tests := []struct {
		name     string
		existing appsv1.Upgrade
		incoming appsv1.Upgrade
		want     appsv1.Upgrade
	}{
		{
			name:     "scheduled plan replacement",
			existing: appsv1.Upgrade{Height: 100, Name: "plan-a", Image: "repo/app:v2", Status: appsv1.UpgradeScheduled, Source: appsv1.OnChainUpgrade},
			incoming: appsv1.Upgrade{Height: 100, Name: "plan-b", Image: "repo/app:v2", Status: appsv1.UpgradeScheduled, Source: appsv1.OnChainUpgrade},
			want:     appsv1.Upgrade{Height: 100, Name: "plan-b", Image: "repo/app:v2", Status: appsv1.UpgradeScheduled, Source: appsv1.OnChainUpgrade},
		},
		{
			name:     "completed history",
			existing: appsv1.Upgrade{Height: 100, Name: "plan-a", Image: "repo/app:v2", Status: appsv1.UpgradeCompleted, Source: appsv1.OnChainUpgrade},
			incoming: appsv1.Upgrade{Height: 100, Name: "plan-b", Image: "repo/app:v2", Status: appsv1.UpgradeScheduled, Source: appsv1.OnChainUpgrade},
			want:     appsv1.Upgrade{Height: 100, Name: "plan-a", Image: "repo/app:v2", Status: appsv1.UpgradeCompleted, Source: appsv1.OnChainUpgrade},
		},
		{
			name:     "forced on-chain image",
			existing: appsv1.Upgrade{Height: 100, Image: "repo/custom:v2", Status: appsv1.UpgradeScheduled, Source: appsv1.OnChainUpgrade},
			incoming: appsv1.Upgrade{Height: 100, Name: "plan-b", Image: "repo/plan:v2", Status: appsv1.UpgradeScheduled, Source: appsv1.OnChainUpgrade},
			want:     appsv1.Upgrade{Height: 100, Name: "plan-b", Image: "repo/custom:v2", Status: appsv1.UpgradeScheduled, Source: appsv1.OnChainUpgrade},
		},
		{
			name:     "manual override",
			existing: appsv1.Upgrade{Height: 100, Image: "repo/manual:v2", Status: appsv1.UpgradeScheduled, Source: appsv1.ManualUpgrade},
			incoming: appsv1.Upgrade{Height: 100, Name: "plan-b", Image: "repo/plan:v2", Status: appsv1.UpgradeScheduled, Source: appsv1.OnChainUpgrade},
			want:     appsv1.Upgrade{Height: 100, Image: "repo/manual:v2", Status: appsv1.UpgradeScheduled, Source: appsv1.ManualUpgrade},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AddOrUpdateUpgrade([]appsv1.Upgrade{tt.existing}, tt.incoming)
			require.Len(t, got, 1)
			assert.Equal(t, tt.want, got[0])
		})
	}
}

func TestAddOrUpdateUpgrade(t *testing.T) {
	tests := []struct {
		name     string
		upgrades []appsv1.Upgrade
		upgrade  appsv1.Upgrade
		wantLen  int
	}{
		{
			name:     "add new upgrade to empty list",
			upgrades: []appsv1.Upgrade{},
			upgrade:  appsv1.Upgrade{Height: 100, Image: "myapp:v1"},
			wantLen:  1,
		},
		{
			name: "add new upgrade to existing list",
			upgrades: []appsv1.Upgrade{
				{Height: 100, Image: "myapp:v1"},
			},
			upgrade: appsv1.Upgrade{Height: 200, Image: "myapp:v2"},
			wantLen: 2,
		},
		{
			name: "update existing upgrade with ImageMissing status",
			upgrades: []appsv1.Upgrade{
				{Height: 100, Image: "", Status: appsv1.UpgradeImageMissing},
			},
			upgrade: appsv1.Upgrade{Height: 100, Image: "new:v1", Source: appsv1.OnChainUpgrade},
			wantLen: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := AddOrUpdateUpgrade(tt.upgrades, tt.upgrade)
			if len(result) != tt.wantLen {
				t.Errorf("AddOrUpdateUpgrade() returned %d upgrades, want %d", len(result), tt.wantLen)
			}
		})
	}
}
