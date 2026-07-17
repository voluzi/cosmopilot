package v1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func TestChainNodeSetValidateWarnsWhenTmKMSIsConfigured(t *testing.T) {
	tmkms := func(key string) *TmKMS {
		return &TmKMS{Provider: TmKmsProvider{Hashicorp: &TmKmsHashicorpProvider{
			Address: "https://vault:8200",
			Key:     key,
		}}}
	}

	t.Run("legacy validator", func(t *testing.T) {
		nodeSet := &ChainNodeSet{Spec: ChainNodeSetSpec{
			Genesis:   &GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
			Validator: &NodeSetValidatorConfig{TmKMS: tmkms("legacy-key")},
		}}
		warnings, err := nodeSet.Validate(nil)
		require.NoError(t, err)
		require.Equal(t, []string{
			".spec.validator.tmKMS is deprecated and will be removed in a future version; migrate to .spec.cosmosigner",
		}, []string(warnings))
	})

	t.Run("validator group", func(t *testing.T) {
		nodeSet := &ChainNodeSet{Spec: ChainNodeSetSpec{
			Genesis: &GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
			Nodes: []NodeGroupSpec{{
				Name:      "validators",
				Instances: ptr.To(1),
				Validator: &NodeSetValidatorConfig{TmKMS: tmkms("group-key")},
			}},
		}}
		warnings, err := nodeSet.Validate(nil)
		require.NoError(t, err)
		require.Equal(t, []string{
			".spec.nodes[0].validator.tmKMS is deprecated and will be removed in a future version; migrate to .spec.nodes[0].cosmosigner",
		}, []string(warnings))
	})
}

func TestChainNodeSetValidateGenesis(t *testing.T) {
	initConfig := &GenesisInitConfig{
		ChainID:     "test-localnet",
		Assets:      []string{"10000000unibi"},
		StakeAmount: "10000000unibi",
	}

	tests := []struct {
		name        string
		spec        ChainNodeSetSpec
		wantErr     bool
		errContains string
	}{
		{
			name: "genesis missing with no validator init is rejected",
			spec: ChainNodeSetSpec{
				Nodes: []NodeGroupSpec{{Name: "fullnodes"}},
			},
			wantErr:     true,
			errContains: ".spec.genesis is required except when initializing new genesis with .spec.validator.init",
		},
		{
			name: "genesis missing with single init validator is allowed",
			spec: ChainNodeSetSpec{
				Nodes:     []NodeGroupSpec{{Name: "fullnodes", Instances: ptr.To(1)}},
				Validator: &NodeSetValidatorConfig{Init: initConfig},
			},
			wantErr: false,
		},
		{
			name: "genesis missing with group init validator is allowed",
			spec: ChainNodeSetSpec{
				Nodes: []NodeGroupSpec{
					{Name: "validators", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{Init: initConfig}},
				},
			},
			wantErr: false,
		},
		{
			name: "genesis missing with mixed init and non-init validators is rejected",
			spec: ChainNodeSetSpec{
				Validator: &NodeSetValidatorConfig{Init: initConfig},
				Nodes: []NodeGroupSpec{
					{Name: "validators", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{}},
				},
			},
			wantErr:     true,
			errContains: ".spec.genesis is required when a validator does not initialize a new genesis",
		},
		{
			name: "genesis present with non-init validators is allowed",
			spec: ChainNodeSetSpec{
				Genesis: &GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
				Nodes: []NodeGroupSpec{
					{Name: "validators", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{}},
				},
			},
			wantErr: false,
		},
		{
			name: "more than one init validator is rejected",
			spec: ChainNodeSetSpec{
				Validator: &NodeSetValidatorConfig{Init: initConfig},
				Nodes: []NodeGroupSpec{
					{Name: "validators", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{Init: initConfig}},
				},
			},
			wantErr:     true,
			errContains: "only one ChainNodeSet validator can initialize genesis",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeSet := &ChainNodeSet{Spec: tt.spec}
			_, err := nodeSet.Validate(nil)
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errContains)
				return
			}
			assert.NoError(t, err)
		})
	}
}

// TestChainNodeSetValidateGcsExportCredentials verifies a ChainNodeSet rejects a GCS tarball export
// config unless exactly one of credentialsSecret or serviceAccountName (Workload Identity) is set. The
// config lives on a node group's persistence snapshots, which is validated the same way for every
// group and validator persistence block.
func TestChainNodeSetValidateGcsExportCredentials(t *testing.T) {
	credsSecret := &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "gcs-credentials"},
		Key:                  "credentials.json",
	}
	nodeSet := func(gcs *GcsExportConfig) *ChainNodeSet {
		return &ChainNodeSet{
			ObjectMeta: metav1.ObjectMeta{Name: "cns"},
			Spec: ChainNodeSetSpec{
				Genesis: &GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
				Nodes: []NodeGroupSpec{
					{
						Name:      "fullnodes",
						Instances: ptr.To(1),
						Persistence: &Persistence{
							Snapshots: &VolumeSnapshotsConfig{
								Frequency:     "24h",
								ExportTarball: &ExportTarballConfig{GCS: gcs},
							},
						},
					},
				},
			},
		}
	}

	t.Run("credentialsSecret only is allowed", func(t *testing.T) {
		_, err := nodeSet(&GcsExportConfig{Bucket: "b", CredentialsSecret: credsSecret}).Validate(nil)
		assert.NoError(t, err)
	})

	t.Run("serviceAccountName only is allowed", func(t *testing.T) {
		_, err := nodeSet(&GcsExportConfig{Bucket: "b", ServiceAccountName: ptr.To("gcs-uploader")}).Validate(nil)
		assert.NoError(t, err)
	})

	t.Run("both set is rejected", func(t *testing.T) {
		_, err := nodeSet(&GcsExportConfig{Bucket: "b", CredentialsSecret: credsSecret, ServiceAccountName: ptr.To("gcs-uploader")}).Validate(nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "mutually exclusive")
	})

	t.Run("neither set is rejected", func(t *testing.T) {
		_, err := nodeSet(&GcsExportConfig{Bucket: "b"}).Validate(nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must be set")
	})

	t.Run("empty serviceAccountName is rejected", func(t *testing.T) {
		_, err := nodeSet(&GcsExportConfig{Bucket: "b", ServiceAccountName: ptr.To("")}).Validate(nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "serviceAccountName must not be empty")
	})
}

func TestChainNodeSetValidateDuplicateGroupNames(t *testing.T) {
	tests := []struct {
		name    string
		nodes   []NodeGroupSpec
		wantErr bool
	}{
		{
			name: "unique group names are allowed",
			nodes: []NodeGroupSpec{
				{Name: "fullnodes", Instances: ptr.To(1)},
				{Name: "validators", Instances: ptr.To(1)},
			},
			wantErr: false,
		},
		{
			name: "duplicate group names are rejected",
			nodes: []NodeGroupSpec{
				{Name: "fullnodes", Instances: ptr.To(1)},
				{Name: "fullnodes", Instances: ptr.To(2)},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeSet := &ChainNodeSet{
				Spec: ChainNodeSetSpec{
					Genesis: &GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
					Nodes:   tt.nodes,
				},
			}
			_, err := nodeSet.Validate(nil)
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "duplicates")
				return
			}
			assert.NoError(t, err)
		})
	}
}

func TestChainNodeSetValidateReservedGroupName(t *testing.T) {
	nodeSet := &ChainNodeSet{
		Spec: ChainNodeSetSpec{
			Genesis: &GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
			Nodes: []NodeGroupSpec{
				{Name: ReservedValidatorGroupName, Instances: ptr.To(1)},
			},
		},
	}

	_, err := nodeSet.Validate(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "is reserved")
}

func TestChainNodeSetValidateMultiInstanceInitValidatorRejectsSharedSecrets(t *testing.T) {
	initConfig := &GenesisInitConfig{
		ChainID:     "test-localnet",
		Assets:      []string{"10000000unibi"},
		StakeAmount: "10000000unibi",
	}

	tests := []struct {
		name        string
		validator   *NodeSetValidatorConfig
		errContains string
	}{
		{
			name: "privateKeySecret is rejected",
			validator: &NodeSetValidatorConfig{
				PrivateKeySecret: ptr.To("shared-priv-key"),
				Init:             initConfig,
			},
			errContains: "privateKeySecret cannot be set",
		},
		{
			name: "init accountMnemonicSecret is rejected",
			validator: &NodeSetValidatorConfig{
				Init: &GenesisInitConfig{
					ChainID:               "test-localnet",
					Assets:                []string{"10000000unibi"},
					StakeAmount:           "10000000unibi",
					AccountMnemonicSecret: ptr.To("shared-account"),
				},
			},
			errContains: "accountMnemonicSecret cannot be set",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeSet := &ChainNodeSet{
				Spec: ChainNodeSetSpec{
					Nodes: []NodeGroupSpec{{
						Name:      "validators",
						Instances: ptr.To(2),
						Validator: tt.validator,
					}},
				},
			}

			_, err := nodeSet.Validate(nil)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.errContains)
		})
	}
}

