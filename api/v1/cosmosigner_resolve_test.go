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

// TestValidateCosmosignerSignerNameCollisions verifies two distinct groups deriving the same signer
// resource name are rejected (e.g. a 2-instance group "foo" and a group "foo-0" both derive
// "<nodeset>-foo-0-signer"), and a group whose own Service name equals a signer's resource name is
// rejected too.
func TestValidateCosmosignerSignerNameCollisions(t *testing.T) {
	base := func(nodes []NodeGroupSpec) *ChainNodeSet {
		return &ChainNodeSet{
			ObjectMeta: metav1.ObjectMeta{Name: "cs"},
			Spec: ChainNodeSetSpec{
				App:     AppSpec{Image: "img", App: "appd", Version: ptr.To("1.0.0")},
				Genesis: &GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
				Nodes:   nodes,
			},
		}
	}

	// Duplicate derived signer name: multi-instance "foo" derives cs-foo-0-signer, and single-instance
	// "foo-0" derives cs-foo-0-signer too.
	dup := base([]NodeGroupSpec{
		{Name: "foo", Instances: ptr.To(2), Validator: &NodeSetValidatorConfig{},
			Cosmosigner: &Cosmosigner{Backend: vaultBackendFor("a")}},
		{Name: "foo-0", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{PrivateKeySecret: ptr.To("k")},
			Cosmosigner: &Cosmosigner{Backend: vaultBackendFor("b")}},
	})
	_, err := dup.Validate(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "also derives")

	// Group Service name shadowing a signer resource name: group "vg-signer"'s Service is
	// cs-vg-signer, the raft Service of group "vg"'s signer.
	shadow := base([]NodeGroupSpec{
		{Name: "vg", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{PrivateKeySecret: ptr.To("k")},
			Cosmosigner: &Cosmosigner{Backend: vaultBackendFor("a")}},
		{Name: "vg-signer", Instances: ptr.To(1)},
	})
	_, err = shadow.Validate(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "collides with a cosmosigner's derived resource name")

	// Group Service name shadowing a signer's discovery Service: group "vg-signer-privval"'s Service
	// is cs-vg-signer-privval.
	shadowPrivval := base([]NodeGroupSpec{
		{Name: "vg", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{PrivateKeySecret: ptr.To("k")},
			Cosmosigner: &Cosmosigner{Backend: vaultBackendFor("a")}},
		{Name: "vg-signer-privval", Instances: ptr.To(1)},
	})
	_, err = shadowPrivval.Validate(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "discovery Service name")
}

// TestValidateCosmosignerGcpMultiInstanceRejected verifies GCP KMS cannot back a multi-instance
// validator group: a keyVersion resource path cannot be index-derived per instance.
func TestValidateCosmosignerGcpMultiInstanceRejected(t *testing.T) {
	nodeSet := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec: ChainNodeSetSpec{
			App:     AppSpec{Image: "img", App: "appd", Version: ptr.To("1.0.0")},
			Genesis: &GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
			Nodes: []NodeGroupSpec{{
				Name:      "vg",
				Instances: ptr.To(3),
				Validator: &NodeSetValidatorConfig{},
				Cosmosigner: &Cosmosigner{Backend: CosmosignerBackend{
					GcpKMS: &CosmosignerGcpKmsBackend{KeyVersion: "projects/p/locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1"},
				}},
			}},
		},
	}
	_, err := nodeSet.Validate(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot be used on a multi-instance validator group")
}

// TestValidateCosmosignerSignerAdditionToEstablishedValidator verifies that adding a pre-provisioned
// signer to an ESTABLISHED validator that previously signed with its own local key is rejected (the
// validator would stop mounting the on-chain key), while a same-key software signer is accepted.
func TestValidateCosmosignerSignerAdditionToEstablishedValidator(t *testing.T) {
	base := func(c *Cosmosigner) *ChainNodeSet {
		return &ChainNodeSet{
			ObjectMeta: metav1.ObjectMeta{Name: "cs"},
			Spec: ChainNodeSetSpec{
				App:     AppSpec{Image: "img", App: "appd", Version: ptr.To("1.0.0")},
				Genesis: &GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
				Nodes: []NodeGroupSpec{{
					Name:        "vg",
					Instances:   ptr.To(1),
					Validator:   &NodeSetValidatorConfig{PrivateKeySecret: ptr.To("vg-key")},
					Cosmosigner: c,
				}},
			},
			Status: ChainNodeSetStatus{ChainID: "test-1"},
		}
	}
	old := base(nil) // established, signing through its own local key

	// Adding a pre-provisioned Vault signer: different effective identity -> rejected.
	vaultAdded := base(&Cosmosigner{Backend: vaultBackendFor("prekey")})
	_, err := vaultAdded.Validate(old)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "immutable after the chain is established")

	// Adding a software signer that uses the validator's own key: same identity -> accepted.
	softwareAdded := base(&Cosmosigner{Backend: CosmosignerBackend{Software: &CosmosignerSoftwareBackend{}}})
	_, err = softwareAdded.Validate(old)
	require.NoError(t, err)
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

