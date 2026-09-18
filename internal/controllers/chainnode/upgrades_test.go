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

func TestGetUpgradeRefusesNamelessEntryForNamedAuthoritativeTarget(t *testing.T) {
	r := &Reconciler{}
	chainNode := &appsv1.ChainNode{Status: appsv1.ChainNodeStatus{Upgrades: []appsv1.Upgrade{{
		Height: 100,
		Image:  "repo/app:v2",
		Source: appsv1.OnChainUpgrade,
		Status: appsv1.UpgradeScheduled,
	}}}}

	got := r.getUpgrade(chainNode, nodeutils.RequiredUpgrade{Height: 100, Source: nodeutils.OnChainUpgrade, Name: "v2"})
	assert.Nil(t, got)
}

func TestResolveRequiredUpgradeMapsLegacySignalToLowestEligibleTarget(t *testing.T) {
	for _, tt := range []struct {
		name   string
		height int64
		want   int64
	}{
		{name: "legacy stop boundary selects next-height target", height: 99, want: 100},
		{name: "crosses target between legacy requests", height: 101, want: 100},
		{name: "late observation selects earliest pending target", height: 110, want: 100},
	} {
		t.Run(tt.name, func(t *testing.T) {
			node := &appsv1.ChainNode{Status: appsv1.ChainNodeStatus{Upgrades: []appsv1.Upgrade{
				{Height: 100, Name: "v2", Source: appsv1.OnChainUpgrade, Status: appsv1.UpgradeScheduled},
				{Height: 105, Name: "v3", Source: appsv1.ManualUpgrade, Status: appsv1.UpgradeOnGoing},
			}}}

			required, err := resolveRequiredUpgrade(node, nodeutils.UpgradeStatus{
				LatestHeight:          ptr.To(tt.height),
				LegacyUpgradeRequired: true,
			})

			require.NoError(t, err)
			require.NotNil(t, required)
			assert.Equal(t, tt.want, required.Height)
			assert.Equal(t, nodeutils.OnChainUpgrade, required.Source)
			assert.Equal(t, "v2", required.Name)
		})
	}
}

func TestResolveRequiredUpgradeRejectsLegacySignalWithoutEligibleTarget(t *testing.T) {
	node := &appsv1.ChainNode{Status: appsv1.ChainNodeStatus{Upgrades: []appsv1.Upgrade{
		{Height: 90, Source: appsv1.ManualUpgrade, Status: appsv1.UpgradeCompleted},
		{Height: 120, Source: appsv1.ManualUpgrade, Status: appsv1.UpgradeScheduled},
	}}}

	required, err := resolveRequiredUpgrade(node, nodeutils.UpgradeStatus{
		LatestHeight:          ptr.To(int64(110)),
		LegacyUpgradeRequired: true,
	})

	require.Error(t, err)
	assert.Nil(t, required)
	assert.Equal(t, appsv1.UpgradeScheduled, node.Status.Upgrades[1].Status)
}

func TestResolveRequiredUpgradePreservesStructuredTarget(t *testing.T) {
	want := &nodeutils.RequiredUpgrade{Height: 120, Source: nodeutils.OnChainUpgrade, Name: "v3"}
	required, err := resolveRequiredUpgrade(&appsv1.ChainNode{}, nodeutils.UpgradeStatus{RequiredUpgrade: want})

	require.NoError(t, err)
	assert.Equal(t, want, required)
}