// TestChainNodeSetValidateMultiInstanceValidatorRejectsSharedPrivateKey verifies that a shared
// privateKeySecret is rejected for any multi-instance validator group, not just genesis-init ones,
// since every instance must sign with its own consensus key.
func TestChainNodeSetValidateMultiInstanceValidatorRejectsSharedPrivateKey(t *testing.T) {
	nodeSet := &ChainNodeSet{
		Spec: ChainNodeSetSpec{
			Genesis: &GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
			Nodes: []NodeGroupSpec{{
				Name:      "validators",
				Instances: ptr.To(2),
				Validator: &NodeSetValidatorConfig{PrivateKeySecret: ptr.To("shared-priv-key")},
			}},
		},
	}
	_, err := nodeSet.Validate(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "privateKeySecret cannot be set when the validator group has multiple instances")

	// A single-instance group may set a privateKeySecret.
	nodeSet.Spec.Nodes[0].Instances = ptr.To(1)
	_, err = nodeSet.Validate(nil)
	assert.NoError(t, err)
}

// TestChainNodeSetValidateZeroInstanceValidatorGroup verifies a validator group with zero
// instances does not count toward the genesis requirements (it runs no validators).
func TestChainNodeSetValidateZeroInstanceValidatorGroup(t *testing.T) {
	nodeSet := &ChainNodeSet{
		Spec: ChainNodeSetSpec{
			Nodes: []NodeGroupSpec{{
				Name:      "validators",
				Instances: ptr.To(0),
				Validator: &NodeSetValidatorConfig{},
			}},
		},
	}
	// No genesis and no active validators: a zero-instance non-init validator group must not
	// trigger the "genesis required when a validator does not init" rejection.
	_, err := nodeSet.Validate(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), ".spec.genesis is required except when initializing new genesis")
	assert.NotContains(t, err.Error(), "when a validator does not initialize")

	initOnly := &ChainNodeSet{
		Spec: ChainNodeSetSpec{Nodes: []NodeGroupSpec{{
			Name:      "validators",
			Instances: ptr.To(0),
			Validator: &NodeSetValidatorConfig{Init: &GenesisInitConfig{ChainID: "test-localnet", Assets: []string{"1unibi"}, StakeAmount: "1unibi"}},
		}}},
		Status: ChainNodeSetStatus{ChainID: "test-localnet"},
	}
	assert.False(t, initOnly.ShouldInitGenesis(), "zero-instance validator.init must not count as an active genesis initializer")
	_, err = initOnly.Validate(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), ".spec.genesis is required")
}

// TestChainNodeSetValidateGenesisSetImmutableAfterCreation verifies that, once genesis has been
// created (old.status.chainID set), the genesis validator set is fixed: an init group cannot be
// scaled up, scaled down, replaced under a new name, or freshly added. Only keeping the same size
// is allowed after genesis. Changes before genesis remain unrestricted.
func TestChainNodeSetValidateGenesisSetImmutableAfterCreation(t *testing.T) {
	initConfig := &GenesisInitConfig{ChainID: "test-localnet", Assets: []string{"1unibi"}, StakeAmount: "1unibi"}
	mk := func(name string, instances int) *ChainNodeSet {
		return &ChainNodeSet{
			Spec: ChainNodeSetSpec{
				Nodes: []NodeGroupSpec{{
					Name:      name,
					Instances: ptr.To(instances),
					Validator: &NodeSetValidatorConfig{Init: initConfig},
				}},
			},
		}
	}
	created := func(ns *ChainNodeSet) *ChainNodeSet {
		ns.Status.ChainID = "test-localnet"
		return ns
	}

	// After genesis: scaling an init group up is rejected.
	_, err := mk("validators", 3).Validate(created(mk("validators", 2)))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot be scaled up after creation")

	// After genesis: scaling an init group down is rejected — the removed validators' voting power
	// stays in the immutable genesis, so dropping them can halt the chain.
	_, err = mk("validators", 1).Validate(created(mk("validators", 2)))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot be scaled down after creation")

	// After genesis: replacing the init group with a new name is rejected.
	_, err = mk("validators-new", 2).Validate(created(mk("validators", 2)))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot be added after genesis has been created")

	// After genesis: keeping the same size is allowed.
	_, err = mk("validators", 2).Validate(created(mk("validators", 2)))
	assert.NoError(t, err)

	// Before genesis (no chainID on old): scaling up and down are both allowed.
	_, err = mk("validators", 3).Validate(mk("validators", 2))
	assert.NoError(t, err)
	_, err = mk("validators", 1).Validate(mk("validators", 2))
	assert.NoError(t, err)
}

// TestChainNodeSetValidateMultiInstanceValidatorRejectsSharedTmKMS verifies a shared tmKMS config
// is rejected for a multi-instance validator group, since every instance would sign with the same
// external consensus key.
func TestChainNodeSetValidateMultiInstanceValidatorRejectsSharedTmKMS(t *testing.T) {
	nodeSet := &ChainNodeSet{
		Spec: ChainNodeSetSpec{
			Genesis: &GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
			Nodes: []NodeGroupSpec{{
				Name:      "validators",
				Instances: ptr.To(2),
				Validator: &NodeSetValidatorConfig{TmKMS: &TmKMS{}},
			}},
		},
	}
	_, err := nodeSet.Validate(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tmKMS cannot be set when the validator group has multiple instances")

	// A single-instance group may use tmKMS.
	nodeSet.Spec.Nodes[0].Instances = ptr.To(1)
	_, err = nodeSet.Validate(nil)
	assert.NoError(t, err)
}

// TestChainNodeSetValidateRejectsDuplicateSigningKeys verifies that two running validators may not
// reference the same explicit signing material — across validator groups and between the legacy
// .spec.validator and a group. Validators without explicit signing material (they get generated
// keys) and validators in zero-instance groups (they do not run) are not rejected.
func TestChainNodeSetValidateRejectsDuplicateSigningKeys(t *testing.T) {
	hashicorp := func(key string) *TmKMS {
		return &TmKMS{Provider: TmKmsProvider{Hashicorp: &TmKmsHashicorpProvider{
			Address: "https://vault.example.com:8200",
			Key:     key,
		}}}
	}

	tests := []struct {
		name        string
		validator   *NodeSetValidatorConfig
		nodes       []NodeGroupSpec
		wantErr     bool
		errContains string
	}{
		{
			name: "two single-instance groups sharing a privateKeySecret are rejected",
			nodes: []NodeGroupSpec{
				{Name: "a", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{PrivateKeySecret: ptr.To("shared")}},
				{Name: "b", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{PrivateKeySecret: ptr.To("shared")}},
			},
			wantErr:     true,
			errContains: "privateKeySecret",
		},
		{
			name:      "legacy validator and a group sharing a privateKeySecret are rejected",
			validator: &NodeSetValidatorConfig{PrivateKeySecret: ptr.To("shared")},
			nodes: []NodeGroupSpec{
				{Name: "a", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{PrivateKeySecret: ptr.To("shared")}},
			},
			wantErr:     true,
			errContains: "privateKeySecret",
		},
		{
			name: "two single-instance groups sharing a tmKMS key are rejected",
			nodes: []NodeGroupSpec{
				{Name: "a", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{TmKMS: hashicorp("same-key")}},
				{Name: "b", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{TmKMS: hashicorp("same-key")}},
			},
			wantErr:     true,
			errContains: "tmKMS references the same signing key",
		},
		{
			name: "distinct privateKeySecrets are allowed",
			nodes: []NodeGroupSpec{
				{Name: "a", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{PrivateKeySecret: ptr.To("key-a")}},
				{Name: "b", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{PrivateKeySecret: ptr.To("key-b")}},
			},
			wantErr: false,
		},
		{
			name: "distinct tmKMS keys are allowed",
			nodes: []NodeGroupSpec{
				{Name: "a", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{TmKMS: hashicorp("key-a")}},
				{Name: "b", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{TmKMS: hashicorp("key-b")}},
			},
			wantErr: false,
		},
		{
			name: "incomplete tmKMS hashicorp providers are ignored",
			nodes: []NodeGroupSpec{
				{Name: "a", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{TmKMS: &TmKMS{Provider: TmKmsProvider{Hashicorp: &TmKmsHashicorpProvider{}}}}},
				{Name: "b", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{TmKMS: &TmKMS{Provider: TmKmsProvider{Hashicorp: &TmKmsHashicorpProvider{}}}}},
			},
			wantErr: false,
		},
		{
			name: "validators without explicit signing material are allowed",
			nodes: []NodeGroupSpec{
				{Name: "a", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{}},
				{Name: "b", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{}},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeSet := &ChainNodeSet{
				Spec: ChainNodeSetSpec{
					Genesis:   &GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
					Validator: tt.validator,
					Nodes:     tt.nodes,
				},
			}
			_, err := nodeSet.Validate(nil)
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errContains)
				return
			}
			assert.NoError(t, err)
		})
	}

	// A validator in a zero-instance group does not run, so its signing material must not collide
	// with a running validator. (Asserted directly: a zero-instance group is independently rejected
	// by the snapshotNodeIndex check in the full Validate path.)
	t.Run("zero-instance group is not considered for duplicates", func(t *testing.T) {
		nodeSet := &ChainNodeSet{
			Spec: ChainNodeSetSpec{
				Nodes: []NodeGroupSpec{
					{Name: "a", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{PrivateKeySecret: ptr.To("shared")}},
					{Name: "b", Instances: ptr.To(0), Validator: &NodeSetValidatorConfig{PrivateKeySecret: ptr.To("shared")}},
				},
			},
		}
		assert.NoError(t, nodeSet.validateUniqueSigningKeys())
	})
}

