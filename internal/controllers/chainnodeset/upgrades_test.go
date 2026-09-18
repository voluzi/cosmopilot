package chainnodeset

import (
	"testing"

	"github.com/stretchr/testify/assert"

	appsv1 "github.com/voluzi/cosmopilot/v3/api/v1"
)

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
