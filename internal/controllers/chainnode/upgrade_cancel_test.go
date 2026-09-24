package chainnode

import (
	"context"
	"strings"
	"testing"

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

	appsv1 "github.com/voluzi/cosmopilot/v3/api/v1"
	"github.com/voluzi/cosmopilot/v3/pkg/nodeutils"
)

func newUpgradeCancelReconciler(t *testing.T, objs ...client.Object) (*Reconciler, *record.FakeRecorder) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&appsv1.ChainNode{}).WithObjects(objs...).Build()
	recorder := record.NewFakeRecorder(20)
	return &Reconciler{Client: c, Scheme: scheme, recorder: recorder}, recorder
}

func upgradeCancelNode(latest int64, upgrades ...appsv1.Upgrade) *appsv1.ChainNode {
	return &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "node", Namespace: "default", UID: "node-uid"},
		Status:     appsv1.ChainNodeStatus{LatestHeight: latest, Upgrades: upgrades},
	}
}

func upgradeStatusAt(t *testing.T, node *appsv1.ChainNode, height int64) appsv1.Upgrade {
	t.Helper()
	for _, u := range node.Status.Upgrades {
		if u.Height == height {
			return u
		}
	}
	t.Fatalf("no upgrade at height %d in %+v", height, node.Status.Upgrades)
	return appsv1.Upgrade{}
}

func drainEvents(recorder *record.FakeRecorder) string {
	var events []string
	for {
		select {
		case e := <-recorder.Events:
			events = append(events, e)
		default:
			return strings.Join(events, "\n")
		}
	}
}

func manualUpgrade(height int64, status appsv1.UpgradePhase) appsv1.Upgrade {
	return appsv1.Upgrade{Height: height, Image: "app:v2", Status: status, Source: appsv1.ManualUpgrade}
}

func govUpgrade(height int64, name string, status appsv1.UpgradePhase) appsv1.Upgrade {
	return appsv1.Upgrade{Height: height, Name: name, Image: "app:v2", Status: status, Source: appsv1.OnChainUpgrade}
}

func TestEnsureUpgradesCancelsRemovedManualUpgrade(t *testing.T) {
	for _, tc := range []struct {
		name       string
		latest     int64
		status     appsv1.UpgradePhase
		wantStatus appsv1.UpgradePhase
		wantEvent  string
	}{
		{name: "scheduled well before the height", latest: 50, status: appsv1.UpgradeScheduled, wantStatus: appsv1.UpgradeCancelled, wantEvent: appsv1.ReasonUpgradeCancelled},
		{name: "node already at the height before", latest: 99, status: appsv1.UpgradeScheduled, wantStatus: appsv1.UpgradeScheduled, wantEvent: appsv1.ReasonUpgradeCancelIgnored},
		{name: "ongoing", latest: 99, status: appsv1.UpgradeOnGoing, wantStatus: appsv1.UpgradeOnGoing, wantEvent: appsv1.ReasonUpgradeCancelIgnored},
		{name: "completed history is kept", latest: 120, status: appsv1.UpgradeCompleted, wantStatus: appsv1.UpgradeCompleted},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := upgradeCancelNode(tc.latest, manualUpgrade(100, tc.status))
			r, recorder := newUpgradeCancelReconciler(t, node)

			require.NoError(t, r.ensureUpgrades(context.Background(), node, false))

			assert.Equal(t, tc.wantStatus, upgradeStatusAt(t, node, 100).Status)
			events := drainEvents(recorder)
			if tc.wantEvent == "" {
				assert.Empty(t, events)
			} else {
				assert.Contains(t, events, tc.wantEvent)
			}
			cm := &corev1.ConfigMap{}
			require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "node-upgrades"}, cm))
			assert.Contains(t, cm.Data[upgradesConfigFile], `"status":"`+string(tc.wantStatus)+`"`)
		})
	}
}

func TestEnsureUpgradesLeavesGovernanceEntryWithoutSpec(t *testing.T) {
	node := upgradeCancelNode(50, govUpgrade(100, "v2", appsv1.UpgradeScheduled))
	r, _ := newUpgradeCancelReconciler(t, node)

	require.NoError(t, r.ensureUpgrades(context.Background(), node, false))
	assert.Equal(t, appsv1.UpgradeScheduled, upgradeStatusAt(t, node, 100).Status)
}