// TestChainNodeSetValidateTmKMSSkipsDefaultPrivKey verifies that a validator using an external TmKMS
// signer does not reserve its local priv-key secret name — default OR explicit — when it never mounts
// that secret (no init, no uploaded create-validator key), so another validator may use that name. The
// TmKMS signing key identity is still registered, and the priv-key secret IS reserved when the
// validator actually uses/uploads a local key (init or create-validator with uploadGenerated).
func TestChainNodeSetValidateTmKMSSkipsDefaultPrivKey(t *testing.T) {
	hashicorp := func(key string) *TmKMS {
		return &TmKMS{Provider: TmKmsProvider{Hashicorp: &TmKmsHashicorpProvider{
			Address: "https://vault.example.com:8200",
			Key:     key,
		}}}
	}

	t.Run("group TmKMS default priv-key is free for another validator", func(t *testing.T) {
		// Group "a" uses TmKMS with no privateKeySecret: its default ns-a-0-priv-key must not be
		// reserved, so group "b" may name it explicitly.
		nodeSet := &ChainNodeSet{
			ObjectMeta: metav1.ObjectMeta{Name: "ns"},
			Spec: ChainNodeSetSpec{Nodes: []NodeGroupSpec{
				{Name: "a", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{TmKMS: hashicorp("key-a")}},
				{Name: "b", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{PrivateKeySecret: ptr.To("ns-a-0-priv-key")}},
			}},
		}
		assert.NoError(t, nodeSet.validateUniqueSigningKeys())
	})

	t.Run("legacy singleton TmKMS default priv-key is free for another validator", func(t *testing.T) {
		// The legacy singleton uses TmKMS with no privateKeySecret: its default ns-validator-priv-key
		// must not be reserved, so a group may name it explicitly.
		nodeSet := &ChainNodeSet{
			ObjectMeta: metav1.ObjectMeta{Name: "ns"},
			Spec: ChainNodeSetSpec{
				Validator: &NodeSetValidatorConfig{TmKMS: hashicorp("key-a")},
				Nodes: []NodeGroupSpec{
					{Name: "b", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{PrivateKeySecret: ptr.To("ns-validator-priv-key")}},
				},
			},
		}
		assert.NoError(t, nodeSet.validateUniqueSigningKeys())
	})

	t.Run("TmKMS signing key is still registered", func(t *testing.T) {
		// Even though the default priv-key is skipped, the TmKMS key identity must still be tracked, so
		// two TmKMS validators sharing a signing key are rejected.
		nodeSet := &ChainNodeSet{
			ObjectMeta: metav1.ObjectMeta{Name: "ns"},
			Spec: ChainNodeSetSpec{Nodes: []NodeGroupSpec{
				{Name: "a", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{TmKMS: hashicorp("shared")}},
				{Name: "b", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{TmKMS: hashicorp("shared")}},
			}},
		}
		err := nodeSet.validateUniqueSigningKeys()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "tmKMS references the same signing key")
	})

	t.Run("unused privateKeySecret on a pure TmKMS validator is not reserved", func(t *testing.T) {
		// Group "a" signs through TmKMS with no init and no uploaded create-validator key, so its
		// privateKeySecret is never mounted and must not be reserved — group "b" may use that name.
		nodeSet := &ChainNodeSet{
			ObjectMeta: metav1.ObjectMeta{Name: "ns"},
			Spec: ChainNodeSetSpec{Nodes: []NodeGroupSpec{
				{Name: "a", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{TmKMS: hashicorp("key-a"), PrivateKeySecret: ptr.To("shared")}},
				{Name: "b", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{PrivateKeySecret: ptr.To("shared")}},
			}},
		}
		assert.NoError(t, nodeSet.validateUniqueSigningKeys())
	})

	t.Run("privateKeySecret is reserved when the TmKMS validator uploads its generated key", func(t *testing.T) {
		// With create-validator + Hashicorp uploadGenerated, the controller creates and uploads the local
		// key, so its privateKeySecret is the real consensus key and must be reserved: a collision rejects.
		upload := hashicorp("key-a")
		upload.Provider.Hashicorp.UploadGenerated = true
		nodeSet := &ChainNodeSet{
			ObjectMeta: metav1.ObjectMeta{Name: "ns"},
			Spec: ChainNodeSetSpec{Nodes: []NodeGroupSpec{
				{Name: "a", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{TmKMS: upload, CreateValidator: &CreateValidatorConfig{}, PrivateKeySecret: ptr.To("shared")}},
				{Name: "b", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{PrivateKeySecret: ptr.To("shared")}},
			}},
		}
		err := nodeSet.validateUniqueSigningKeys()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "privateKeySecret")
	})

	t.Run("TmKMS init validator reserves its default priv-key (RequiresPrivKey creates and uploads it)", func(t *testing.T) {
		initConfig := &GenesisInitConfig{ChainID: "test-localnet", Assets: []string{"1u"}, StakeAmount: "1u"}
		// An init TmKMS validator still creates and uploads the local priv-key via RequiresPrivKey,
		// so its default ns-a-0-priv-key MUST be reserved and must conflict with a validator that
		// explicitly names that secret.
		nodeSet := &ChainNodeSet{
			ObjectMeta: metav1.ObjectMeta{Name: "ns"},
			Spec: ChainNodeSetSpec{Nodes: []NodeGroupSpec{
				{Name: "a", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{Init: initConfig, TmKMS: hashicorp("vault-key")}},
				{Name: "b", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{PrivateKeySecret: ptr.To("ns-a-0-priv-key")}},
			}},
		}
		err := nodeSet.validateUniqueSigningKeys()
		require.Error(t, err, "init TmKMS validator must reserve its default priv-key")
		assert.Contains(t, err.Error(), "ns-a-0-priv-key")
	})

	t.Run("TmKMS create-validator uploadGenerated reserves its default priv-key", func(t *testing.T) {
		// A create-validator TmKMS validator with uploadGenerated creates the local default
		// priv-key and uploads it to Vault, so the default ns-a-0-priv-key is real signing
		// material and must conflict with another validator naming it explicitly.
		nodeSet := &ChainNodeSet{
			ObjectMeta: metav1.ObjectMeta{Name: "ns"},
			Spec: ChainNodeSetSpec{Nodes: []NodeGroupSpec{
				{Name: "a", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{
					CreateValidator: &CreateValidatorConfig{},
					TmKMS: &TmKMS{Provider: TmKmsProvider{Hashicorp: &TmKmsHashicorpProvider{
						Address:         "https://vault.example.com:8200",
						Key:             "vault-key",
						UploadGenerated: true,
					}}},
				}},
				{Name: "b", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{PrivateKeySecret: ptr.To("ns-a-0-priv-key")}},
			}},
		}
		err := nodeSet.validateUniqueSigningKeys()
		require.Error(t, err, "uploadGenerated TmKMS create-validator must reserve its default priv-key")
		assert.Contains(t, err.Error(), "ns-a-0-priv-key")
	})
}

