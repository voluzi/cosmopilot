package v1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

func chainNodeWithUpgrades(image string, version *string, latestHeight int64, upgrades ...Upgrade) *ChainNode {
	return &ChainNode{
		Spec: ChainNodeSpec{
			App: AppSpec{Image: image, Version: version, App: "appd"},
		},
		Status: ChainNodeStatus{
			LatestHeight: latestHeight,
			Upgrades:     upgrades,
		},
	}
}

func TestGetAppImageUsesUpgradeImageVerbatim(t *testing.T) {
	// The regression this guards: an upgrade that moves the node to a different registry used to
	// contribute only its tag, producing `alloranetwork/allora-chain:986-test` — an image that does
	// not exist — instead of the image the upgrade actually named.
	chainNode := chainNodeWithUpgrades("alloranetwork/allora-chain", ptr.To("v0.8.2"), 10618000,
		Upgrade{Height: 10511421, Image: "registry.ops.allora.run/bryn-test/allorad:986-test", Status: UpgradeCompleted},
	)

	assert.Equal(t, "registry.ops.allora.run/bryn-test/allorad:986-test", chainNode.GetAppImage())
	assert.Equal(t, "986-test", chainNode.GetAppVersion())
}

func TestGetAppImageWithoutUpgradesUsesSpec(t *testing.T) {
	chainNode := chainNodeWithUpgrades("alloranetwork/allora-chain", ptr.To("v0.8.2"), 100)
	assert.Equal(t, "alloranetwork/allora-chain:v0.8.2", chainNode.GetAppImage())
	assert.Equal(t, "v0.8.2", chainNode.GetAppVersion())
}

func TestGetAppImageIgnoresUpgradesAboveLatestHeight(t *testing.T) {
	chainNode := chainNodeWithUpgrades("repo/app", ptr.To("v1"), 500,
		Upgrade{Height: 100, Image: "repo/app:v2", Status: UpgradeCompleted},
		Upgrade{Height: 900, Image: "repo/app:v3", Status: UpgradeScheduled},
	)
	assert.Equal(t, "repo/app:v2", chainNode.GetAppImage())
}

func TestGetAppImagePicksHighestReachedUpgrade(t *testing.T) {
	chainNode := chainNodeWithUpgrades("repo/app", ptr.To("v1"), 1000,
		Upgrade{Height: 300, Image: "repo/app:v3", Status: UpgradeCompleted},
		Upgrade{Height: 100, Image: "repo/app:v2", Status: UpgradeSkipped},
	)
	assert.Equal(t, "repo/app:v3", chainNode.GetAppImage())
}

func TestGetAppImageSkipsUpgradesWithoutImage(t *testing.T) {
	// An on-chain plan that carried no image must not drop the node back to the initial version.
	chainNode := chainNodeWithUpgrades("repo/app", ptr.To("v1"), 1000,
		Upgrade{Height: 100, Image: "repo/app:v2", Status: UpgradeCompleted},
		Upgrade{Height: 500, Image: "", Status: UpgradeImageMissing},
	)
	assert.Equal(t, "repo/app:v2", chainNode.GetAppImage())
}

func TestGetAppImageOverridePrecedence(t *testing.T) {
	base := func() *ChainNode {
		return chainNodeWithUpgrades("repo/app", ptr.To("v1"), 1000,
			Upgrade{Height: 100, Image: "other/app:v2", Status: UpgradeCompleted},
		)
	}

	t.Run("overrideVersion applies to the configured repository", func(t *testing.T) {
		chainNode := base()
		chainNode.Spec.OverrideVersion = ptr.To("v9")
		assert.Equal(t, "repo/app:v9", chainNode.GetAppImage())
	})

	t.Run("overrideImage replaces repository and tag", func(t *testing.T) {
		chainNode := base()
		chainNode.Spec.OverrideImage = ptr.To("registry.local:5000/team/app:test")
		assert.Equal(t, "registry.local:5000/team/app:test", chainNode.GetAppImage())
		assert.Equal(t, "test", chainNode.GetAppVersion())
	})

	t.Run("overrideImage wins over overrideVersion", func(t *testing.T) {
		chainNode := base()
		chainNode.Spec.OverrideVersion = ptr.To("v9")
		chainNode.Spec.OverrideImage = ptr.To("other/app:v10")
		assert.Equal(t, "other/app:v10", chainNode.GetAppImage())
	})
}