func TestResolveRequiredUpgradeAllowsKnownGovernanceMarkerWhenDiscoveryDisabled(t *testing.T) {
	marker := &nodeutils.RequiredUpgrade{Height: 100, Source: nodeutils.OnChainUpgrade, Name: "v2"}
	for _, tt := range []struct {
		name     string
		upgrades []appsv1.UpgradeSpec
		status   []appsv1.Upgrade
	}{
		{
			name: "explicit force-on-chain configuration",
			upgrades: []appsv1.UpgradeSpec{{
				Height:       100,
				ForceOnChain: ptr.To(true),
			}},
		},
		{
			name: "known pending on-chain upgrade",
			status: []appsv1.Upgrade{{
				Height: 100,
				Source: appsv1.OnChainUpgrade,
				Status: appsv1.UpgradeImageMissing,
			}},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			node := &appsv1.ChainNode{
				Spec: appsv1.ChainNodeSpec{App: appsv1.AppSpec{
					CheckGovUpgrades: ptr.To(false),
					Upgrades:         tt.upgrades,
				}},
				Status: appsv1.ChainNodeStatus{LatestHeight: 99, Upgrades: tt.status},
			}

			required, err := resolveRequiredUpgrade(node, nodeutils.UpgradeStatus{RequiredUpgrade: marker})

			require.NoError(t, err)
			assert.Equal(t, marker, required)
		})
	}
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

func TestApplyUpgradeStatusRejectsMarkerBelowPersistedHeight(t *testing.T) {
	node := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "node", Namespace: "default"},
		Status: appsv1.ChainNodeStatus{
			LatestHeight: 200,
		},
	}
	scheme := gcpImportTestScheme(t)
	config := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "node-upgrades", Namespace: "default"},
		Data:       map[string]string{upgradesConfigFile: `{"upgrades":[]}`},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&appsv1.ChainNode{}).
		WithObjects(node, config).
		Build()
	r := &Reconciler{Client: c, Scheme: scheme}
	markerStatus := nodeutils.UpgradeStatus{RequiredUpgrade: &nodeutils.RequiredUpgrade{
		Height: 100,
		Source: nodeutils.OnChainUpgrade,
		Name:   "v2",
		Image:  "repo/app:v2",
	}}

	require.NoError(t, r.applyUpgradeStatus(t.Context(), node, markerStatus))
	assert.Empty(t, node.Status.Upgrades)
	required, err := resolveRequiredUpgrade(node, markerStatus)
	require.NoError(t, err)
	assert.Nil(t, required)
	assert.Nil(t, r.getUpgrade(node, *markerStatus.RequiredUpgrade))

	published := &corev1.ConfigMap{}
	require.NoError(t, c.Get(t.Context(), types.NamespacedName{Name: "node-upgrades", Namespace: "default"}, published))
	assert.JSONEq(t, `{"upgrades":[]}`, published.Data[upgradesConfigFile])
}