func TestChainNodeSetValidateReservesMigratedLocalKeySecret(t *testing.T) {
	base := func() *ChainNodeSet {
		return &ChainNodeSet{
			ObjectMeta: metav1.ObjectMeta{Name: "ns"},
			Spec: ChainNodeSetSpec{Nodes: []NodeGroupSpec{
				{
					Name:        "primary",
					Instances:   ptr.To(1),
					Validator:   &NodeSetValidatorConfig{PrivateKeySecret: ptr.To("shared")},
					Cosmosigner: &Cosmosigner{Backend: CosmosignerBackend{Vault: &CosmosignerVaultBackend{Address: "https://vault.example.com:8200", KeyName: "primary", TokenSecret: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "vault-token"}, Key: "token"}}}},
				},
				{Name: "secondary", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{PrivateKeySecret: ptr.To("shared")}},
			}},
		}
	}

	t.Run("post-establishment migration keeps the former local key reserved", func(t *testing.T) {
		nodeSet := base()
		nodeSet.Status.ChainID = "chain-1"
		err := nodeSet.validateUniqueSigningKeys()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "privateKeySecret")
	})

	t.Run("pre-establishment external signer leaves the local key unused", func(t *testing.T) {
		assert.NoError(t, base().validateUniqueSigningKeys())
	})

	t.Run("at-establishment signer proves the local key was unused", func(t *testing.T) {
		nodeSet := base()
		nodeSet.Status.ChainID = "chain-1"
		signer := nodeSet.ResolveCosmosigners()[0]
		identity := signer.ValidatorTargetedIdentity()
		nodeSet.Status.Cosmosigners = []CosmosignerStatus{{
			Name: signer.Name, ServingGroup: signer.ValidatorGroup, AtEstablishment: &identity,
			LocalKeyEverServed: ptr.To(false),
		}}
		assert.NoError(t, nodeSet.validateUniqueSigningKeys())
	})

	t.Run("round trip through the local key keeps it reserved", func(t *testing.T) {
		nodeSet := base()
		nodeSet.Status.ChainID = "chain-1"
		signer := nodeSet.ResolveCosmosigners()[0]
		identity := signer.ValidatorTargetedIdentity()
		nodeSet.Status.Cosmosigners = []CosmosignerStatus{{
			Name: signer.Name, ServingGroup: signer.ValidatorGroup, AtEstablishment: &identity,
			LocalKeyEverServed: ptr.To(true),
		}}
		err := nodeSet.validateUniqueSigningKeys()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "privateKeySecret")
	})

	t.Run("manifest placement move retains the unused-local-key proof", func(t *testing.T) {
		nodeSet := base()
		nodeSet.Status.ChainID = "chain-1"
		oldSigner := nodeSet.ResolveCosmosigners()[0]
		identity := oldSigner.ValidatorTargetedIdentity()
		nodeSet.Status.Cosmosigners = []CosmosignerStatus{{
			Name: oldSigner.Name, ServingGroup: oldSigner.ValidatorGroup, ServingIdentity: identity,
			AtEstablishment: &identity, LocalKeyEverServed: ptr.To(false),
		}}
		nodeSet.Spec.Cosmosigner = nodeSet.Spec.Nodes[0].Cosmosigner
		nodeSet.Spec.Cosmosigner.NodeGroups = []string{"primary"}
		nodeSet.Spec.Nodes[0].Cosmosigner = nil

		assert.NoError(t, nodeSet.validateUniqueSigningKeys())
	})

	t.Run("current local-key history overrides a stale placement proof", func(t *testing.T) {
		nodeSet := base()
		nodeSet.Status.ChainID = "chain-1"
		oldSigner := nodeSet.ResolveCosmosigners()[0]
		identity := oldSigner.ValidatorTargetedIdentity()
		nodeSet.Status.Cosmosigners = []CosmosignerStatus{{
			Name: oldSigner.Name, ServingGroup: oldSigner.ValidatorGroup, ServingIdentity: identity,
			AtEstablishment: &identity, LocalKeyEverServed: ptr.To(false),
		}}
		nodeSet.Spec.Cosmosigner = nodeSet.Spec.Nodes[0].Cosmosigner
		nodeSet.Spec.Cosmosigner.NodeGroups = []string{"primary"}
		nodeSet.Spec.Nodes[0].Cosmosigner = nil
		currentSigner := nodeSet.ResolveCosmosigners()[0]
		nodeSet.Status.Cosmosigners = append(nodeSet.Status.Cosmosigners, CosmosignerStatus{
			Name: currentSigner.Name, ServingGroup: currentSigner.ValidatorGroup, ServingIdentity: identity,
			AtEstablishment: &identity, LocalKeyEverServed: ptr.To(true),
		})

		err := nodeSet.validateUniqueSigningKeys()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "privateKeySecret")
	})
}

func TestChainNodeSetValidateRejectsCreateValidatorTmKMSWithoutUploadedKey(t *testing.T) {
	tmkms := &TmKMS{Provider: TmKmsProvider{Hashicorp: &TmKmsHashicorpProvider{
		Address: "https://vault.example.com:8200",
		Key:     "validator-key",
	}}}

	mk := func(v *NodeSetValidatorConfig) *ChainNodeSet {
		return &ChainNodeSet{Spec: ChainNodeSetSpec{
			Genesis: &GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
			Nodes: []NodeGroupSpec{{
				Name:      "validators",
				Instances: ptr.To(1),
				Validator: v,
			}},
		}}
	}

	_, err := mk(&NodeSetValidatorConfig{
		CreateValidator: &CreateValidatorConfig{},
		TmKMS:           tmkms,
	}).Validate(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires hashicorp.uploadGenerated=true")

	// Uploading the generated key to the KMS makes the registered create-validator pubkey match the
	// key the pod signs with.
	withUpload := tmkms.DeepCopy()
	withUpload.Provider.Hashicorp.UploadGenerated = true
	_, err = mk(&NodeSetValidatorConfig{
		CreateValidator: &CreateValidatorConfig{},
		TmKMS:           withUpload,
	}).Validate(nil)
	assert.NoError(t, err)

	// An explicit privateKeySecret does NOT exempt the requirement: the pod still signs through the
	// KMS sidecar and never mounts the secret, so without uploadGenerated the registered (local)
	// pubkey would not match the KMS signing key.
	_, err = mk(&NodeSetValidatorConfig{
		CreateValidator:  &CreateValidatorConfig{},
		TmKMS:            tmkms,
		PrivateKeySecret: ptr.To("explicit-priv-key"),
	}).Validate(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "requires hashicorp.uploadGenerated=true")

	// privateKeySecret together with uploadGenerated=true is accepted: that local key is uploaded to
	// the KMS, so the registered pubkey matches the signing key.
	_, err = mk(&NodeSetValidatorConfig{
		CreateValidator:  &CreateValidatorConfig{},
		TmKMS:            withUpload,
		PrivateKeySecret: ptr.To("explicit-priv-key"),
	}).Validate(nil)
	assert.NoError(t, err)
}

// TestChainNodeSetValidateAllowsNonInitValidatorAfterGenesis verifies that, once a chain is running
// (old.status.chainID set), a non-init validator can be added with no .spec.genesis — the controller
// derives the genesis from the existing <chainID>-genesis ConfigMap. On create, or before the chain
// exists, genesis is still required for non-init validators.
func TestChainNodeSetValidateAllowsNonInitValidatorAfterGenesis(t *testing.T) {
	initConfig := &GenesisInitConfig{ChainID: "test-localnet", Assets: []string{"1unibi"}, StakeAmount: "1unibi"}

	// A running chain that was initialized via a group validator.init.
	old := &ChainNodeSet{
		Spec: ChainNodeSetSpec{
			Nodes: []NodeGroupSpec{
				{Name: "validators", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{Init: initConfig}},
			},
		},
		Status: ChainNodeSetStatus{ChainID: "test-localnet"},
	}

	// Adding a non-init validator group with no .spec.genesis on a running chain is allowed.
	updated := &ChainNodeSet{
		Spec: ChainNodeSetSpec{
			Nodes: []NodeGroupSpec{
				{Name: "validators", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{Init: initConfig}},
				{Name: "joiners", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{CreateValidator: &CreateValidatorConfig{}}},
			},
		},
	}
	_, err := updated.Validate(old)
	assert.NoError(t, err)

	// On create (old == nil) the same spec is rejected: there is no existing genesis to consume.
	_, err = updated.Validate(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), ".spec.genesis is required")

	// Before the chain exists (old has no chainID) it is likewise rejected.
	_, err = updated.Validate(&ChainNodeSet{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), ".spec.genesis is required")
}

// TestChainNodeSetValidateRejectsDefaultPrivateKeySecretCollision verifies that a validator
// explicitly setting another running validator's *default* private-key secret name is rejected:
// both would resolve to the same secret and sign with the same consensus key. Default names match
// the generated ChainNodes — <nodeset>-validator-priv-key for the legacy singleton and
// <nodeset>-<group>-<index>-priv-key for group validators.
func TestChainNodeSetValidateRejectsDefaultPrivateKeySecretCollision(t *testing.T) {
	tests := []struct {
		name      string
		validator *NodeSetValidatorConfig
		nodes     []NodeGroupSpec
		wantErr   bool
	}{
		{
			name:      "explicit secret colliding with the legacy singleton default is rejected",
			validator: &NodeSetValidatorConfig{}, // resolves to ns-validator-priv-key
			nodes: []NodeGroupSpec{
				{Name: "extra", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{PrivateKeySecret: ptr.To("ns-validator-priv-key")}},
			},
			wantErr: true,
		},
		{
			name: "explicit secret colliding with a group instance default is rejected",
			nodes: []NodeGroupSpec{
				{Name: "validators", Instances: ptr.To(2), Validator: &NodeSetValidatorConfig{}}, // ns-validators-0/1-priv-key
				{Name: "extra", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{PrivateKeySecret: ptr.To("ns-validators-0-priv-key")}},
			},
			wantErr: true,
		},
		{
			name: "distinct default secrets across groups are allowed",
			nodes: []NodeGroupSpec{
				{Name: "a", Instances: ptr.To(2), Validator: &NodeSetValidatorConfig{}},
				{Name: "b", Instances: ptr.To(2), Validator: &NodeSetValidatorConfig{}},
			},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeSet := &ChainNodeSet{
				ObjectMeta: metav1.ObjectMeta{Name: "ns"},
				Spec: ChainNodeSetSpec{
					Validator: tt.validator,
					Nodes:     tt.nodes,
				},
			}
			err := nodeSet.validateUniqueSigningKeys()
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "privateKeySecret")
				return
			}
			assert.NoError(t, err)
		})
	}
}

