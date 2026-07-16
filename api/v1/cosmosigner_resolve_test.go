package v1

import (
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
}

// TestResolveCosmosignersPerGroup covers per-group .spec.nodes[].cosmosigner resolutions: one signer
// per group, for a single-instance validator group or a sentry group.
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
	assert.Equal(t, "vg-key", signers[0].SoftwareKeySecret)

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
	assert.Equal(t, "sentry-key", signers[0].SoftwareKeySecret)
}

func TestHasLegacyPerInstanceCosmosignerStatusIgnoresModernNumericGroup(t *testing.T) {
	nodeSet := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec: ChainNodeSetSpec{Nodes: []NodeGroupSpec{
			{Name: "foo", Validator: &NodeSetValidatorConfig{}, Cosmosigner: &Cosmosigner{}},
			{Name: "foo-1", Validator: &NodeSetValidatorConfig{}, Cosmosigner: &Cosmosigner{}},
		}},
		Status: ChainNodeSetStatus{Cosmosigners: []CosmosignerStatus{
			{Name: "cs-foo-signer", ServingGroup: "foo"},
			{Name: "cs-foo-1-signer", ServingGroup: "foo-1"},
		}},
	}

	assert.False(t, nodeSet.HasLegacyPerInstanceCosmosignerStatus("foo"))

	nodeSet.Status.Cosmosigners[1].ServingGroup = "foo"
	assert.True(t, nodeSet.HasLegacyPerInstanceCosmosignerStatus("foo"))

	nodeSet.Spec.Nodes[1].Validator = nil
	nodeSet.Status.Cosmosigners[1] = CosmosignerStatus{Name: "cs-foo-1-signer", AtEstablishment: ptr.To("")}
	assert.False(t, nodeSet.HasLegacyPerInstanceCosmosignerStatus("foo"))

	nodeSet.Status.Cosmosigners[1].AtEstablishment = nil
	assert.True(t, nodeSet.HasLegacyPerInstanceCosmosignerStatus("foo"))

	nodeSet.Spec.Nodes = nodeSet.Spec.Nodes[:1]
	nodeSet.Status.Cosmosigners[1] = CosmosignerStatus{Name: "cs-foo-1-signer", AtEstablishment: ptr.To("")}
	assert.False(t, nodeSet.HasLegacyPerInstanceCosmosignerStatus("foo"), "a removed modern sentry must not masquerade as a legacy per-instance validator signer")
}

