package chainnodeset

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	appsv1 "github.com/voluzi/cosmopilot/v3/api/v1"
	"github.com/voluzi/cosmopilot/v3/internal/controllers"
	childcontroller "github.com/voluzi/cosmopilot/v3/internal/controllers/chainnode"
)

func TestAggregateChildUpgradesConflictingPlansIsOrderIndependent(t *testing.T) {
	planA := appsv1.Upgrade{Height: 100, Name: "plan-a", Image: "repo/app:a", Source: appsv1.OnChainUpgrade, Status: appsv1.UpgradeScheduled}
	planB := appsv1.Upgrade{Height: 100, Name: "plan-b", Image: "repo/app:b", Source: appsv1.OnChainUpgrade, Status: appsv1.UpgradeScheduled}
	children := []appsv1.Upgrade{planA, planB, planB}
	permutations := [][]int{
		{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0},
	}
	want := appsv1.Upgrade{Height: 100, Source: appsv1.OnChainUpgrade, Status: appsv1.UpgradeImageMissing}

	for _, order := range permutations {
		nodes := make([]appsv1.ChainNode, 0, len(order))
		for _, index := range order {
			nodes = append(nodes, appsv1.ChainNode{Status: appsv1.ChainNodeStatus{Upgrades: []appsv1.Upgrade{children[index]}}})
		}
		got := aggregateChildUpgrades(nodes)
		require.Len(t, got, 1)
		assert.Equal(t, want, got[0], "order %v", order)
	}
}

func TestGetAppSpecWithUpgradesDoesNotPropagateConflictingPlan(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		Spec: appsv1.ChainNodeSetSpec{App: appsv1.AppSpec{Image: "repo/app", App: "appd"}},
		Status: appsv1.ChainNodeSetStatus{Upgrades: []appsv1.Upgrade{{
			Height: 100,
			Source: appsv1.OnChainUpgrade,
			Status: appsv1.UpgradeImageMissing,
		}}},
	}

	assert.Empty(t, nodeSet.GetAppSpecWithUpgrades().Upgrades)
}

func TestEnsureUpgradesConvergesFromPreviousPlanAfterChildrenReplaceIt(t *testing.T) {
	planA := appsv1.Upgrade{Height: 100, Name: "plan-a", Image: "repo/app:a", Source: appsv1.OnChainUpgrade, Status: appsv1.UpgradeScheduled}
	planB := appsv1.Upgrade{Height: 100, Name: "plan-b", Image: "repo/app:b", Source: appsv1.OnChainUpgrade, Status: appsv1.UpgradeScheduled}
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: "default"},
		Status:     appsv1.ChainNodeSetStatus{Upgrades: []appsv1.Upgrade{planA}},
	}
	child := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "set-fullnode-0",
			Namespace: "default",
			Labels:    map[string]string{controllers.LabelChainNodeSet: "set"},
		},
		Status: appsv1.ChainNodeStatus{Upgrades: []appsv1.Upgrade{planB}},
	}
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&appsv1.ChainNodeSet{}).
		WithObjects(nodeSet, child).
		Build()
	r := &Reconciler{Client: c, Scheme: scheme}

	require.NoError(t, r.ensureUpgrades(t.Context(), nodeSet))
	require.Len(t, nodeSet.Status.Upgrades, 1)
	assert.Equal(t, planB, nodeSet.Status.Upgrades[0])
}

func TestAggregateToChildRoundTripDoesNotRevertReplacementPlan(t *testing.T) {
	planA := appsv1.Upgrade{Height: 100, Name: "plan-a", Image: "repo/app:a", Source: appsv1.OnChainUpgrade, Status: appsv1.UpgradeScheduled}
	planB := appsv1.Upgrade{Height: 100, Name: "plan-b", Source: appsv1.OnChainUpgrade, Status: appsv1.UpgradeImageMissing}
	nodeSet := &appsv1.ChainNodeSet{
		Spec:   appsv1.ChainNodeSetSpec{App: appsv1.AppSpec{Image: "repo/app", App: "appd"}},
		Status: appsv1.ChainNodeSetStatus{Upgrades: []appsv1.Upgrade{planA}},
	}

	staleSpec := nodeSet.GetAppSpecWithUpgrades().Upgrades[0]
	assert.Equal(t, "plan-a", staleSpec.Name)
	child := childcontroller.AddOrUpdateConfiguredUpgrade([]appsv1.Upgrade{planB}, appsv1.Upgrade{
		Height: staleSpec.Height,
		Name:   staleSpec.Name,
		Image:  staleSpec.Image,
		Source: appsv1.OnChainUpgrade,
		Status: appsv1.UpgradeScheduled,
	})
	require.Len(t, child, 1)
	assert.Equal(t, planB, child[0])

	child = childcontroller.AddOrUpdateConfiguredUpgrade(child, appsv1.Upgrade{
		Height: 100,
		Name:   "plan-b",
		Image:  "repo/app:b",
		Source: appsv1.OnChainUpgrade,
		Status: appsv1.UpgradeScheduled,
	})
	require.Len(t, child, 1)
	assert.Equal(t, appsv1.Upgrade{
		Height: 100,
		Name:   "plan-b",
		Image:  "repo/app:b",
		Source: appsv1.OnChainUpgrade,
		Status: appsv1.UpgradeScheduled,
	}, child[0])
}