// TestChainNodeSetValidateRejectsRemovingGenesisInitGroupAfterCreation verifies that, once genesis
// has been created, an existing genesis-initializing validator group cannot be deleted or converted
// to a non-init or non-validator group. Its validators are part of the immutable genesis validator
// set, so dropping them (ensureValidator would delete the underlying ChainNodes) can halt the chain.
func TestChainNodeSetValidateRejectsRemovingGenesisInitGroupAfterCreation(t *testing.T) {
	initConfig := &GenesisInitConfig{ChainID: "test-localnet", Assets: []string{"1unibi"}, StakeAmount: "1unibi"}
	oldRunning := func() *ChainNodeSet {
		ns := &ChainNodeSet{
			Spec: ChainNodeSetSpec{
				Nodes: []NodeGroupSpec{{
					Name:      "validators",
					Instances: ptr.To(2),
					Validator: &NodeSetValidatorConfig{Init: initConfig},
				}},
			},
		}
		ns.Status.ChainID = "test-localnet"
		return ns
	}

	tests := []struct {
		name string
		spec ChainNodeSetSpec
	}{
		{
			name: "deleting the init group is rejected",
			spec: ChainNodeSetSpec{
				Nodes: []NodeGroupSpec{{Name: "fullnodes", Instances: ptr.To(1)}},
			},
		},
		{
			name: "converting the init group to a non-init validator group is rejected",
			spec: ChainNodeSetSpec{
				Nodes: []NodeGroupSpec{{
					Name:      "validators",
					Instances: ptr.To(2),
					Validator: &NodeSetValidatorConfig{CreateValidator: &CreateValidatorConfig{}},
				}},
			},
		},
		{
			name: "converting the init group to a non-validator group is rejected",
			spec: ChainNodeSetSpec{
				Nodes: []NodeGroupSpec{{Name: "validators", Instances: ptr.To(2)}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			newSet := &ChainNodeSet{Spec: tt.spec}
			_, err := newSet.Validate(oldRunning())
			require.Error(t, err)
			assert.Contains(t, err.Error(), "cannot be removed or converted after genesis has been created")
		})
	}
}

// TestChainNodeSetValidateMultiInstanceCreateValidatorRejectsSharedAccount verifies that a shared
// createValidator.accountMnemonicSecret is rejected for a multi-instance validator group: every
// instance would otherwise submit a create-validator tx for the same operator account. A
// single-instance group may set it.
func TestChainNodeSetValidateMultiInstanceCreateValidatorRejectsSharedAccount(t *testing.T) {
	nodeSet := &ChainNodeSet{
		Spec: ChainNodeSetSpec{
			Genesis: &GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
			Nodes: []NodeGroupSpec{{
				Name:      "validators",
				Instances: ptr.To(2),
				Validator: &NodeSetValidatorConfig{
					CreateValidator: &CreateValidatorConfig{AccountMnemonicSecret: ptr.To("shared-account")},
				},
			}},
		},
	}
	_, err := nodeSet.Validate(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "createValidator.accountMnemonicSecret cannot be set when the validator group has multiple instances")

	// A single-instance group may set createValidator.accountMnemonicSecret.
	nodeSet.Spec.Nodes[0].Instances = ptr.To(1)
	_, err = nodeSet.Validate(nil)
	assert.NoError(t, err)
}

// TestChainNodeSetValidateGenesisWaiverRequiresInitGenerated verifies that the .spec.genesis
// requirement is only waived after the chain is running when the genesis was produced by a
// genesis-initializing validator (which generates the <chainID>-genesis ConfigMap). When the chain
// used an explicit genesis source (configMap, useDataVolume, ...), no such ConfigMap is generated,
// so .spec.genesis must be retained for newly added non-init validators.
func TestChainNodeSetValidateGenesisWaiverRequiresInitGenerated(t *testing.T) {
	initConfig := &GenesisInitConfig{ChainID: "test-localnet", Assets: []string{"1unibi"}, StakeAmount: "1unibi"}
	joiner := NodeGroupSpec{Name: "joiners", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{CreateValidator: &CreateValidatorConfig{}}}

	running := func(genesis *GenesisConfig, nodes []NodeGroupSpec) *ChainNodeSet {
		return &ChainNodeSet{
			Spec:   ChainNodeSetSpec{Genesis: genesis, Nodes: nodes},
			Status: ChainNodeSetStatus{ChainID: "test-localnet"},
		}
	}

	t.Run("init-generated genesis allows dropping genesis for a non-init validator", func(t *testing.T) {
		old := running(nil, []NodeGroupSpec{{Name: "validators", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{Init: initConfig}}})
		updated := &ChainNodeSet{Spec: ChainNodeSetSpec{Nodes: []NodeGroupSpec{
			{Name: "validators", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{Init: initConfig}},
			joiner,
		}}}
		_, err := updated.Validate(old)
		assert.NoError(t, err)
	})

	t.Run("explicit configMap genesis requires genesis for a non-init validator", func(t *testing.T) {
		old := running(&GenesisConfig{ConfigMap: ptr.To("custom-genesis")}, []NodeGroupSpec{{Name: "fullnodes", Instances: ptr.To(1)}})
		updated := &ChainNodeSet{Spec: ChainNodeSetSpec{Nodes: []NodeGroupSpec{
			{Name: "fullnodes", Instances: ptr.To(1)},
			joiner,
		}}}
		_, err := updated.Validate(old)
		require.Error(t, err)
		assert.Contains(t, err.Error(), ".spec.genesis is required")
	})

	t.Run("useDataVolume genesis requires genesis for a non-init validator", func(t *testing.T) {
		old := running(&GenesisConfig{ChainID: ptr.To("test-localnet"), UseDataVolume: ptr.To(true)}, []NodeGroupSpec{{Name: "fullnodes", Instances: ptr.To(1)}})
		updated := &ChainNodeSet{Spec: ChainNodeSetSpec{Nodes: []NodeGroupSpec{
			{Name: "fullnodes", Instances: ptr.To(1)},
			joiner,
		}}}
		_, err := updated.Validate(old)
		require.Error(t, err)
		assert.Contains(t, err.Error(), ".spec.genesis is required")
	})

	t.Run("retaining the explicit configMap genesis is allowed", func(t *testing.T) {
		old := running(&GenesisConfig{ConfigMap: ptr.To("custom-genesis")}, []NodeGroupSpec{{Name: "fullnodes", Instances: ptr.To(1)}})
		updated := &ChainNodeSet{Spec: ChainNodeSetSpec{
			Genesis: &GenesisConfig{ConfigMap: ptr.To("custom-genesis")},
			Nodes: []NodeGroupSpec{
				{Name: "fullnodes", Instances: ptr.To(1)},
				joiner,
			},
		}}
		_, err := updated.Validate(old)
		assert.NoError(t, err)
	})
}

// TestChainNodeSetValidateRejectsRemovingLegacyInitValidatorAfterCreation verifies that, once
// genesis has been created, the legacy singleton .spec.validator.init cannot be removed — neither by
// dropping .spec.validator entirely nor by clearing its .init — mirroring the group removal guard.
func TestChainNodeSetValidateRejectsRemovingLegacyInitValidatorAfterCreation(t *testing.T) {
	initConfig := &GenesisInitConfig{ChainID: "test-localnet", Assets: []string{"1unibi"}, StakeAmount: "1unibi"}
	oldRunning := func() *ChainNodeSet {
		ns := &ChainNodeSet{
			Spec: ChainNodeSetSpec{
				Validator: &NodeSetValidatorConfig{Init: initConfig},
				Nodes:     []NodeGroupSpec{{Name: "fullnodes", Instances: ptr.To(1)}},
			},
		}
		ns.Status.ChainID = "test-localnet"
		return ns
	}

	t.Run("removing .spec.validator is rejected", func(t *testing.T) {
		newSet := &ChainNodeSet{Spec: ChainNodeSetSpec{Nodes: []NodeGroupSpec{{Name: "fullnodes", Instances: ptr.To(1)}}}}
		_, err := newSet.Validate(oldRunning())
		require.Error(t, err)
		assert.Contains(t, err.Error(), ".spec.validator.init cannot be removed after genesis has been created")
	})

	t.Run("clearing .spec.validator.init is rejected", func(t *testing.T) {
		newSet := &ChainNodeSet{Spec: ChainNodeSetSpec{
			Validator: &NodeSetValidatorConfig{CreateValidator: &CreateValidatorConfig{}},
			Nodes:     []NodeGroupSpec{{Name: "fullnodes", Instances: ptr.To(1)}},
		}}
		_, err := newSet.Validate(oldRunning())
		require.Error(t, err)
		assert.Contains(t, err.Error(), ".spec.validator.init cannot be removed after genesis has been created")
	})

	t.Run("keeping .spec.validator.init is allowed", func(t *testing.T) {
		newSet := &ChainNodeSet{Spec: ChainNodeSetSpec{
			Validator: &NodeSetValidatorConfig{Init: initConfig},
			Nodes:     []NodeGroupSpec{{Name: "fullnodes", Instances: ptr.To(1)}},
		}}
		_, err := newSet.Validate(oldRunning())
		assert.NoError(t, err)
	})
}

// TestChainNodeSetValidateRejectsValidatorPdbNameCollision verifies that a validator group whose
// validator PDB ("<nodeset>-<group>-validator") would collide with a regular group's PDB
// ("<nodeset>-<group>") is rejected — e.g. a validator group "foo" alongside a regular group
// "foo-validator" — but only when both groups actually create a PDB (PDBs default to disabled). Two
// validator groups with matching suffixes never collide: their PDBs are "<nodeset>-foo-validator" and
// "<nodeset>-foo-validator-validator".
func TestChainNodeSetValidateRejectsValidatorPdbNameCollision(t *testing.T) {
	mk := func(nodes []NodeGroupSpec) *ChainNodeSet {
		return &ChainNodeSet{Spec: ChainNodeSetSpec{
			Genesis: &GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
			Nodes:   nodes,
		}}
	}
	valPdbOn := func() *NodeSetValidatorConfig { return &NodeSetValidatorConfig{PDB: &PdbConfig{Enabled: true}} }
	pdbOn := func() *PdbConfig { return &PdbConfig{Enabled: true} }

	// Validator group "foo" + regular group "foo-validator", both with a PDB enabled: collision.
	_, err := mk([]NodeGroupSpec{
		{Name: "foo", Instances: ptr.To(1), Validator: valPdbOn()},
		{Name: "foo-validator", Instances: ptr.To(1), PDB: pdbOn()},
	}).Validate(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "collides with regular group")

	// Order-independent: the colliding regular group can come first.
	_, err = mk([]NodeGroupSpec{
		{Name: "foo-validator", Instances: ptr.To(1), PDB: pdbOn()},
		{Name: "foo", Instances: ptr.To(1), Validator: valPdbOn()},
	}).Validate(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "collides with regular group")

	// No collision when the validator PDB is disabled (the default): no "<nodeset>-foo-validator"
	// validator PDB is created, so the suffix pair is a valid topology.
	_, err = mk([]NodeGroupSpec{
		{Name: "foo", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{}},
		{Name: "foo-validator", Instances: ptr.To(1), PDB: pdbOn()},
	}).Validate(nil)
	assert.NoError(t, err)

	// Still rejected when the regular group's PDB is disabled: that regular group reconciles the same
	// "<nodeset>-foo-validator" name and its disabled-PDB cleanup would delete the validator group's PDB.
	_, err = mk([]NodeGroupSpec{
		{Name: "foo", Instances: ptr.To(1), Validator: valPdbOn()},
		{Name: "foo-validator", Instances: ptr.To(1)},
	}).Validate(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "collides with regular group")

	// Suffix-matched validator groups are allowed even with PDBs enabled: their validator PDB names
	// remain distinct.
	_, err = mk([]NodeGroupSpec{
		{Name: "foo", Instances: ptr.To(1), Validator: valPdbOn()},
		{Name: "foo-validator", Instances: ptr.To(1), Validator: valPdbOn()},
	}).Validate(nil)
	assert.NoError(t, err)

	// Non-colliding validator group names are allowed.
	_, err = mk([]NodeGroupSpec{
		{Name: "foo", Instances: ptr.To(1), Validator: valPdbOn()},
		{Name: "bar", Instances: ptr.To(1)},
	}).Validate(nil)
	assert.NoError(t, err)

	// Two regular groups "foo" and "foo-validator" do not collide: only validator groups produce the
	// "<group>-validator" PDB.
	_, err = mk([]NodeGroupSpec{
		{Name: "foo", Instances: ptr.To(1), PDB: pdbOn()},
		{Name: "foo-validator", Instances: ptr.To(1), PDB: pdbOn()},
	}).Validate(nil)
	assert.NoError(t, err)
}

// TestChainNodeSetValidateRejectsInitWithCreateValidator verifies that a validator configured with
// both .init and .createValidator is rejected — for the legacy singleton and for validator groups,
// including multi-instance init groups. An init validator is already part of the generated genesis
// validator set, so a create-validator tx for it would be redundant and fail on-chain.
func TestChainNodeSetValidateRejectsInitWithCreateValidator(t *testing.T) {
	initConfig := &GenesisInitConfig{ChainID: "test-localnet", Assets: []string{"1unibi"}, StakeAmount: "1unibi"}

	t.Run("legacy validator with init and createValidator is rejected", func(t *testing.T) {
		nodeSet := &ChainNodeSet{Spec: ChainNodeSetSpec{
			Validator: &NodeSetValidatorConfig{Init: initConfig, CreateValidator: &CreateValidatorConfig{}},
		}}
		_, err := nodeSet.Validate(nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), ".spec.validator.init and .spec.validator.createValidator are mutually exclusive")
	})

	t.Run("single-instance group with init and createValidator is rejected", func(t *testing.T) {
		nodeSet := &ChainNodeSet{Spec: ChainNodeSetSpec{Nodes: []NodeGroupSpec{{
			Name:      "validators",
			Instances: ptr.To(1),
			Validator: &NodeSetValidatorConfig{Init: initConfig, CreateValidator: &CreateValidatorConfig{}},
		}}}}
		_, err := nodeSet.Validate(nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), ".spec.nodes[0].validator.init and .spec.nodes[0].validator.createValidator are mutually exclusive")
	})

	t.Run("multi-instance group with init and createValidator is rejected", func(t *testing.T) {
		nodeSet := &ChainNodeSet{Spec: ChainNodeSetSpec{Nodes: []NodeGroupSpec{{
			Name:      "validators",
			Instances: ptr.To(3),
			Validator: &NodeSetValidatorConfig{Init: initConfig, CreateValidator: &CreateValidatorConfig{}},
		}}}}
		_, err := nodeSet.Validate(nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "mutually exclusive")
	})

	t.Run("init alone is allowed", func(t *testing.T) {
		nodeSet := &ChainNodeSet{Spec: ChainNodeSetSpec{Nodes: []NodeGroupSpec{{
			Name:      "validators",
			Instances: ptr.To(3),
			Validator: &NodeSetValidatorConfig{Init: initConfig},
		}}}}
		_, err := nodeSet.Validate(nil)
		assert.NoError(t, err)
	})
}