func TestEnsureUpgradesReschedulesReaddedManualUpgradeAndCorrectsImage(t *testing.T) {
	node := upgradeCancelNode(50, manualUpgrade(100, appsv1.UpgradeCancelled))
	node.Spec.App.Upgrades = []appsv1.UpgradeSpec{{Height: 100, Image: "app:v2-fixed"}}
	r, _ := newUpgradeCancelReconciler(t, node)

	require.NoError(t, r.ensureUpgrades(context.Background(), node, false))
	got := upgradeStatusAt(t, node, 100)
	assert.Equal(t, appsv1.UpgradeScheduled, got.Status)
	assert.Equal(t, "app:v2-fixed", got.Image)
	require.Len(t, node.Status.Upgrades, 1)

	node.Spec.App.Upgrades[0].Image = "app:v2-fixed-again"
	require.NoError(t, r.ensureUpgrades(context.Background(), node, false))
	assert.Equal(t, "app:v2-fixed-again", upgradeStatusAt(t, node, 100).Image, "a scheduled image can be corrected before the height")
}

// TestChainNodeSetChildCancelsManualUpgradeRemovedFromTheSet covers the round-trip: the set's status
// still lists the child's scheduled manual upgrade, but it must not come back into the child's spec.
func TestChainNodeSetChildCancelsManualUpgradeRemovedFromTheSet(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: "default", UID: "set-uid"},
		Status: appsv1.ChainNodeSetStatus{LatestHeight: 50, Upgrades: []appsv1.Upgrade{
			manualUpgrade(100, appsv1.UpgradeScheduled),
			{Height: 30, Image: "app:v1.5", Status: appsv1.UpgradeCompleted, Source: appsv1.ManualUpgrade},
		}},
	}
	child := upgradeCancelNode(50, manualUpgrade(100, appsv1.UpgradeScheduled))
	child.OwnerReferences = []metav1.OwnerReference{{
		APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNodeSet", Name: "set", UID: "set-uid", Controller: ptr.To(true),
	}}
	child.Spec.App = nodeSet.GetAppSpecWithUpgrades()
	require.Len(t, child.Spec.App.Upgrades, 1, "only finished manual history is propagated")
	assert.Equal(t, int64(30), child.Spec.App.Upgrades[0].Height)

	r, _ := newUpgradeCancelReconciler(t, nodeSet, child)
	require.NoError(t, r.ensureUpgrades(context.Background(), child, false))
	assert.Equal(t, appsv1.UpgradeCancelled, upgradeStatusAt(t, child, 100).Status)
}

func TestMergeGovUpgradesCancelsPlansTheChainNoLongerSchedules(t *testing.T) {
	for _, tc := range []struct {
		name   string
		plans  []appsv1.Upgrade
		want   map[int64]appsv1.UpgradePhase
		forced bool
	}{
		{name: "plan cancelled", want: map[int64]appsv1.UpgradePhase{100: appsv1.UpgradeCancelled}},
		{
			name:  "plan moved later",
			plans: []appsv1.Upgrade{govUpgrade(200, "v2", appsv1.UpgradeScheduled)},
			want:  map[int64]appsv1.UpgradePhase{100: appsv1.UpgradeCancelled, 200: appsv1.UpgradeScheduled},
		},
		{
			name:  "plan moved earlier",
			plans: []appsv1.Upgrade{govUpgrade(80, "v2", appsv1.UpgradeScheduled)},
			want:  map[int64]appsv1.UpgradePhase{100: appsv1.UpgradeCancelled, 80: appsv1.UpgradeScheduled},
		},
		{
			name:  "plan still scheduled",
			plans: []appsv1.Upgrade{govUpgrade(100, "v2", appsv1.UpgradeScheduled)},
			want:  map[int64]appsv1.UpgradePhase{100: appsv1.UpgradeScheduled},
		},
		{name: "user forced on-chain", forced: true, want: map[int64]appsv1.UpgradePhase{100: appsv1.UpgradeScheduled}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := upgradeCancelNode(50, govUpgrade(100, "v2", appsv1.UpgradeScheduled))
			if tc.forced {
				node.Spec.App.Upgrades = []appsv1.UpgradeSpec{{Height: 100, Image: "app:v2", ForceOnChain: ptr.To(true)}}
			}
			r, recorder := newUpgradeCancelReconciler(t, node)

			require.NoError(t, r.mergeGovUpgrades(context.Background(), node, tc.plans))
			for height, status := range tc.want {
				assert.Equal(t, status, upgradeStatusAt(t, node, height).Status, "height %d", height)
			}
			if tc.want[100] == appsv1.UpgradeCancelled {
				assert.Contains(t, drainEvents(recorder), appsv1.ReasonUpgradeRetired)
			}
		})
	}
}