func TestAddOrUpdateUpgradeBackfillsMissingImage(t *testing.T) {
	upgrades := []appsv1.Upgrade{
		{
			Height: 44788180,
			Image:  "",
			Source: appsv1.OnChainUpgrade,
			Status: appsv1.UpgradeImageMissing,
		},
	}
	upgrade := appsv1.Upgrade{
		Height: 44788180,
		Image:  "ghcr.io/nibiruchain/nibiru:2.18.1",
		Source: appsv1.OnChainUpgrade,
		Status: appsv1.UpgradeCompleted,
	}

	got := AddOrUpdateUpgrade(upgrades, upgrade)

	if len(got) != 1 {
		t.Fatalf("AddOrUpdateUpgrade() returned %d upgrades, want 1", len(got))
	}
	if got[0].Image != upgrade.Image {
		t.Errorf("AddOrUpdateUpgrade() image = %q, want %q", got[0].Image, upgrade.Image)
	}
	if got[0].Status != appsv1.UpgradeCompleted {
		t.Errorf("AddOrUpdateUpgrade() status = %q, want %q", got[0].Status, appsv1.UpgradeCompleted)
	}
}

func TestAddOrUpdateUpgradePreservesName(t *testing.T) {
	got := AddOrUpdateUpgrade([]appsv1.Upgrade{{
		Height: 100,
		Image:  "repo/app:v2",
		Source: appsv1.OnChainUpgrade,
		Status: appsv1.UpgradeScheduled,
	}}, appsv1.Upgrade{
		Height: 100,
		Name:   "v2",
		Image:  "repo/app:v2",
		Source: appsv1.OnChainUpgrade,
		Status: appsv1.UpgradeScheduled,
	})

	assert.Equal(t, "v2", got[0].Name)
}

func TestAddOrUpdateUpgradeRefreshesScheduledPlanAtSameHeight(t *testing.T) {
	got := AddOrUpdateUpgrade([]appsv1.Upgrade{{
		Height: 100,
		Name:   "plan-a",
		Image:  "repo/app:a",
		Source: appsv1.OnChainUpgrade,
		Status: appsv1.UpgradeScheduled,
	}}, appsv1.Upgrade{
		Height: 100,
		Name:   "plan-b",
		Image:  "repo/app:b",
		Source: appsv1.OnChainUpgrade,
		Status: appsv1.UpgradeScheduled,
	})

	assert.Equal(t, "plan-b", got[0].Name)
	assert.Equal(t, "repo/app:b", got[0].Image)
}

func TestAddOrUpdateUpgradeSchedulesBackfilledMissingImage(t *testing.T) {
	upgrades := []appsv1.Upgrade{
		{
			Height: 44788180,
			Source: appsv1.OnChainUpgrade,
			Status: appsv1.UpgradeImageMissing,
		},
	}
	upgrade := appsv1.Upgrade{
		Height: 44788180,
		Image:  "ghcr.io/nibiruchain/nibiru:2.18.1",
		Source: appsv1.ManualUpgrade,
		Status: appsv1.UpgradeScheduled,
	}

	got := AddOrUpdateUpgrade(upgrades, upgrade)

	if got[0].Image != upgrade.Image {
		t.Errorf("AddOrUpdateUpgrade() image = %q, want %q", got[0].Image, upgrade.Image)
	}
	if got[0].Status != appsv1.UpgradeScheduled {
		t.Errorf("AddOrUpdateUpgrade() status = %q, want %q", got[0].Status, appsv1.UpgradeScheduled)
	}
	if got[0].Source != appsv1.OnChainUpgrade {
		t.Errorf("AddOrUpdateUpgrade() source = %q, want %q", got[0].Source, appsv1.OnChainUpgrade)
	}
}

func TestAddOrUpdateUpgradeDoesNotOverwriteExistingImage(t *testing.T) {
	upgrades := []appsv1.Upgrade{
		{
			Height: 100,
			Image:  "registry.example.com/app:v1",
			Source: appsv1.OnChainUpgrade,
			Status: appsv1.UpgradeScheduled,
		},
	}
	upgrade := appsv1.Upgrade{
		Height: 100,
		Image:  "registry.example.com/app:unexpected",
		Source: appsv1.OnChainUpgrade,
		Status: appsv1.UpgradeCompleted,
	}

	got := AddOrUpdateUpgrade(upgrades, upgrade)

	if got[0].Image != "registry.example.com/app:v1" {
		t.Errorf("AddOrUpdateUpgrade() image = %q, want existing image", got[0].Image)
	}
}