// TestChainNodeSetValidateValidatorPersistenceSize verifies that the validator-specific persistence
// size is validated with the same quantity parsing used for regular group persistence, for both the
// legacy singleton and validator groups.
func TestChainNodeSetValidateValidatorPersistenceSize(t *testing.T) {
	genesis := &GenesisConfig{Url: ptr.To("https://example.com/genesis.json")}

	t.Run("invalid group validator persistence size is rejected", func(t *testing.T) {
		nodeSet := &ChainNodeSet{Spec: ChainNodeSetSpec{
			Genesis: genesis,
			Nodes: []NodeGroupSpec{{
				Name:      "validators",
				Instances: ptr.To(1),
				Validator: &NodeSetValidatorConfig{Persistence: &Persistence{Size: ptr.To("not-a-size")}},
			}},
		}}
		_, err := nodeSet.Validate(nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "bad format for .spec.nodes[0].validator.persistence.size")
	})

	t.Run("valid group validator persistence size is allowed", func(t *testing.T) {
		nodeSet := &ChainNodeSet{Spec: ChainNodeSetSpec{
			Genesis: genesis,
			Nodes: []NodeGroupSpec{{
				Name:      "validators",
				Instances: ptr.To(1),
				Validator: &NodeSetValidatorConfig{Persistence: &Persistence{Size: ptr.To("50Gi")}},
			}},
		}}
		_, err := nodeSet.Validate(nil)
		assert.NoError(t, err)
	})

	t.Run("invalid legacy validator persistence size is rejected", func(t *testing.T) {
		nodeSet := &ChainNodeSet{Spec: ChainNodeSetSpec{
			Genesis:   genesis,
			Validator: &NodeSetValidatorConfig{Persistence: &Persistence{Size: ptr.To("bad")}},
		}}
		_, err := nodeSet.Validate(nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "bad format for .spec.validator.persistence.size")
	})
}