func TestApplyUpgradeStatusHonorsGovernanceDiscoveryPolicy(t *testing.T) {
	marker := nodeutils.UpgradeStatus{RequiredUpgrade: &nodeutils.RequiredUpgrade{
		Height: 100,
		Source: nodeutils.OnChainUpgrade,
		Name:   "v2",
		Image:  "repo/app:v2",
	}}
	for _, tt := range []struct {
		name         string
		app          appsv1.AppSpec
		known        []appsv1.Upgrade
		wantRecorded bool
	}{
		{
			name: "disabled discovery rejects unknown marker plan",
			app:  appsv1.AppSpec{CheckGovUpgrades: ptr.To(false)},
		},
		{
			name: "disabled discovery permits explicit forced plan",
			app: appsv1.AppSpec{
				CheckGovUpgrades: ptr.To(false),
				Upgrades: []appsv1.UpgradeSpec{{
					Height:       100,
					ForceOnChain: ptr.To(true),
				}},
			},
			wantRecorded: true,
		},
		{
			name: "disabled discovery permits known pending plan",
			app:  appsv1.AppSpec{CheckGovUpgrades: ptr.To(false)},
			known: []appsv1.Upgrade{{
				Height: 100,
				Source: appsv1.OnChainUpgrade,
				Status: appsv1.UpgradeImageMissing,
			}},
			wantRecorded: true,
		},
		{
			name:         "default discovery recovers unknown marker plan",
			app:          appsv1.AppSpec{},
			wantRecorded: true,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			node := &appsv1.ChainNode{
				ObjectMeta: metav1.ObjectMeta{Name: "node", Namespace: "default"},
				Spec:       appsv1.ChainNodeSpec{App: tt.app},
				Status: appsv1.ChainNodeStatus{
					LatestHeight: 99,
					Upgrades:     tt.known,
				},
			}
			scheme := gcpImportTestScheme(t)
			config := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: "node-upgrades", Namespace: "default"},
				Data:       map[string]string{upgradesConfigFile: `{"upgrades":[]}`},
			}
			c := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(&appsv1.ChainNode{}).
				WithObjects(node, config).
				Build()
			r := &Reconciler{Client: c, Scheme: scheme}

			require.NoError(t, r.applyUpgradeStatus(t.Context(), node, marker))
			selected := r.getUpgrade(node, *marker.RequiredUpgrade)
			published := &corev1.ConfigMap{}
			require.NoError(t, c.Get(t.Context(), types.NamespacedName{Name: "node-upgrades", Namespace: "default"}, published))
			if !tt.wantRecorded {
				assert.Empty(t, node.Status.Upgrades)
				assert.Nil(t, selected)
				assert.JSONEq(t, `{"upgrades":[]}`, published.Data[upgradesConfigFile])
				return
			}

			require.Len(t, node.Status.Upgrades, 1)
			assert.Equal(t, "v2", node.Status.Upgrades[0].Name)
			assert.Equal(t, "repo/app:v2", node.Status.Upgrades[0].Image)
			require.NotNil(t, selected)
			assert.Equal(t, "repo/app:v2", selected.Image)
			assert.Contains(t, published.Data[upgradesConfigFile], `"name":"v2"`)
			assert.Contains(t, published.Data[upgradesConfigFile], `"image":"repo/app:v2"`)
		})
	}
}

func TestLateManualAndLegacyRequiredUpgradesRemainSelectable(t *testing.T) {
	tests := []struct {
		name         string
		upgrade      appsv1.Upgrade
		status       nodeutils.UpgradeStatus
		wantLatest   int64
		wantRequired nodeutils.RequiredUpgrade
		wantImage    string
	}{
		{
			name: "structured manual requirement after target height",
			upgrade: appsv1.Upgrade{
				Height: 100,
				Name:   "manual-v2",
				Image:  "repo/app:manual-v2",
				Source: appsv1.ManualUpgrade,
				Status: appsv1.UpgradeScheduled,
			},
			status: nodeutils.UpgradeStatus{
				LatestHeight: ptr.To(int64(120)),
				RequiredUpgrade: &nodeutils.RequiredUpgrade{
					Height: 100,
					Source: nodeutils.ManualUpgrade,
					Name:   "manual-v2",
				},
			},
			wantLatest:   120,
			wantRequired: nodeutils.RequiredUpgrade{Height: 100, Source: nodeutils.ManualUpgrade, Name: "manual-v2"},
			wantImage:    "repo/app:manual-v2",
		},
		{
			name: "legacy requirement at target height",
			upgrade: appsv1.Upgrade{
				Height: 100,
				Name:   "v2",
				Image:  "repo/app:v2",
				Source: appsv1.OnChainUpgrade,
				Status: appsv1.UpgradeScheduled,
			},
			status: nodeutils.UpgradeStatus{
				LatestHeight:          ptr.To(int64(100)),
				LegacyUpgradeRequired: true,
			},
			wantLatest:   100,
			wantRequired: nodeutils.RequiredUpgrade{Height: 100, Source: nodeutils.OnChainUpgrade, Name: "v2"},
			wantImage:    "repo/app:v2",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := &appsv1.ChainNode{
				ObjectMeta: metav1.ObjectMeta{Name: "node", Namespace: "default"},
				Status: appsv1.ChainNodeStatus{
					LatestHeight: 99,
					Upgrades:     []appsv1.Upgrade{tt.upgrade},
				},
			}
			scheme := gcpImportTestScheme(t)
			c := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(&appsv1.ChainNode{}).
				WithObjects(node).
				Build()
			r := &Reconciler{Client: c, Scheme: scheme}

			require.NoError(t, r.applyUpgradeStatus(t.Context(), node, tt.status))
			assert.Equal(t, tt.wantLatest, node.Status.LatestHeight)
			required, err := resolveRequiredUpgrade(node, tt.status)
			require.NoError(t, err)
			require.NotNil(t, required)
			require.Equal(t, tt.wantRequired, *required)
			selected := r.getUpgrade(node, *required)
			require.NotNil(t, selected)
			assert.Equal(t, tt.wantImage, selected.Image)
		})
	}
}

