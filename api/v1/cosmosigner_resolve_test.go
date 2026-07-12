package v1

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func vaultBackendFor(key string) CosmosignerBackend {
	return CosmosignerBackend{Vault: &CosmosignerVaultBackend{
		Address:     "https://vault:8200",
		KeyName:     key,
		TokenSecret: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "vault-token"}, Key: "token"},
	}}
}

// TestResolveCosmosignersTopLevel covers the top-level .spec.cosmosigner resolutions.
func TestResolveCosmosignersTopLevel(t *testing.T) {
	// Legacy singleton validator (nodeGroups empty).
	legacy := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec: ChainNodeSetSpec{
			Validator:   &NodeSetValidatorConfig{PrivateKeySecret: ptr.To("val-key")},
			Cosmosigner: &Cosmosigner{Backend: vaultBackendFor("k")},
		},
	}
	signers := legacy.ResolveCosmosigners()
	require.Len(t, signers, 1)
	assert.Equal(t, "cs-signer", signers[0].Name)
	assert.Equal(t, []string{ReservedValidatorGroupName}, signers[0].TargetGroups)
	assert.Equal(t, ReservedValidatorGroupName, signers[0].ValidatorGroup)
	require.NotNil(t, signers[0].ValidatorInstance)
	assert.Equal(t, 0, *signers[0].ValidatorInstance)
	assert.Equal(t, "val-key", signers[0].SoftwareKeySecret)

	// Sentry over a regular group.
	sentry := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec: ChainNodeSetSpec{
			Nodes:       []NodeGroupSpec{{Name: "fullnodes", Instances: ptr.To(3)}},
			Cosmosigner: &Cosmosigner{NodeGroups: []string{"fullnodes"}, Backend: vaultBackendFor("k")},
		},
	}
	signers = sentry.ResolveCosmosigners()
	require.Len(t, signers, 1)
	assert.Equal(t, "cs-signer", signers[0].Name)
	assert.Equal(t, []string{"fullnodes"}, signers[0].TargetGroups)
	assert.Empty(t, signers[0].ValidatorGroup)
	assert.Nil(t, signers[0].ValidatorInstance)
}

// TestResolveCosmosignersPerGroup covers per-group .spec.nodes[].cosmosigner resolutions, including
// the one-signer-per-instance expansion of a multi-instance validator group.
func TestResolveCosmosignersPerGroup(t *testing.T) {
	// Single-instance validator group.
	single := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec: ChainNodeSetSpec{
			Nodes: []NodeGroupSpec{{
				Name:        "vg",
				Instances:   ptr.To(1),
				Validator:   &NodeSetValidatorConfig{PrivateKeySecret: ptr.To("vg-key")},
				Cosmosigner: &Cosmosigner{Backend: CosmosignerBackend{Software: &CosmosignerSoftwareBackend{}}},
			}},
		},
	}
	signers := single.ResolveCosmosigners()
	require.Len(t, signers, 1)
	assert.Equal(t, "cs-vg-signer", signers[0].Name)
	assert.Equal(t, "vg", signers[0].ValidatorGroup)
	require.NotNil(t, signers[0].ValidatorInstance)
	assert.Equal(t, 0, *signers[0].ValidatorInstance)
	assert.Equal(t, "vg-key", signers[0].SoftwareKeySecret)

	// Multi-instance validator group -> one signer per instance with index-appended Vault keys.
	multi := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec: ChainNodeSetSpec{
			Nodes: []NodeGroupSpec{{
				Name:        "vg",
				Instances:   ptr.To(3),
				Validator:   &NodeSetValidatorConfig{},
				Cosmosigner: &Cosmosigner{Backend: vaultBackendFor("basekey")},
			}},
		},
	}
	signers = multi.ResolveCosmosigners()
	require.Len(t, signers, 3)
	for i, s := range signers {
		assert.Equal(t, fmt.Sprintf("cs-vg-%d-signer", i), s.Name)
		assert.Equal(t, "vg", s.ValidatorGroup)
		require.NotNil(t, s.ValidatorInstance)
		assert.Equal(t, i, *s.ValidatorInstance)
		require.True(t, s.Spec.UsesVaultBackend())
		assert.Equal(t, fmt.Sprintf("basekey-%d", i), s.Spec.Backend.Vault.KeyName,
			"each per-instance signer holds a distinct index-appended Vault key")
		assert.Equal(t, fmt.Sprintf("cs-vg-%d-priv-key", i), s.SoftwareKeySecret)
	}

	// Sentry per-group signer on a regular group.
	sentry := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec: ChainNodeSetSpec{
			Nodes: []NodeGroupSpec{{
				Name:        "sg",
				Instances:   ptr.To(3),
				Cosmosigner: &Cosmosigner{Backend: CosmosignerBackend{Software: &CosmosignerSoftwareBackend{PrivateKeySecret: ptr.To("sentry-key")}}},
			}},
		},
	}
	signers = sentry.ResolveCosmosigners()
	require.Len(t, signers, 1)
	assert.Equal(t, "cs-sg-signer", signers[0].Name)
	assert.Empty(t, signers[0].ValidatorGroup)
	assert.Nil(t, signers[0].ValidatorInstance)
	assert.Equal(t, "sentry-key", signers[0].SoftwareKeySecret)
}

// TestResolveCosmosignersTopLevelPlusPerGroup verifies the two sources compose into independent signers.
func TestResolveCosmosignersTopLevelPlusPerGroup(t *testing.T) {
	nodeSet := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec: ChainNodeSetSpec{
			Cosmosigner: &Cosmosigner{NodeGroups: []string{"fullnodes"}, Backend: vaultBackendFor("topkey")},
			Nodes: []NodeGroupSpec{
				{Name: "fullnodes", Instances: ptr.To(2)},
				{Name: "vg", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{PrivateKeySecret: ptr.To("vg-key")},
					Cosmosigner: &Cosmosigner{Backend: vaultBackendFor("vgkey")}},
			},
		},
	}
	names := []string{}
	for _, s := range nodeSet.ResolveCosmosigners() {
		names = append(names, s.Name)
	}
	assert.ElementsMatch(t, []string{"cs-signer", "cs-vg-signer"}, names)
}

// TestValidateCosmosignerNameLengthRejected verifies the derived discovery Service name is bounded to
// 63 characters.
func TestValidateCosmosignerNameLengthRejected(t *testing.T) {
	longName := strings.Repeat("a", 50)
	nodeSet := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: longName},
		Spec: ChainNodeSetSpec{
			App:     AppSpec{Image: "img", App: "appd", Version: ptr.To("1.0.0")},
			Genesis: &GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
			Nodes: []NodeGroupSpec{{
				Name:        "vg",
				Instances:   ptr.To(1),
				Validator:   &NodeSetValidatorConfig{PrivateKeySecret: ptr.To("k")},
				Cosmosigner: &Cosmosigner{Backend: vaultBackendFor("vk")},
			}},
		},
	}
	_, err := nodeSet.Validate(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds the 63-character limit")
}
