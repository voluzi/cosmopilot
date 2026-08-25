package v1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// TestValidatorConfigGetAccountSecretName verifies account-mnemonic secret resolution for a
// ChainNode validator. A create-validator node must use its configured (funded) operator account
// secret instead of the generated <chainnode>-account default; init takes precedence when both are
// set (they are mutually exclusive in practice).
func TestValidatorConfigGetAccountSecretName(t *testing.T) {
	node := &ChainNode{ObjectMeta: metav1.ObjectMeta{Name: "mynode"}}

	tests := []struct {
		name string
		val  *ValidatorConfig
		want string
	}{
		{
			name: "no account configured falls back to the default",
			val:  &ValidatorConfig{},
			want: "mynode-account",
		},
		{
			name: "createValidator account secret is honored",
			val:  &ValidatorConfig{CreateValidator: &CreateValidatorConfig{AccountMnemonicSecret: ptr.To("funded-operator")}},
			want: "funded-operator",
		},
		{
			name: "createValidator without an account secret falls back to the default",
			val:  &ValidatorConfig{CreateValidator: &CreateValidatorConfig{}},
			want: "mynode-account",
		},
		{
			name: "init account secret is honored",
			val:  &ValidatorConfig{Init: &GenesisInitConfig{AccountMnemonicSecret: ptr.To("init-account")}},
			want: "init-account",
		},
		{
			name: "init account secret takes precedence over createValidator",
			val: &ValidatorConfig{
				Init:            &GenesisInitConfig{AccountMnemonicSecret: ptr.To("init-account")},
				CreateValidator: &CreateValidatorConfig{AccountMnemonicSecret: ptr.To("funded-operator")},
			},
			want: "init-account",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.val.GetAccountSecretName(node))
		})
	}
}

func TestChainNodeIsReady(t *testing.T) {
	stopsForSnapshots := &Persistence{Snapshots: &VolumeSnapshotsConfig{StopNode: ptr.To(true)}}

	tests := []struct {
		name        string
		phase       ChainNodePhase
		persistence *Persistence
		want        bool
	}{
		{name: "running", phase: PhaseChainNodeRunning, want: true},
		{name: "syncing", phase: PhaseChainNodeSyncing, want: true},
		{name: "snapshotting while still serving", phase: PhaseChainNodeSnapshotting, want: true},
		{
			name:        "snapshotting with stopNode is down",
			phase:       PhaseChainNodeSnapshotting,
			persistence: stopsForSnapshots,
			want:        false,
		},
		{
			name:        "running with stopNode configured but no snapshot in progress",
			phase:       PhaseChainNodeRunning,
			persistence: stopsForSnapshots,
			want:        true,
		},
		{name: "error", phase: PhaseChainNodeError, want: false},
		{name: "restarting", phase: PhaseChainNodeRestarting, want: false},
		{name: "stopped", phase: PhaseChainNodeStopped, want: false},
		{name: "no phase reported yet", phase: "", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := &ChainNode{
				Spec:   ChainNodeSpec{Persistence: tt.persistence},
				Status: ChainNodeStatus{Phase: tt.phase},
			}
			assert.Equal(t, tt.want, node.IsReady())
		})
	}
}

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