// TestResolveCosmosignersMultiInstanceValidatorGroup verifies a multi-instance validator group with
// a cosmosigner is ONE validator: the webhook accepts it, it resolves to a single signer targeting
// the whole group (one consensus identity, N redundant signing endpoints), and an explicit
// privateKeySecret names that single identity. tmKMS on a multi-instance group stays rejected (a
// per-pod sidecar would make every instance sign independently with the same key).
func TestResolveCosmosignersMultiInstanceValidatorGroup(t *testing.T) {
	nodeSet := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec: ChainNodeSetSpec{
			App:     AppSpec{Image: "img", App: "appd", Version: ptr.To("1.0.0")},
			Genesis: &GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
			Nodes: []NodeGroupSpec{{
				Name:        "vg",
				Instances:   ptr.To(3),
				Validator:   &NodeSetValidatorConfig{},
				Cosmosigner: &Cosmosigner{Backend: vaultBackendFor("basekey")},
			}},
		},
	}
	_, err := nodeSet.Validate(nil)
	require.NoError(t, err, "multi-instance validator group with cosmosigner must be accepted (one identity, redundant endpoints)")

	signers := nodeSet.ResolveCosmosigners()
	require.Len(t, signers, 1, "one signer for the whole group, never one per instance")
	assert.Equal(t, "cs-vg-signer", signers[0].Name)
	assert.Equal(t, []string{"vg"}, signers[0].TargetGroups)
	assert.Equal(t, "vg", signers[0].ValidatorGroup)
	assert.Equal(t, "basekey", signers[0].Spec.Backend.Vault.KeyName, "no index-appended keys")
	assert.Equal(t, "cs-vg-0-priv-key", signers[0].SoftwareKeySecret, "default identity key is instance 0's")

	// An explicit privateKeySecret on the signer-targeted multi-instance group names the single
	// identity and is honored.
	explicit := nodeSet.DeepCopy()
	explicit.Spec.Nodes[0].Validator.PrivateKeySecret = ptr.To("vg-key")
	_, err = explicit.Validate(nil)
	require.NoError(t, err, "explicit privateKeySecret is allowed on a signer-targeted multi-instance group")
	signers = explicit.ResolveCosmosigners()
	require.Len(t, signers, 1)
	assert.Equal(t, "vg-key", signers[0].SoftwareKeySecret)

	// Without a cosmosigner the shared privateKeySecret stays rejected (N validators, one key).
	noSigner := explicit.DeepCopy()
	noSigner.Spec.Nodes[0].Cosmosigner = nil
	_, err = noSigner.Validate(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "privateKeySecret cannot be set when the validator group has multiple instances")

	// tmKMS on a multi-instance validator group stays rejected.
	tmkms := nodeSet.DeepCopy()
	tmkms.Spec.Nodes[0].Cosmosigner = nil
	tmkms.Spec.Nodes[0].Validator.TmKMS = &TmKMS{Provider: TmKmsProvider{Hashicorp: &TmKmsHashicorpProvider{
		Address: "https://vault:8200",
		Key:     "k",
		TokenSecret: &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: "vault-token"}, Key: "token"},
	}}}
	_, err = tmkms.Validate(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tmKMS cannot be set when the validator group has multiple instances")

	// A multi-instance SENTRY group with a cosmosigner stays valid too (one signer, whole group).
	sentry := nodeSet.DeepCopy()
	sentry.Spec.Nodes[0].Validator = nil
	sentry.Spec.Nodes[0].Cosmosigner.Backend = CosmosignerBackend{Software: &CosmosignerSoftwareBackend{PrivateKeySecret: ptr.To("sentry-key")}}
	_, err = sentry.Validate(nil)
	require.NoError(t, err)
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

// TestValidateCosmosignerSignerNameCollisions verifies a group whose own Service name equals a
// signer's derived resource name is rejected.
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

	// These group Services collide with a same-named standalone ChainNode's signer Services even when
	// this ChainNodeSet does not configure its own signer.
	for _, name := range []string{"signer", "signer-privval", "fullnodes-signer", "fullnodes-signer-privval"} {
		t.Run("reserved group "+name, func(t *testing.T) {
			_, err := base([]NodeGroupSpec{{Name: name, Instances: ptr.To(1)}}).Validate(nil)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "standalone ChainNode cosmosigner Service")
		})
	}

	for _, route := range []struct {
		name      string
		ingresses []GlobalIngressConfig
		gateways  []GlobalGatewayConfig
	}{
		{
			name: "standalone collision from global ingress",
			ingresses: []GlobalIngressConfig{{
				Name: "rpc-signer", Groups: []string{"fullnodes"}, Host: "nodes.example.com", EnableRPC: true,
			}},
		},
		{
			name: "standalone collision from global gateway",
			gateways: []GlobalGatewayConfig{{
				Name: "rpc-signer-privval", Groups: []string{"fullnodes"}, Host: "nodes.example.com", EnableRPC: true,
				Gateway: GatewayRef{Name: "gateway"},
			}},
		},
	} {
		t.Run(route.name, func(t *testing.T) {
			nodeSet := base([]NodeGroupSpec{{Name: "fullnodes", Instances: ptr.To(1)}})
			nodeSet.Spec.Ingresses = route.ingresses
			nodeSet.Spec.GatewayRoutes = route.gateways

			_, err := nodeSet.Validate(nil)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "standalone ChainNode cosmosigner Service")
		})
	}

	// Group Service name shadowing a signer resource name: group "vg-signer"'s Service is
	// cs-vg-signer, the raft Service of group "vg"'s signer.
	shadow := base([]NodeGroupSpec{
		{Name: "vg", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{PrivateKeySecret: ptr.To("k")},
			Cosmosigner: &Cosmosigner{Backend: vaultBackendFor("a")}},
		{Name: "vg-signer", Instances: ptr.To(1)},
	})
	_, err := shadow.Validate(nil)
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

	for _, route := range []struct {
		name      string
		ingresses []GlobalIngressConfig
		gateways  []GlobalGatewayConfig
	}{
		{
			name: "global ingress",
			ingresses: []GlobalIngressConfig{{
				Name: "signer", Groups: []string{"global"}, Host: "nodes.example.com", EnableRPC: true,
			}},
		},
		{
			name: "global gateway",
			gateways: []GlobalGatewayConfig{{
				Name: "signer", Groups: []string{"global"}, Host: "nodes.example.com", EnableRPC: true,
				Gateway: GatewayRef{Name: "gateway"},
			}},
		},
	} {
		t.Run(route.name, func(t *testing.T) {
			nodeSet := base([]NodeGroupSpec{{
				Name: "global", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{PrivateKeySecret: ptr.To("k")},
				Cosmosigner: &Cosmosigner{Backend: vaultBackendFor("a")},
			}})
			nodeSet.Spec.Ingresses = route.ingresses
			nodeSet.Spec.GatewayRoutes = route.gateways

			_, err := nodeSet.Validate(nil)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "global route Service")
		})
	}
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

	// REMOVING the signer entirely (the genesis key loses its only signing path) is rejected too.
	removed := base("genesis-sentry-key")
	removed.Spec.Nodes[0].Cosmosigner = nil
	_, err = removed.Validate(old)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "registered in the immutable genesis validator set")

	// A sentry signer whose key is NOT a genesis key stays rotatable.
	oldFree := base("free-key")
	oldFree.Spec.Nodes[0].Cosmosigner.Backend.Software.PrivateKeySecret = ptr.To("free-key")
	freeRotated := base("free-key-2")
	freeRotated.Spec.Nodes[0].Cosmosigner.Backend.Software.PrivateKeySecret = ptr.To("free-key-2")
	_, err = freeRotated.Validate(oldFree)
	require.NoError(t, err)
}