func TestApplyUpgradeStatusRecordsAuthoritativeMarkerPlanBeforeImageSelection(t *testing.T) {
	node := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "node", Namespace: "default"},
		Spec:       appsv1.ChainNodeSpec{App: appsv1.AppSpec{App: "appd", Image: "repo/app"}},
		Status: appsv1.ChainNodeStatus{
			LatestHeight: 99,
			Upgrades: []appsv1.Upgrade{{
				Height: 100,
				Name:   "plan-a",
				Image:  "repo/app:a",
				Source: appsv1.OnChainUpgrade,
				Status: appsv1.UpgradeScheduled,
			}},
		},
	}
	scheme := gcpImportTestScheme(t)
	config := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "node-upgrades", Namespace: "default"},
		Data:       map[string]string{upgradesConfigFile: `{"upgrades":[{"height":100,"name":"plan-a","image":"repo/app:a","status":"scheduled","source":"on-chain"}]}`},
	}
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&appsv1.ChainNode{}).
		WithObjects(node, config).
		Build()
	r := &Reconciler{Client: c, Scheme: scheme}
	markerStatus := nodeutils.UpgradeStatus{
		LatestHeight: ptr.To(int64(99)),
		RequiredUpgrade: &nodeutils.RequiredUpgrade{
			Height: 100,
			Source: nodeutils.OnChainUpgrade,
			Name:   "plan-b",
		},
	}

	require.NoError(t, r.applyUpgradeStatus(t.Context(), node, markerStatus))
	require.Len(t, node.Status.Upgrades, 1)
	assert.Equal(t, appsv1.Upgrade{
		Height: 100,
		Name:   "plan-b",
		Source: appsv1.OnChainUpgrade,
		Status: appsv1.UpgradeImageMissing,
	}, node.Status.Upgrades[0])
	assert.Nil(t, r.getUpgrade(node, nodeutils.RequiredUpgrade{Height: 100, Source: nodeutils.OnChainUpgrade, Name: "plan-a"}))
	selected := r.getUpgrade(node, *markerStatus.RequiredUpgrade)
	require.NotNil(t, selected)
	assert.Equal(t, appsv1.UpgradeImageMissing, selected.Status)
	assert.Empty(t, selected.Image)

	published := &corev1.ConfigMap{}
	require.NoError(t, c.Get(t.Context(), types.NamespacedName{Name: "node-upgrades", Namespace: "default"}, published))
	assert.Contains(t, published.Data[upgradesConfigFile], `"name":"plan-b"`)
	assert.NotContains(t, published.Data[upgradesConfigFile], "repo/app:a")

	restarted := &Reconciler{Client: c, Scheme: scheme}
	require.NoError(t, restarted.applyUpgradeStatus(t.Context(), node, markerStatus))
	assert.Equal(t, "plan-b", node.Status.Upgrades[0].Name)
	assert.Empty(t, node.Status.Upgrades[0].Image)

	node.Spec.App.Upgrades = []appsv1.UpgradeSpec{{
		Height:       100,
		Name:         "plan-b",
		Image:        "repo/app:b",
		ForceOnChain: ptr.To(true),
	}}
	require.NoError(t, restarted.ensureUpgrades(t.Context(), node, false))
	selected = restarted.getUpgrade(node, *markerStatus.RequiredUpgrade)
	require.NotNil(t, selected)
	assert.Equal(t, "repo/app:b", selected.Image)
	assert.Equal(t, appsv1.UpgradeScheduled, selected.Status)
}

