package v1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func TestChainNodeValidateWarnsWhenTmKMSIsConfigured(t *testing.T) {
	chainNode := &ChainNode{Spec: ChainNodeSpec{
		Genesis: &GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
		Validator: &ValidatorConfig{TmKMS: &TmKMS{Provider: TmKmsProvider{Hashicorp: &TmKmsHashicorpProvider{
			Address: "https://vault:8200",
			Key:     "validator-key",
		}}}},
	}}

	warnings, err := chainNode.Validate(nil)
	require.NoError(t, err)
	require.Equal(t, []string{
		".spec.validator.tmKMS is deprecated and will be removed in a future version; migrate to .spec.cosmosigner",
	}, []string(warnings))
}

func TestChainNodeValidateWarnsWhenDeprecatedVaultTokenRenewerIsConfigured(t *testing.T) {
	chainNode := &ChainNode{Spec: ChainNodeSpec{
		Genesis: &GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
		Validator: &ValidatorConfig{TmKMS: &TmKMS{Provider: TmKmsProvider{Hashicorp: &TmKmsHashicorpProvider{
			Address:        "https://vault:8200",
			Key:            "validator-key",
			AutoRenewToken: true,
		}}}},
	}}

	warnings, err := chainNode.Validate(nil)
	require.NoError(t, err)
	require.Equal(t, []string{
		".spec.validator.tmKMS is deprecated and will be removed in a future version; migrate to .spec.cosmosigner",
		".spec.validator.tmKMS.provider.hashicorp.autoRenewToken uses the deprecated vault-token-renewer sidecar; migrate to .spec.cosmosigner, which renews Vault tokens internally",
	}, []string(warnings))
}

// TestChainNodeValidateGenesisValidators verifies that a standalone ChainNode rejects duplicate
// signing keys or account mnemonics among .spec.validator.init.genesisValidators — both between two
// entries and against the init validator's own resolved priv-key/account secret — which would
// otherwise be accepted by the webhook and only fail later at genesis creation.
func TestChainNodeValidateGenesisValidators(t *testing.T) {
	gv := func(privKey, account string) GenesisValidator {
		return GenesisValidator{
			PrivKeySecret:         privKey,
			AccountMnemonicSecret: account,
			Moniker:               "extra",
			Assets:                []string{"1unibi"},
			StakeAmount:           "1unibi",
		}
	}
	chainNode := func(gvs ...GenesisValidator) *ChainNode {
		return &ChainNode{
			ObjectMeta: metav1.ObjectMeta{Name: "cn"},
			Spec: ChainNodeSpec{
				Validator: &ValidatorConfig{
					Init: &GenesisInitConfig{
						ChainID:           "test-localnet",
						Assets:            []string{"1unibi"},
						StakeAmount:       "1unibi",
						GenesisValidators: gvs,
					},
				},
			},
		}
	}

	t.Run("duplicate privKeySecret between two genesis validators is rejected", func(t *testing.T) {
		_, err := chainNode(gv("dup-priv-key", "account-a"), gv("dup-priv-key", "account-b")).Validate(nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "dup-priv-key")
	})

	t.Run("duplicate accountMnemonicSecret between two genesis validators is rejected", func(t *testing.T) {
		_, err := chainNode(gv("priv-key-a", "dup-account"), gv("priv-key-b", "dup-account")).Validate(nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "dup-account")
	})

	t.Run("collision with the init validator default priv-key is rejected", func(t *testing.T) {
		// The init validator's default priv-key secret is cn-priv-key.
		_, err := chainNode(gv("cn-priv-key", "account-a")).Validate(nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cn-priv-key")
	})

	t.Run("collision with the init validator default account is rejected", func(t *testing.T) {
		// The init validator's default account secret is cn-account.
		_, err := chainNode(gv("priv-key-a", "cn-account")).Validate(nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cn-account")
	})

	t.Run("distinct genesis validators are allowed", func(t *testing.T) {
		_, err := chainNode(gv("priv-key-a", "account-a"), gv("priv-key-b", "account-b")).Validate(nil)
		assert.NoError(t, err)
	})

	t.Run("explicit privateKeySecret on the init validator is honored when seeding", func(t *testing.T) {
		// With an explicit privateKeySecret, the init validator no longer reserves cn-priv-key, so a
		// genesis validator may use it; but it still cannot collide with the explicit name.
		cn := chainNode(gv("cn-priv-key", "account-a"))
		cn.Spec.Validator.PrivateKeySecret = ptr.To("explicit-priv-key")
		_, err := cn.Validate(nil)
		assert.NoError(t, err)

		cn = chainNode(gv("explicit-priv-key", "account-a"))
		cn.Spec.Validator.PrivateKeySecret = ptr.To("explicit-priv-key")
		_, err = cn.Validate(nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "explicit-priv-key")
	})
}

// TestChainNodeValidateGcsExportCredentials verifies that a ChainNode's GCS tarball export config must
// set exactly one of credentialsSecret or serviceAccountName (Workload Identity): both set or neither
// set is rejected, while either one alone is accepted.
func TestChainNodeValidateGcsExportCredentials(t *testing.T) {
	credsSecret := &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "gcs-credentials"},
		Key:                  "credentials.json",
	}
	chainNode := func(gcs *GcsExportConfig) *ChainNode {
		return &ChainNode{
			ObjectMeta: metav1.ObjectMeta{Name: "cn"},
			Spec: ChainNodeSpec{
				Genesis: &GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
				Persistence: &Persistence{
					Snapshots: &VolumeSnapshotsConfig{
						Frequency:     "24h",
						ExportTarball: &ExportTarballConfig{GCS: gcs},
					},
				},
			},
		}
	}

	t.Run("credentialsSecret only is allowed", func(t *testing.T) {
		_, err := chainNode(&GcsExportConfig{Bucket: "b", CredentialsSecret: credsSecret}).Validate(nil)
		assert.NoError(t, err)
	})

	t.Run("serviceAccountName only is allowed", func(t *testing.T) {
		_, err := chainNode(&GcsExportConfig{Bucket: "b", ServiceAccountName: ptr.To("gcs-uploader")}).Validate(nil)
		assert.NoError(t, err)
	})

	t.Run("both set is rejected", func(t *testing.T) {
		_, err := chainNode(&GcsExportConfig{Bucket: "b", CredentialsSecret: credsSecret, ServiceAccountName: ptr.To("gcs-uploader")}).Validate(nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "mutually exclusive")
	})

	t.Run("neither set is rejected", func(t *testing.T) {
		_, err := chainNode(&GcsExportConfig{Bucket: "b"}).Validate(nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "must be set")
	})

	t.Run("empty serviceAccountName is rejected", func(t *testing.T) {
		_, err := chainNode(&GcsExportConfig{Bucket: "b", ServiceAccountName: ptr.To("")}).Validate(nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "serviceAccountName must not be empty")
	})
}