func TestMergeGovUpgradesKeepsEntriesAtOrBelowTheCurrentHeight(t *testing.T) {
	node := upgradeCancelNode(100, govUpgrade(100, "v2", appsv1.UpgradeScheduled))
	r, _ := newUpgradeCancelReconciler(t, node)

	require.NoError(t, r.mergeGovUpgrades(context.Background(), node, nil))
	assert.Equal(t, appsv1.UpgradeScheduled, upgradeStatusAt(t, node, 100).Status)
}

func TestMergeGovUpgradesReschedulesPlanProposedAgainAtTheSameHeight(t *testing.T) {
	node := upgradeCancelNode(50, govUpgrade(100, "v2", appsv1.UpgradeCancelled))
	r, _ := newUpgradeCancelReconciler(t, node)

	require.NoError(t, r.mergeGovUpgrades(context.Background(), node, []appsv1.Upgrade{govUpgrade(100, "v2-fixed", appsv1.UpgradeScheduled)}))
	got := upgradeStatusAt(t, node, 100)
	assert.Equal(t, appsv1.UpgradeScheduled, got.Status)
	assert.Equal(t, "v2-fixed", got.Name)
	require.Len(t, node.Status.Upgrades, 1)
}

// TestChainNodeSetChildGovernanceCancellation covers a child whose spec carries a governance entry
// propagated from the set's status: only a forceOnChain entry in the set's own spec keeps it, and a
// propagated entry does not bring a cancelled one back after a restart.
func TestChainNodeSetChildGovernanceCancellation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		setForced  bool
		wantStatus appsv1.UpgradePhase
	}{
		{name: "propagated entry", wantStatus: appsv1.UpgradeCancelled},
		{name: "forced in the set spec", setForced: true, wantStatus: appsv1.UpgradeScheduled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nodeSet := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: "default", UID: "set-uid"}}
			if tc.setForced {
				nodeSet.Spec.App.Upgrades = []appsv1.UpgradeSpec{{Height: 100, Image: "app:v2", ForceOnChain: ptr.To(true)}}
			}
			child := upgradeCancelNode(50, govUpgrade(100, "v2", appsv1.UpgradeScheduled))
			child.OwnerReferences = []metav1.OwnerReference{{
				APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNodeSet", Name: "set", UID: "set-uid", Controller: ptr.To(true),
			}}
			child.Spec.App.Upgrades = []appsv1.UpgradeSpec{{Height: 100, Name: "v2", Image: "app:v2", ForceOnChain: ptr.To(true)}}
			r, _ := newUpgradeCancelReconciler(t, nodeSet, child)

			require.NoError(t, r.mergeGovUpgrades(context.Background(), child, nil))
			assert.Equal(t, tc.wantStatus, upgradeStatusAt(t, child, 100).Status)

			// A later reconcile without a governance query (for example after a controller restart)
			// merges the spec again and must keep the result.
			require.NoError(t, r.ensureUpgrades(context.Background(), child, false))
			assert.Equal(t, tc.wantStatus, upgradeStatusAt(t, child, 100).Status)
		})
	}
}

func TestResolveRequiredUpgradeLegacySignalIgnoresCancelledUpgrade(t *testing.T) {
	node := upgradeCancelNode(99, govUpgrade(100, "v2", appsv1.UpgradeCancelled))
	_, err := resolveRequiredUpgrade(node, nodeutils.UpgradeStatus{LegacyUpgradeRequired: true, LatestHeight: ptr.To(int64(99))})
	require.ErrorContains(t, err, "no pending upgrade is eligible")
}

func TestMergeGovUpgradesSchedulesPlanAtCancelledManualHeight(t *testing.T) {
	node := upgradeCancelNode(50, manualUpgrade(100, appsv1.UpgradeCancelled))
	r, _ := newUpgradeCancelReconciler(t, node)

	require.NoError(t, r.mergeGovUpgrades(context.Background(), node, []appsv1.Upgrade{govUpgrade(100, "v2", appsv1.UpgradeScheduled)}))
	got := upgradeStatusAt(t, node, 100)
	assert.Equal(t, appsv1.UpgradeScheduled, got.Status)
	assert.Equal(t, appsv1.OnChainUpgrade, got.Source)
	require.Len(t, node.Status.Upgrades, 1)
}