// TestGenesisValidatorPrivKeySecretsExcludesZeroInstance verifies a genesisValidators entry on a
// zero-instance group (which runs no validators and contributes nothing to genesis) is not collected
// as an on-chain genesis key, so a sentry key matching it is not treated as immutable.
func TestGenesisValidatorPrivKeySecretsExcludesZeroInstance(t *testing.T) {
	nodeSet := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec: ChainNodeSetSpec{
			Nodes: []NodeGroupSpec{
				{Name: "active", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{Init: &GenesisInitConfig{
					GenesisValidators: []GenesisValidator{{PrivKeySecret: "active-key"}},
				}}},
				{Name: "inactive", Instances: ptr.To(0), Validator: &NodeSetValidatorConfig{Init: &GenesisInitConfig{
					GenesisValidators: []GenesisValidator{{PrivKeySecret: "inactive-key"}},
				}}},
			},
		},
	}
	secrets := nodeSet.genesisValidatorPrivKeySecrets()
	_, active := secrets["active-key"]
	_, inactive := secrets["inactive-key"]
	assert.True(t, active, "active group's genesis key must be collected")
	assert.False(t, inactive, "zero-instance group's genesis key must be excluded")
}

// TestGenesisSentryEstablishmentIdentity verifies the marker helper returns an identity only for a
// SOFTWARE sentry whose key is registered in init.genesisValidators (the case the controller can prove
// from spec), and "" for validator-targeted signers, non-genesis sentries, and non-software sentries.
// This is what SetEstablishedChainID records and what ensureCosmosigner backfills for a genesis-sentry
// signer whose status entry is first created after establishment.
func TestGenesisSentryEstablishmentIdentity(t *testing.T) {
	nodeSet := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec: ChainNodeSetSpec{
			Validator: &NodeSetValidatorConfig{Init: &GenesisInitConfig{
				GenesisValidators: []GenesisValidator{{PrivKeySecret: "genesis-key"}},
			}},
			Nodes: []NodeGroupSpec{
				{Name: "gsentry", Cosmosigner: &Cosmosigner{Backend: CosmosignerBackend{Software: &CosmosignerSoftwareBackend{PrivateKeySecret: ptr.To("genesis-key")}}}},
				{Name: "fsentry", Cosmosigner: &Cosmosigner{Backend: CosmosignerBackend{Software: &CosmosignerSoftwareBackend{PrivateKeySecret: ptr.To("free-key")}}}},
			},
		},
	}

	byGroup := map[string]ResolvedSigner{}
	for _, s := range nodeSet.ResolveCosmosigners() {
		if len(s.TargetGroups) == 1 {
			byGroup[s.TargetGroups[0]] = s
		}
	}

	// Genesis-registered software sentry: records its key identity.
	gs := byGroup["gsentry"]
	if got := nodeSet.GenesisSentryEstablishmentIdentity(gs); got == "" || got != gs.Identity() {
		t.Fatalf("genesis sentry must record its identity, got %q", got)
	}
	// Non-genesis software sentry: records nothing.
	if got := nodeSet.GenesisSentryEstablishmentIdentity(byGroup["fsentry"]); got != "" {
		t.Fatalf("non-genesis sentry must record nothing, got %q", got)
	}
	// A validator-targeted signer: records nothing here (ValidatorTargetedIdentity handles it; nil marker
	// is how the no-webhook ADD guard detects post-establishment validator additions).
	validatorSigner := ResolvedSigner{ValidatorGroup: "validators", SoftwareKeySecret: "genesis-key", Spec: &Cosmosigner{Backend: CosmosignerBackend{Software: &CosmosignerSoftwareBackend{}}}}
	if got := nodeSet.GenesisSentryEstablishmentIdentity(validatorSigner); got != "" {
		t.Fatalf("validator-targeted signer must record nothing via the sentry helper, got %q", got)
	}
}