// TestChainNodeValidateRejectsInitChangeAfterCreation verifies that a standalone ChainNode rejects
// changes to .spec.validator.init after genesis (old.status.chainID set): the entire init block is
// baked into the immutable genesis, so altering it would rebuild a different genesis if the ChainNode
// and its <chainID>-genesis ConfigMap were recreated. Before genesis the same change is allowed.
func TestChainNodeValidateRejectsInitChangeAfterCreation(t *testing.T) {
	mk := func(init *GenesisInitConfig) *ChainNode {
		return &ChainNode{
			ObjectMeta: metav1.ObjectMeta{Name: "cn"},
			Spec:       ChainNodeSpec{Validator: &ValidatorConfig{Init: init}},
		}
	}
	base := func() *GenesisInitConfig {
		return &GenesisInitConfig{ChainID: "test-localnet", Assets: []string{"1unibi"}, StakeAmount: "1unibi"}
	}
	created := func(cn *ChainNode) *ChainNode {
		cn.Status.ChainID = "test-localnet"
		return cn
	}

	// After genesis: adding a genesis validator entry is rejected.
	withGV := base()
	withGV.GenesisValidators = []GenesisValidator{{
		PrivKeySecret: "extra-priv-key", AccountMnemonicSecret: "extra-account", Moniker: "extra",
		Assets: []string{"1unibi"}, StakeAmount: "1unibi",
	}}
	_, err := mk(withGV).Validate(created(mk(base())))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "immutable after genesis")

	// After genesis: changing the stake amount is rejected.
	changedStake := base()
	changedStake.StakeAmount = "2unibi"
	_, err = mk(changedStake).Validate(created(mk(base())))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "immutable after genesis")

	// After genesis: an unchanged init config is accepted.
	_, err = mk(base()).Validate(created(mk(base())))
	assert.NoError(t, err)

	// After genesis: removing init (dropping it for an external genesis) is rejected — the validator
	// stays in the immutable genesis set but the node would stop using its key.
	consumer := &ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "cn"},
		Spec:       ChainNodeSpec{Genesis: &GenesisConfig{Url: ptr.To("https://example.com/genesis.json")}},
	}
	_, err = consumer.Validate(created(mk(base())))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot be removed after genesis")

	// After genesis: adding init to a running non-init node is rejected.
	_, err = mk(base()).Validate(created(consumer.DeepCopy()))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot be added after genesis")

	// Before genesis (old has no chainID): the same init change is allowed.
	_, err = mk(changedStake).Validate(mk(base()))
	assert.NoError(t, err)
}