// TestChainNodeSetValidateRejectsDuplicateCreateValidatorAccounts verifies that two running
// create-validator validators may not resolve to the same account-mnemonic secret (which would derive
// the same operator account). Defaults match the generated ChainNodes: <nodeset>-validator-account for
// the legacy singleton and <nodeset>-<group>-<index>-account for group validators.
func TestChainNodeSetValidateRejectsDuplicateCreateValidatorAccounts(t *testing.T) {
	tests := []struct {
		name      string
		validator *NodeSetValidatorConfig
		nodes     []NodeGroupSpec
		wantErr   bool
	}{
		{
			name: "two single-instance groups sharing an explicit account are rejected",
			nodes: []NodeGroupSpec{
				{Name: "a", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{CreateValidator: &CreateValidatorConfig{AccountMnemonicSecret: ptr.To("shared-account")}}},
				{Name: "b", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{CreateValidator: &CreateValidatorConfig{AccountMnemonicSecret: ptr.To("shared-account")}}},
			},
			wantErr: true,
		},
		{
			name: "explicit account colliding with another validator's default is rejected",
			nodes: []NodeGroupSpec{
				{Name: "a", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{CreateValidator: &CreateValidatorConfig{}}},
				{Name: "b", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{CreateValidator: &CreateValidatorConfig{AccountMnemonicSecret: ptr.To("ns-a-0-account")}}},
			},
			wantErr: true,
		},
		{
			name:      "legacy default colliding with a group's explicit account is rejected",
			validator: &NodeSetValidatorConfig{CreateValidator: &CreateValidatorConfig{}}, // resolves to ns-validator-account
			nodes: []NodeGroupSpec{
				{Name: "extra", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{CreateValidator: &CreateValidatorConfig{AccountMnemonicSecret: ptr.To("ns-validator-account")}}},
			},
			wantErr: true,
		},
		{
			name: "distinct default accounts across groups are allowed",
			nodes: []NodeGroupSpec{
				{Name: "a", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{CreateValidator: &CreateValidatorConfig{}}},
				{Name: "b", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{CreateValidator: &CreateValidatorConfig{}}},
			},
			wantErr: false,
		},
		{
			name: "multi-instance createValidator group gets distinct per-instance defaults",
			nodes: []NodeGroupSpec{
				{Name: "a", Instances: ptr.To(2), Validator: &NodeSetValidatorConfig{CreateValidator: &CreateValidatorConfig{}}},
			},
			wantErr: false,
		},
		{
			name: "init validators seed the genesis accounts; createValidator reuses that account",
			nodes: []NodeGroupSpec{
				{Name: "a", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{CreateValidator: &CreateValidatorConfig{AccountMnemonicSecret: ptr.To("ns-b-0-account")}}},
				{Name: "b", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{Init: &GenesisInitConfig{ChainID: "c", Assets: []string{"1u"}, StakeAmount: "1u"}}},
			},
			wantErr: true, // ns-b-0-account is the genesis default for group b instance 0
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeSet := &ChainNodeSet{
				ObjectMeta: metav1.ObjectMeta{Name: "ns"},
				Spec:       ChainNodeSetSpec{Validator: tt.validator, Nodes: tt.nodes},
			}
			err := nodeSet.validateUniqueCreateValidatorAccounts()
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), "each create-validator validator must use a distinct account")
				return
			}
			assert.NoError(t, err)
		})
	}

	// The check is wired into the full Validate path.
	t.Run("duplicate create-validator accounts are rejected through Validate", func(t *testing.T) {
		nodeSet := &ChainNodeSet{
			ObjectMeta: metav1.ObjectMeta{Name: "ns"},
			Spec: ChainNodeSetSpec{
				Genesis: &GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
				Nodes: []NodeGroupSpec{
					{Name: "a", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{CreateValidator: &CreateValidatorConfig{AccountMnemonicSecret: ptr.To("shared-account")}}},
					{Name: "b", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{CreateValidator: &CreateValidatorConfig{AccountMnemonicSecret: ptr.To("shared-account")}}},
				},
			},
		}
		_, err := nodeSet.Validate(nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "each create-validator validator must use a distinct account")
	})
}

// TestChainNodeSetValidateRejectsGenesisValidatorPrivKeyCollision verifies that a user-preserved
// validator.init.genesisValidators[].privKeySecret is included in signing-key uniqueness validation,
// so it cannot collide with the init validator's own default key or a generated non-init group default.
func TestChainNodeSetValidateRejectsGenesisValidatorPrivKeyCollision(t *testing.T) {
	gv := func(privKey string) GenesisValidator {
		return GenesisValidator{
			PrivKeySecret:         privKey,
			AccountMnemonicSecret: "ext-account",
			Moniker:               "ext",
			Assets:                []string{"1unibi"},
			StakeAmount:           "1unibi",
		}
	}
	initWith := func(gvs ...GenesisValidator) *GenesisInitConfig {
		return &GenesisInitConfig{ChainID: "test-localnet", Assets: []string{"1unibi"}, StakeAmount: "1unibi", GenesisValidators: gvs}
	}

	t.Run("collision with the legacy init validator default is rejected", func(t *testing.T) {
		// The legacy singleton's default priv key is ns-validator-priv-key.
		nodeSet := &ChainNodeSet{
			ObjectMeta: metav1.ObjectMeta{Name: "ns"},
			Spec:       ChainNodeSetSpec{Validator: &NodeSetValidatorConfig{Init: initWith(gv("ns-validator-priv-key"))}},
		}
		err := nodeSet.validateUniqueSigningKeys()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "ns-validator-priv-key")
	})

	t.Run("collision with a generated non-init group default is rejected", func(t *testing.T) {
		// Instance 1 of a 3-instance init group has the generated default ns-validators-1-priv-key.
		nodeSet := &ChainNodeSet{
			ObjectMeta: metav1.ObjectMeta{Name: "ns"},
			Spec: ChainNodeSetSpec{Nodes: []NodeGroupSpec{{
				Name:      "validators",
				Instances: ptr.To(3),
				Validator: &NodeSetValidatorConfig{Init: initWith(gv("ns-validators-1-priv-key"))},
			}}},
		}
		err := nodeSet.validateUniqueSigningKeys()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "ns-validators-1-priv-key")
	})

	t.Run("a distinct preserved genesis validator priv key is allowed", func(t *testing.T) {
		nodeSet := &ChainNodeSet{
			ObjectMeta: metav1.ObjectMeta{Name: "ns"},
			Spec: ChainNodeSetSpec{Nodes: []NodeGroupSpec{{
				Name:      "validators",
				Instances: ptr.To(3),
				Validator: &NodeSetValidatorConfig{Init: initWith(gv("external-priv-key"))},
			}}},
		}
		err := nodeSet.validateUniqueSigningKeys()
		assert.NoError(t, err)
	})
}

// TestChainNodeSetValidateRejectsCreateValidatorCollidingWithGenesisAccount verifies that a
// create-validator validator whose resolved account-mnemonic secret matches a genesis validator
// account (init validator default, preserved genesis validator, or generated group instance default)
// is rejected: submitting create-validator from a genesis operator account would try to create a
// validator that already exists in the immutable genesis validator set.
func TestChainNodeSetValidateRejectsCreateValidatorCollidingWithGenesisAccount(t *testing.T) {
	initConfig := &GenesisInitConfig{ChainID: "test-localnet", Assets: []string{"1unibi"}, StakeAmount: "1unibi"}

	t.Run("createValidator colliding with legacy genesis validator default is rejected", func(t *testing.T) {
		// Legacy singleton default account is ns-validator-account.
		nodeSet := &ChainNodeSet{
			ObjectMeta: metav1.ObjectMeta{Name: "ns"},
			Spec: ChainNodeSetSpec{
				Validator: &NodeSetValidatorConfig{Init: initConfig},
				Nodes: []NodeGroupSpec{{
					Name:      "joiners",
					Instances: ptr.To(1),
					Validator: &NodeSetValidatorConfig{CreateValidator: &CreateValidatorConfig{AccountMnemonicSecret: ptr.To("ns-validator-account")}},
				}},
			},
		}
		err := nodeSet.validateUniqueCreateValidatorAccounts()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "ns-validator-account")
	})

	t.Run("createValidator colliding with a generated group genesis validator account is rejected", func(t *testing.T) {
		// A 3-instance init group generates accounts ns-validators-1-account and ns-validators-2-account.
		nodeSet := &ChainNodeSet{
			ObjectMeta: metav1.ObjectMeta{Name: "ns"},
			Spec: ChainNodeSetSpec{
				Nodes: []NodeGroupSpec{
					{Name: "validators", Instances: ptr.To(3), Validator: &NodeSetValidatorConfig{Init: initConfig}},
					{Name: "joiners", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{CreateValidator: &CreateValidatorConfig{AccountMnemonicSecret: ptr.To("ns-validators-1-account")}}},
				},
			},
		}
		err := nodeSet.validateUniqueCreateValidatorAccounts()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "ns-validators-1-account")
	})

	t.Run("distinct create-validator account from genesis accounts is allowed", func(t *testing.T) {
		nodeSet := &ChainNodeSet{
			ObjectMeta: metav1.ObjectMeta{Name: "ns"},
			Spec: ChainNodeSetSpec{
				Validator: &NodeSetValidatorConfig{Init: initConfig},
				Nodes: []NodeGroupSpec{{
					Name:      "joiners",
					Instances: ptr.To(1),
					Validator: &NodeSetValidatorConfig{CreateValidator: &CreateValidatorConfig{AccountMnemonicSecret: ptr.To("funded-joiner-account")}},
				}},
			},
		}
		err := nodeSet.validateUniqueCreateValidatorAccounts()
		assert.NoError(t, err)
	})
}