// TestCosmosignerStateStorageEqual verifies size is compared as a parsed quantity (so 1024Mi==1Gi)
// and class is compared by pointer value (nil default distinct from explicit "").
func TestCosmosignerStateStorageEqual(t *testing.T) {
	// Quantity-equivalent sizes with matching (nil) class.
	assert.True(t, CosmosignerStateStorageEqual("1024Mi", nil, "1Gi", nil))
	// Different sizes.
	assert.False(t, CosmosignerStateStorageEqual("10Gi", nil, "20Gi", nil))
	// nil (cluster default) vs explicit "" are distinct.
	assert.False(t, CosmosignerStateStorageEqual("10Gi", nil, "10Gi", ptr.To("")))
	// Same explicit class.
	assert.True(t, CosmosignerStateStorageEqual("10Gi", ptr.To("fast"), "10240Mi", ptr.To("fast")))
	// Different class.
	assert.False(t, CosmosignerStateStorageEqual("10Gi", ptr.To("fast"), "10Gi", ptr.To("slow")))
	// Unparseable falls back to string equality.
	assert.True(t, CosmosignerStateStorageEqual("bogus", nil, "bogus", nil))
	assert.False(t, CosmosignerStateStorageEqual("bogus", nil, "other", nil))
}

// TestValidateCosmosignerGenesisSentryKeyImmutable verifies a sentry software signer whose key is
// registered in init.genesisValidators cannot switch to a different key after establishment (that key
// is part of the immutable genesis validator set), while a sentry key NOT in genesis stays rotatable.
func TestValidateCosmosignerGenesisSentryKeyImmutable(t *testing.T) {
	base := func(sentryKey string) *ChainNodeSet {
		return &ChainNodeSet{
			ObjectMeta: metav1.ObjectMeta{Name: "cs"},
			Spec: ChainNodeSetSpec{
				App: AppSpec{Image: "img", App: "appd", Version: ptr.To("1.0.0")},
				Validator: &NodeSetValidatorConfig{Init: &GenesisInitConfig{
					ChainID:     "test-1",
					Assets:      []string{"1stake"},
					StakeAmount: "1stake",
					GenesisValidators: []GenesisValidator{{
						PrivKeySecret:         "genesis-sentry-key",
						AccountMnemonicSecret: "mn",
						Moniker:               "preserved",
						Assets:                []string{"1stake"},
						StakeAmount:           "1stake",
					}},
				}},
				Nodes: []NodeGroupSpec{{
					Name:        "sentries",
					Instances:   ptr.To(3),
					Cosmosigner: &Cosmosigner{Backend: CosmosignerBackend{Software: &CosmosignerSoftwareBackend{PrivateKeySecret: ptr.To(sentryKey)}}},
				}},
			},
			Status: ChainNodeSetStatus{ChainID: "test-1"},
		}
	}

	// Established with the sentry signer using the genesis-registered key.
	old := base("genesis-sentry-key")

	// Rotating that key to a different one after establishment is rejected.
	rotated := base("some-other-key")
	_, err := rotated.Validate(old)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "registered in the immutable genesis validator set")

	// Unchanged: accepted.
	_, err = base("genesis-sentry-key").Validate(old)
	require.NoError(t, err)

	// A sentry signer whose key is NOT a genesis key stays rotatable.
	oldFree := base("free-key")
	oldFree.Spec.Nodes[0].Cosmosigner.Backend.Software.PrivateKeySecret = ptr.To("free-key")
	freeRotated := base("free-key-2")
	freeRotated.Spec.Nodes[0].Cosmosigner.Backend.Software.PrivateKeySecret = ptr.To("free-key-2")
	_, err = freeRotated.Validate(oldFree)
	require.NoError(t, err)
}