func TestGetAppImageWithRegistryPort(t *testing.T) {
	chainNode := chainNodeWithUpgrades("registry.local:5000/app", ptr.To("v1"), 1000,
		Upgrade{Height: 100, Image: "registry.local:5000/app:v2", Status: UpgradeCompleted},
	)
	assert.Equal(t, "registry.local:5000/app:v2", chainNode.GetAppImage())
	// A naive split on ":" would have yielded "5000/app" here.
	assert.Equal(t, "v2", chainNode.GetAppVersion())
}

func TestGetAppImagePullPolicyFollowsResolvedImage(t *testing.T) {
	// An upgrade onto `latest` must still pull every start, even though .spec.app.version is pinned.
	chainNode := chainNodeWithUpgrades("repo/app", ptr.To("v1"), 1000,
		Upgrade{Height: 100, Image: "repo/app:latest", Status: UpgradeCompleted},
	)
	assert.Equal(t, corev1.PullAlways, chainNode.GetAppImagePullPolicy())

	pinned := chainNodeWithUpgrades("repo/app", ptr.To("v1"), 1000)
	assert.Equal(t, corev1.PullIfNotPresent, pinned.GetAppImagePullPolicy())

	explicit := chainNodeWithUpgrades("repo/app", ptr.To("v1"), 1000)
	explicit.Spec.App.ImagePullPolicy = corev1.PullNever
	assert.Equal(t, corev1.PullNever, explicit.GetAppImagePullPolicy())
}

func TestGetLastUpgradeImageOnNodeSet(t *testing.T) {
	nodeSet := &ChainNodeSet{
		Spec: ChainNodeSetSpec{
			App: AppSpec{Image: "alloranetwork/allora-chain", Version: ptr.To("v0.8.2"), App: "allorad"},
		},
		Status: ChainNodeSetStatus{
			LatestHeight: 10618000,
			Upgrades: []Upgrade{
				{Height: 8824055, Image: "alloranetwork/allora-chain:v0.16.0", Status: UpgradeCompleted},
				{Height: 10511421, Image: "registry.ops.allora.run/bryn-test/allorad:986-test", Status: UpgradeCompleted},
			},
		},
	}

	assert.Equal(t, "registry.ops.allora.run/bryn-test/allorad:986-test", nodeSet.GetLastUpgradeImage())
	assert.Equal(t, "986-test", nodeSet.GetLastUpgradeVersion())
}

func TestValidateImageOverrides(t *testing.T) {
	assert.NoError(t, ValidateImageOverrides(".spec", nil, nil))
	assert.NoError(t, ValidateImageOverrides(".spec", ptr.To("v1"), nil))
	assert.NoError(t, ValidateImageOverrides(".spec", nil, ptr.To("repo/app:v1")))

	assert.Error(t, ValidateImageOverrides(".spec", ptr.To("v1"), ptr.To("repo/app:v1")))
	assert.Error(t, ValidateImageOverrides(".spec", nil, ptr.To("repo/app")))
}

func TestUpgradeImageWarnings(t *testing.T) {
	app := AppSpec{
		Image: "repo/app",
		Upgrades: []UpgradeSpec{
			{Height: 1, Image: "repo/app:v2"},
			{Height: 2, Image: "other/app:v3"},
			{Height: 3, Image: "repo/app"},
			{Height: 4, Image: ""},
		},
	}

	warnings := app.UpgradeImageWarnings(".spec.app")
	assert.Len(t, warnings, 2)
	assert.Contains(t, warnings[0], "upgrades[1]")
	assert.Contains(t, warnings[0], "other/app")
	assert.Contains(t, warnings[1], "upgrades[2]")
	assert.Contains(t, warnings[1], "no tag or digest")
}