// TestChainNodeValidateRejectsInitChangeNoWebhook verifies the no-webhook reconcile path (Validate with
// old == nil): a post-genesis .validator.init change is rejected by diffing the current spec against the
// genesis fingerprint recorded in the object's own status. Without a recorded digest (legacy/upgrade)
// the check defers, letting the controller backfill it.
func TestChainNodeValidateRejectsInitChangeNoWebhook(t *testing.T) {
	mk := func(init *GenesisInitConfig) *ChainNode {
		return &ChainNode{
			ObjectMeta: metav1.ObjectMeta{Name: "cn"},
			Spec:       ChainNodeSpec{Validator: &ValidatorConfig{Init: init}},
		}
	}
	base := func() *GenesisInitConfig {
		return &GenesisInitConfig{ChainID: "test-localnet", Assets: []string{"1unibi"}, StakeAmount: "1unibi"}
	}
	// digest is the fingerprint the controller records at genesis creation, from the original config.
	digest := mk(base()).Spec.Validator.GenesisSigningFingerprint("cn-priv-key")

	// Unchanged init matches the recorded digest: accepted.
	cn := mk(base())
	cn.Status.ChainID = "test-localnet"
	cn.Status.GenesisSigningDigest = digest
	_, err := cn.Validate(nil)
	require.NoError(t, err)

	// Changed init (different stake) against the recorded digest: rejected.
	changed := mk(base())
	changed.Spec.Validator.Init.StakeAmount = "2unibi"
	changed.Status.ChainID = "test-localnet"
	changed.Status.GenesisSigningDigest = digest
	_, err = changed.Validate(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot be changed or removed after genesis")

	// Removing init entirely (validator dropped for an external genesis) against the recorded digest:
	// rejected too.
	removed := &ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "cn"},
		Spec:       ChainNodeSpec{Genesis: &GenesisConfig{Url: ptr.To("https://example.com/genesis.json")}},
		Status:     ChainNodeStatus{ChainID: "test-localnet", GenesisSigningDigest: digest},
	}
	_, err = removed.Validate(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot be changed or removed after genesis")

	// No recorded digest (legacy/upgrade): the no-webhook check defers rather than reject.
	noDigest := mk(base())
	noDigest.Spec.Validator.Init.StakeAmount = "2unibi"
	noDigest.Status.ChainID = "test-localnet"
	_, err = noDigest.Validate(nil)
	require.NoError(t, err)
}

// TestChainNodeValidateRejectsAddingInitToExternalGenesisNoWebhook verifies the no-webhook path: a node
// that established its genesis from an external source (digest == GenesisDigestExternal sentinel) cannot
// gain .spec.validator.init after genesis, while staying init-less — including changing its signing
// material — remains allowed.
func TestChainNodeValidateRejectsAddingInitToExternalGenesisNoWebhook(t *testing.T) {
	// External consumer that gains init (and drops genesis): rejected.
	withInit := &ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "cn"},
		Spec:       ChainNodeSpec{Validator: &ValidatorConfig{Init: &GenesisInitConfig{ChainID: "test-localnet", Assets: []string{"1unibi"}, StakeAmount: "1unibi"}}},
		Status:     ChainNodeStatus{ChainID: "test-localnet", GenesisSigningDigest: GenesisDigestExternal},
	}
	_, err := withInit.Validate(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot be added after genesis")

	// External consumer staying init-less (a createValidator validator) may still change signing material.
	consumer := &ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "cn"},
		Spec: ChainNodeSpec{
			Genesis:   &GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
			Validator: &ValidatorConfig{CreateValidator: &CreateValidatorConfig{}, PrivateKeySecret: ptr.To("rotated-key")},
		},
		Status: ChainNodeStatus{ChainID: "test-localnet", GenesisSigningDigest: GenesisDigestExternal},
	}
	_, err = consumer.Validate(nil)
	require.NoError(t, err)
}