func TestApplyUpgradeStatusUsesMarkerImageWithoutOverwritingExplicitSamePlanImage(t *testing.T) {
	tests := []struct {
		name          string
		existingImage string
		markerImage   string
		wantImage     string
	}{
		{name: "fills missing image", markerImage: "repo/app:b", wantImage: "repo/app:b"},
		{name: "preserves explicit image", existingImage: "repo/custom:b", markerImage: "repo/app:b", wantImage: "repo/custom:b"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upgrades := []appsv1.Upgrade{{
				Height: 100,
				Name:   "plan-b",
				Image:  tt.existingImage,
				Source: appsv1.OnChainUpgrade,
				Status: appsv1.UpgradeImageMissing,
			}}

			got, changed := recordRequiredGovernanceUpgrade(upgrades, nodeutils.RequiredUpgrade{
				Height: 100,
				Source: nodeutils.OnChainUpgrade,
				Name:   "plan-b",
				Image:  tt.markerImage,
			})
			assert.True(t, changed)
			assert.Equal(t, tt.wantImage, got[0].Image)
			assert.Equal(t, appsv1.UpgradeScheduled, got[0].Status)
		})
	}
}

func TestApplyUpgradeStatusPreservesCompletedSkippedAndOngoingHistory(t *testing.T) {
	for _, phase := range []appsv1.UpgradePhase{appsv1.UpgradeCompleted, appsv1.UpgradeSkipped, appsv1.UpgradeOnGoing} {
		t.Run(string(phase), func(t *testing.T) {
			node := &appsv1.ChainNode{Status: appsv1.ChainNodeStatus{Upgrades: []appsv1.Upgrade{{
				Height: 100,
				Name:   "plan-a",
				Image:  "repo/app:a",
				Source: appsv1.OnChainUpgrade,
				Status: phase,
			}}}}
			r := &Reconciler{}

			require.NoError(t, r.applyUpgradeStatus(t.Context(), node, nodeutils.UpgradeStatus{RequiredUpgrade: &nodeutils.RequiredUpgrade{
				Height: 100,
				Source: nodeutils.OnChainUpgrade,
				Name:   "plan-b",
				Image:  "repo/app:b",
			}}))
			assert.Equal(t, "plan-a", node.Status.Upgrades[0].Name)
			assert.Equal(t, "repo/app:a", node.Status.Upgrades[0].Image)
			assert.Equal(t, phase, node.Status.Upgrades[0].Status)
		})
	}
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

func TestEnsureUpgradesBackfillsNamedGovernancePlanFromDocumentedForceOnChainSpec(t *testing.T) {
	node := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "node", Namespace: "default"},
		Spec: appsv1.ChainNodeSpec{App: appsv1.AppSpec{
			App:   "appd",
			Image: "repo/app",
			Upgrades: []appsv1.UpgradeSpec{{
				Height:       3000,
				Image:        "yourimage:yourtag",
				ForceOnChain: ptr.To(true),
			}},
		}},
		Status: appsv1.ChainNodeStatus{Upgrades: []appsv1.Upgrade{{
			Height: 3000,
			Name:   "plan-b",
			Source: appsv1.OnChainUpgrade,
			Status: appsv1.UpgradeImageMissing,
		}}},
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
	assert.Equal(t, appsv1.Upgrade{
		Height: 3000,
		Name:   "plan-b",
		Image:  "yourimage:yourtag",
		Source: appsv1.OnChainUpgrade,
		Status: appsv1.UpgradeScheduled,
	}, node.Status.Upgrades[0])
}