// TestGenesisSigningMaterialChangedIncludesChainID verifies that genesisSigningMaterialChanged
// also detects a change in init.chainID, which is immutable after genesis (it determines which
// <chainID>-genesis ConfigMap the generated ChainNode consumes).
// TestChainNodeSetValidateRejectsGenesisValidatorsChangeAfterCreation verifies that changing the
// preserved genesis validator list (init.genesisValidators) after genesis is rejected: those entries
// are baked into the immutable initial validator set, so a recreated init ChainNode would otherwise
// regenerate a different genesis.
func TestChainNodeSetValidateRejectsGenesisValidatorsChangeAfterCreation(t *testing.T) {
	gv := func(priv string) GenesisValidator {
		return GenesisValidator{
			PrivKeySecret:         priv,
			AccountMnemonicSecret: "extra-account",
			Moniker:               "extra",
			Assets:                []string{"1unibi"},
			StakeAmount:           "1unibi",
		}
	}
	mk := func(gvs []GenesisValidator) *ChainNodeSet {
		return &ChainNodeSet{Spec: ChainNodeSetSpec{Nodes: []NodeGroupSpec{{
			Name:      "validators",
			Instances: ptr.To(1),
			Validator: &NodeSetValidatorConfig{Init: &GenesisInitConfig{
				ChainID:           "test-localnet",
				Assets:            []string{"1unibi"},
				StakeAmount:       "1unibi",
				GenesisValidators: gvs,
			}},
		}}}}
	}
	created := func(ns *ChainNodeSet) *ChainNodeSet {
		ns.Status.ChainID = "test-localnet"
		return ns
	}

	// Adding a preserved genesis validator after genesis is rejected.
	_, err := mk([]GenesisValidator{gv("extra-priv-key")}).Validate(created(mk(nil)))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "genesis parameters cannot be changed")

	// Removing one after genesis is rejected too.
	_, err = mk(nil).Validate(created(mk([]GenesisValidator{gv("extra-priv-key")})))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "genesis parameters cannot be changed")

	// An unchanged genesis validator list is accepted.
	_, err = mk([]GenesisValidator{gv("extra-priv-key")}).Validate(created(mk([]GenesisValidator{gv("extra-priv-key")})))
	assert.NoError(t, err)
}

// TestChainNodeSetValidateRejectsInitParamChangeAfterCreation verifies that changing any
// genesis-affecting init parameter (not just signing material) after genesis is rejected — the whole
// .validator.init block is baked into the immutable genesis.
func TestChainNodeSetValidateRejectsInitParamChangeAfterCreation(t *testing.T) {
	base := func() *GenesisInitConfig {
		return &GenesisInitConfig{ChainID: "test-localnet", Assets: []string{"1unibi"}, StakeAmount: "1unibi"}
	}
	mk := func(init *GenesisInitConfig) *ChainNodeSet {
		return &ChainNodeSet{Spec: ChainNodeSetSpec{Nodes: []NodeGroupSpec{{
			Name:      "validators",
			Instances: ptr.To(1),
			Validator: &NodeSetValidatorConfig{Init: init},
		}}}}
	}
	created := func(ns *ChainNodeSet) *ChainNodeSet {
		ns.Status.ChainID = "test-localnet"
		return ns
	}

	// Changing the stake amount after genesis is rejected.
	changedStake := base()
	changedStake.StakeAmount = "2unibi"
	_, err := mk(changedStake).Validate(created(mk(base())))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "genesis parameters cannot be changed")

	// Changing the funded assets after genesis is rejected.
	changedAssets := base()
	changedAssets.Assets = []string{"2unibi"}
	_, err = mk(changedAssets).Validate(created(mk(base())))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "genesis parameters cannot be changed")

	// An unchanged init config is accepted.
	_, err = mk(base()).Validate(created(mk(base())))
	assert.NoError(t, err)
}

func TestValidateCosmosignerUpdateRejectsAddingSignerToEstablishedMultiInstanceGroup(t *testing.T) {
	old := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ns"},
		Spec: ChainNodeSetSpec{Nodes: []NodeGroupSpec{{
			Name:      "validators",
			Instances: ptr.To(2),
			Validator: &NodeSetValidatorConfig{CreateValidator: &CreateValidatorConfig{}},
		}}},
		Status: ChainNodeSetStatus{ChainID: "chain-1"},
	}
	updated := old.DeepCopy()
	updated.Spec.Nodes[0].Cosmosigner = &Cosmosigner{
		Backend: CosmosignerBackend{Software: &CosmosignerSoftwareBackend{}},
	}

	err := updated.validateCosmosignerUpdate(old)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "multi-instance validator group")
}

// TestChainNodeSetValidateRejectsAccountSettingsChangeAfterCreation verifies that changing the
// validator-level account derivation settings (accountPrefix/valPrefix/accountHDPath) after genesis is
// rejected — they live on the validator config (outside .init) yet determine the operator/account
// addresses initGenesis bakes into the immutable genesis.
func TestChainNodeSetValidateRejectsAccountSettingsChangeAfterCreation(t *testing.T) {
	mk := func(prefix string) *ChainNodeSet {
		v := &NodeSetValidatorConfig{Init: &GenesisInitConfig{ChainID: "test-localnet", Assets: []string{"1unibi"}, StakeAmount: "1unibi"}}
		if prefix != "" {
			v.AccountPrefix = ptr.To(prefix)
		}
		return &ChainNodeSet{Spec: ChainNodeSetSpec{Nodes: []NodeGroupSpec{{
			Name:      "validators",
			Instances: ptr.To(1),
			Validator: v,
		}}}}
	}
	created := func(ns *ChainNodeSet) *ChainNodeSet {
		ns.Status.ChainID = "test-localnet"
		return ns
	}

	// Changing the account prefix after genesis is rejected.
	_, err := mk("osmo").Validate(created(mk("")))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "genesis parameters cannot be changed")

	// Unchanged account settings are accepted.
	_, err = mk("osmo").Validate(created(mk("osmo")))
	assert.NoError(t, err)
}

// TestChainNodeSetValidateRejectsValidatorInfoChangeAfterCreation verifies that changing
// .validator.info after genesis is rejected — initGenesis bakes it into the init validator's gentx.
func TestChainNodeSetValidateRejectsValidatorInfoChangeAfterCreation(t *testing.T) {
	mk := func(details string) *ChainNodeSet {
		v := &NodeSetValidatorConfig{Init: &GenesisInitConfig{ChainID: "test-localnet", Assets: []string{"1unibi"}, StakeAmount: "1unibi"}}
		if details != "" {
			v.Info = &ValidatorInfo{Details: ptr.To(details)}
		}
		return &ChainNodeSet{Spec: ChainNodeSetSpec{Nodes: []NodeGroupSpec{{
			Name:      "validators",
			Instances: ptr.To(1),
			Validator: v,
		}}}}
	}
	created := func(ns *ChainNodeSet) *ChainNodeSet {
		ns.Status.ChainID = "test-localnet"
		return ns
	}

	// Changing the validator info details after genesis is rejected.
	_, err := mk("new details").Validate(created(mk("old details")))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "genesis parameters cannot be changed")

	// Unchanged info is accepted.
	_, err = mk("same").Validate(created(mk("same")))
	assert.NoError(t, err)
}

// TestShouldUseCosmoGuardPortsLegacyValidator verifies that a global route listing the reserved
// "validator" group reflects CosmoGuard enabled on the legacy .spec.validator (which is not in
// .spec.nodes), so the global Service targets the CosmoGuard ports instead of the raw app ports.
func TestShouldUseCosmoGuardPortsLegacyValidator(t *testing.T) {
	guarded := &Config{CosmoGuard: &CosmoGuardConfig{Enable: true}}

	withGuard := &ChainNodeSet{Spec: ChainNodeSetSpec{
		Validator: &NodeSetValidatorConfig{Config: guarded},
	}}
	withoutGuard := &ChainNodeSet{Spec: ChainNodeSetSpec{
		Validator: &NodeSetValidatorConfig{},
	}}

	ing := &GlobalIngressConfig{Groups: []string{ReservedValidatorGroupName}}
	assert.True(t, ing.ShouldUseCosmoGuardPorts(withGuard), "ingress should detect CosmoGuard on the legacy validator")
	assert.False(t, ing.ShouldUseCosmoGuardPorts(withoutGuard))

	gw := &GlobalGatewayConfig{Groups: []string{ReservedValidatorGroupName}}
	assert.True(t, gw.ShouldUseCosmoGuardPorts(withGuard), "gateway should detect CosmoGuard on the legacy validator")
	assert.False(t, gw.ShouldUseCosmoGuardPorts(withoutGuard))
}

func TestGenesisSigningMaterialChangedIncludesChainID(t *testing.T) {
	base := &NodeSetValidatorConfig{
		Init: &GenesisInitConfig{ChainID: "chain-a", Assets: []string{"1u"}, StakeAmount: "1u"},
	}
	sameChainID := &NodeSetValidatorConfig{
		Init: &GenesisInitConfig{ChainID: "chain-a", Assets: []string{"1u"}, StakeAmount: "1u"},
	}
	diffChainID := &NodeSetValidatorConfig{
		Init: &GenesisInitConfig{ChainID: "chain-b", Assets: []string{"1u"}, StakeAmount: "1u"},
	}
	const defaultKey = "ns-validator-priv-key"

	assert.False(t, genesisSigningMaterialChanged(base, sameChainID, defaultKey), "same chain ID should not be flagged as changed")
	assert.True(t, genesisSigningMaterialChanged(base, diffChainID, defaultKey), "changed chain ID should be detected")
}