func TestEnsureUpgradesRejectsStaleNamelessForcedEntryOnManagedChild(t *testing.T) {
	node := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "set-fullnode-0",
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: appsv1.GroupVersion.String(),
				Kind:       "ChainNodeSet",
				Name:       "set",
				UID:        types.UID("set-uid"),
				Controller: ptr.To(true),
			}},
		},
		Spec: appsv1.ChainNodeSpec{App: appsv1.AppSpec{
			App:   "appd",
			Image: "repo/app",
			Upgrades: []appsv1.UpgradeSpec{{
				Height:       100,
				Image:        "repo/app:plan-a",
				ForceOnChain: ptr.To(true),
			}},
		}},
		Status: appsv1.ChainNodeStatus{Upgrades: []appsv1.Upgrade{{
			Height: 100,
			Name:   "plan-b",
			Source: appsv1.OnChainUpgrade,
			Status: appsv1.UpgradeImageMissing,
		}}},
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
	assert.Equal(t, appsv1.Upgrade{
		Height: 100,
		Name:   "plan-b",
		Source: appsv1.OnChainUpgrade,
		Status: appsv1.UpgradeImageMissing,
	}, node.Status.Upgrades[0])
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
			name:     "authoritative named plan replaces legacy nameless image",
			existing: appsv1.Upgrade{Height: 100, Image: "repo/plan-a:v2", Status: appsv1.UpgradeScheduled, Source: appsv1.OnChainUpgrade},
			incoming: appsv1.Upgrade{Height: 100, Name: "plan-b", Status: appsv1.UpgradeImageMissing, Source: appsv1.OnChainUpgrade},
			want:     appsv1.Upgrade{Height: 100, Name: "plan-b", Status: appsv1.UpgradeImageMissing, Source: appsv1.OnChainUpgrade},
		},
		{
			name:     "completed history",
			existing: appsv1.Upgrade{Height: 100, Name: "plan-a", Image: "repo/app:v2", Status: appsv1.UpgradeCompleted, Source: appsv1.OnChainUpgrade},
			incoming: appsv1.Upgrade{Height: 100, Name: "plan-b", Image: "repo/app:v2", Status: appsv1.UpgradeScheduled, Source: appsv1.OnChainUpgrade},
			want:     appsv1.Upgrade{Height: 100, Name: "plan-a", Image: "repo/app:v2", Status: appsv1.UpgradeCompleted, Source: appsv1.OnChainUpgrade},
		},
		{
			name:     "authoritative plan image replaces nameless image",
			existing: appsv1.Upgrade{Height: 100, Image: "repo/custom:v2", Status: appsv1.UpgradeScheduled, Source: appsv1.OnChainUpgrade},
			incoming: appsv1.Upgrade{Height: 100, Name: "plan-b", Image: "repo/plan:v2", Status: appsv1.UpgradeScheduled, Source: appsv1.OnChainUpgrade},
			want:     appsv1.Upgrade{Height: 100, Name: "plan-b", Image: "repo/plan:v2", Status: appsv1.UpgradeScheduled, Source: appsv1.OnChainUpgrade},
		},
		{
			name:     "manual override",
			existing: appsv1.Upgrade{Height: 100, Image: "repo/manual:v2", Status: appsv1.UpgradeScheduled, Source: appsv1.ManualUpgrade},
			incoming: appsv1.Upgrade{Height: 100, Name: "plan-b", Image: "repo/plan:v2", Status: appsv1.UpgradeScheduled, Source: appsv1.OnChainUpgrade},
			want:     appsv1.Upgrade{Height: 100, Image: "repo/manual:v2", Status: appsv1.UpgradeScheduled, Source: appsv1.ManualUpgrade},
		},
		{
			name:     "manual image backfills named governance plan",
			existing: appsv1.Upgrade{Height: 100, Name: "plan-b", Status: appsv1.UpgradeImageMissing, Source: appsv1.OnChainUpgrade},
			incoming: appsv1.Upgrade{Height: 100, Image: "repo/manual:v2", Status: appsv1.UpgradeOnGoing, Source: appsv1.OnChainUpgrade},
			want:     appsv1.Upgrade{Height: 100, Name: "plan-b", Image: "repo/manual:v2", Status: appsv1.UpgradeOnGoing, Source: appsv1.OnChainUpgrade},
		},
		{
			name:     "stale nameless forced plan cannot overwrite queried plan",
			existing: appsv1.Upgrade{Height: 100, Name: "plan-b", Image: "repo/plan:b", Status: appsv1.UpgradeScheduled, Source: appsv1.OnChainUpgrade},
			incoming: appsv1.Upgrade{Height: 100, Image: "repo/plan:a", Status: appsv1.UpgradeScheduled, Source: appsv1.OnChainUpgrade},
			want:     appsv1.Upgrade{Height: 100, Name: "plan-b", Image: "repo/plan:b", Status: appsv1.UpgradeScheduled, Source: appsv1.OnChainUpgrade},
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

func TestAuthoritativePlanThenDirectForceOnChainSpecUsesConfiguredImage(t *testing.T) {
	legacy := appsv1.Upgrade{
		Height: 100,
		Image:  "repo/plan-a:v2",
		Source: appsv1.OnChainUpgrade,
		Status: appsv1.UpgradeScheduled,
	}
	queried := appsv1.Upgrade{
		Height: 100,
		Name:   "plan-b",
		Source: appsv1.OnChainUpgrade,
		Status: appsv1.UpgradeImageMissing,
	}
	configured := appsv1.Upgrade{
		Height: 100,
		Image:  "yourimage:yourtag",
		Source: appsv1.OnChainUpgrade,
		Status: appsv1.UpgradeScheduled,
	}

	upgrades := AddOrUpdateUpgrade([]appsv1.Upgrade{legacy}, queried)
	upgrades = AddOrUpdateConfiguredUpgrade(upgrades, configured, false)
	require.Len(t, upgrades, 1)
	assert.Equal(t, appsv1.Upgrade{
		Height: 100,
		Name:   "plan-b",
		Image:  "yourimage:yourtag",
		Source: appsv1.OnChainUpgrade,
		Status: appsv1.UpgradeScheduled,
	}, upgrades[0])
}

func TestAddOrUpdateUpgradeRepeatedManualBackfillPreservesGovernanceIdentity(t *testing.T) {
	upgrades := []appsv1.Upgrade{{
		Height: 100,
		Name:   "plan-b",
		Source: appsv1.OnChainUpgrade,
		Status: appsv1.UpgradeImageMissing,
	}}
	manual := appsv1.Upgrade{
		Height: 100,
		Image:  "repo/manual:v2",
		Source: appsv1.ManualUpgrade,
		Status: appsv1.UpgradeScheduled,
	}

	upgrades = AddOrUpdateUpgrade(upgrades, manual)
	upgrades = AddOrUpdateUpgrade(upgrades, manual)
	require.Len(t, upgrades, 1)
	assert.Equal(t, appsv1.Upgrade{
		Height: 100,
		Name:   "plan-b",
		Image:  "repo/manual:v2",
		Source: appsv1.OnChainUpgrade,
		Status: appsv1.UpgradeScheduled,
	}, upgrades[0])

	replacement := appsv1.Upgrade{
		Height: 100,
		Name:   "plan-c",
		Image:  "repo/plan-c:v2",
		Source: appsv1.OnChainUpgrade,
		Status: appsv1.UpgradeScheduled,
	}
	upgrades = AddOrUpdateUpgrade(upgrades, replacement)
	assert.Equal(t, replacement, upgrades[0])
}

func TestAddOrUpdateUpgradeChangedManualBackfillPreservesGovernanceIdentity(t *testing.T) {
	upgrades := []appsv1.Upgrade{{
		Height: 100,
		Name:   "plan-b",
		Source: appsv1.OnChainUpgrade,
		Status: appsv1.UpgradeImageMissing,
	}}
	upgrades = AddOrUpdateUpgrade(upgrades, appsv1.Upgrade{
		Height: 100,
		Image:  "repo/manual:v2",
		Source: appsv1.ManualUpgrade,
		Status: appsv1.UpgradeScheduled,
	})
	corrected := appsv1.Upgrade{
		Height: 100,
		Image:  "repo/manual:v3",
		Source: appsv1.ManualUpgrade,
		Status: appsv1.UpgradeScheduled,
	}

	upgrades = AddOrUpdateUpgrade(upgrades, corrected)
	require.Len(t, upgrades, 1)
	assert.Equal(t, appsv1.Upgrade{
		Height: 100,
		Name:   "plan-b",
		Image:  "repo/manual:v3",
		Source: appsv1.OnChainUpgrade,
		Status: appsv1.UpgradeScheduled,
	}, upgrades[0])
}

func TestAddOrUpdateConfiguredUpgradeBackfillsManualImageForNamedPlan(t *testing.T) {
	existing := appsv1.Upgrade{
		Height: 100,
		Name:   "plan-b",
		Source: appsv1.OnChainUpgrade,
		Status: appsv1.UpgradeImageMissing,
	}
	configured := appsv1.Upgrade{
		Height: 100,
		Image:  "repo/manual:v2",
		Source: appsv1.ManualUpgrade,
		Status: appsv1.UpgradeScheduled,
	}

	got := AddOrUpdateConfiguredUpgrade([]appsv1.Upgrade{existing}, configured, false)
	require.Len(t, got, 1)
	assert.Equal(t, appsv1.Upgrade{
		Height: 100,
		Name:   "plan-b",
		Image:  "repo/manual:v2",
		Source: appsv1.OnChainUpgrade,
		Status: appsv1.UpgradeScheduled,
	}, got[0])
}

func TestAddOrUpdateConfiguredUpgradeRejectsNamelessOnChainImageForNamedPlan(t *testing.T) {
	existing := appsv1.Upgrade{
		Height: 100,
		Name:   "plan-b",
		Source: appsv1.OnChainUpgrade,
		Status: appsv1.UpgradeImageMissing,
	}
	staleConfigured := appsv1.Upgrade{
		Height: 100,
		Image:  "repo/plan-a:v2",
		Source: appsv1.OnChainUpgrade,
		Status: appsv1.UpgradeScheduled,
	}

	got := AddOrUpdateConfiguredUpgrade([]appsv1.Upgrade{existing}, staleConfigured, true)
	require.Len(t, got, 1)
	assert.Equal(t, existing, got[0])
}

func TestAddOrUpdateConfiguredUpgradePreservesTerminalAndOngoingGovernanceState(t *testing.T) {
	for _, status := range []appsv1.UpgradePhase{
		appsv1.UpgradeCompleted,
		appsv1.UpgradeSkipped,
		appsv1.UpgradeOnGoing,
	} {
		t.Run(string(status), func(t *testing.T) {
			existing := appsv1.Upgrade{
				Height: 100,
				Name:   "plan-b",
				Image:  "repo/app:v2",
				Source: appsv1.OnChainUpgrade,
				Status: status,
			}
			configured := appsv1.Upgrade{
				Height: 100,
				Name:   "plan-b",
				Image:  "repo/configured:v2",
				Source: appsv1.OnChainUpgrade,
				Status: appsv1.UpgradeScheduled,
			}

			got := AddOrUpdateConfiguredUpgrade([]appsv1.Upgrade{existing}, configured, true)
			require.Len(t, got, 1)
			assert.Equal(t, existing, got[0])
		})
	}
}

func TestAddOrUpdateConfiguredUpgradePreservesCompletedPlanFromStandaloneNamelessSpec(t *testing.T) {
	existing := appsv1.Upgrade{
		Height: 100,
		Name:   "plan-b",
		Image:  "repo/app:v2",
		Source: appsv1.OnChainUpgrade,
		Status: appsv1.UpgradeCompleted,
	}
	configured := appsv1.Upgrade{
		Height: 100,
		Image:  "repo/configured:v2",
		Source: appsv1.OnChainUpgrade,
		Status: appsv1.UpgradeScheduled,
	}

	got := AddOrUpdateConfiguredUpgrade([]appsv1.Upgrade{existing}, configured, false)
	require.Len(t, got, 1)
	assert.Equal(t, existing, got[0])
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
