package chainnodeset

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
)

// TestValidateForReconcileHonorsExistingStatus verifies the controller's no-webhook validation
// path validates an already-persisted ChainNodeSet against its own current status, so Validate can
// observe Status.ChainID (genesis already exists). A running ChainNodeSet that adds a
// createValidator group with no .spec.genesis is accepted, even though the same spec is rejected as
// a fresh create.
func TestValidateForReconcileHonorsExistingStatus(t *testing.T) {
	initConfig := &appsv1.GenesisInitConfig{ChainID: "test-localnet", Assets: []string{"1unibi"}, StakeAmount: "1unibi"}
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default"},
		Spec: appsv1.ChainNodeSetSpec{
			Nodes: []appsv1.NodeGroupSpec{
				{Name: "validators", Instances: ptr.To(1), Validator: &appsv1.NodeSetValidatorConfig{Init: initConfig}},
				{Name: "joiners", Instances: ptr.To(1), Validator: &appsv1.NodeSetValidatorConfig{CreateValidator: &appsv1.CreateValidatorConfig{}}},
			},
		},
		Status: appsv1.ChainNodeSetStatus{
			ChainID: "test-localnet",
			Validators: []appsv1.ChainNodeSetValidatorStatus{
				{Name: "test-nodeset-validators-0", Group: "validators", Init: true},
			},
		},
	}

	// With the existing status visible, the running configuration is valid.
	_, err := validateForReconcile(nodeSet)
	require.NoError(t, err)

	// The recorded status is what makes it valid: the same spec validated as a fresh create is
	// rejected because there is no existing genesis to consume.
	fresh := nodeSet.DeepCopy()
	fresh.Status = appsv1.ChainNodeSetStatus{}
	_, err = fresh.Validate(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), ".spec.genesis is required")
}

// TestValidateForReconcileRejectsUnsafeDroppedGenesis verifies the no-webhook path enforces the
// status-gated genesis invariant without an old spec. A running chain (chainID set) whose current
// spec has a non-init validator group but no .spec.genesis and no genesis-initializing validator has
// no derivable <chainID>-genesis to consume, so it is rejected — rather than validated against a
// copy of itself, which previously masked the missing genesis.
func TestValidateForReconcileRejectsUnsafeDroppedGenesis(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default"},
		Spec: appsv1.ChainNodeSetSpec{
			Nodes: []appsv1.NodeGroupSpec{
				{Name: "joiners", Instances: ptr.To(1), Validator: &appsv1.NodeSetValidatorConfig{CreateValidator: &appsv1.CreateValidatorConfig{}}},
			},
		},
		Status: appsv1.ChainNodeSetStatus{ChainID: "test-localnet"},
	}
	_, err := validateForReconcile(nodeSet)
	require.Error(t, err)
	assert.Contains(t, err.Error(), ".spec.genesis is required")
}

func TestValidateForReconcileRejectsUnsafeGenesisInitMutation(t *testing.T) {
	initConfig := &appsv1.GenesisInitConfig{ChainID: "test-localnet", Assets: []string{"1unibi"}, StakeAmount: "1unibi"}
	base := func(instances int, validators []appsv1.ChainNodeSetValidatorStatus) *appsv1.ChainNodeSet {
		return &appsv1.ChainNodeSet{
			ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default"},
			Spec: appsv1.ChainNodeSetSpec{Nodes: []appsv1.NodeGroupSpec{{
				Name:      "validators",
				Instances: ptr.To(instances),
				Validator: &appsv1.NodeSetValidatorConfig{Init: initConfig},
			}}},
			Status: appsv1.ChainNodeSetStatus{ChainID: "test-localnet", Validators: validators},
		}
	}

	t.Run("matching status is accepted", func(t *testing.T) {
		nodeSet := base(2, []appsv1.ChainNodeSetValidatorStatus{
			{Name: "test-nodeset-validators-0", Group: "validators", Init: true},
			{Name: "test-nodeset-validators-1", Group: "validators", Init: true},
		})
		_, err := validateForReconcile(nodeSet)
		assert.NoError(t, err)
	})

	t.Run("scale up is rejected", func(t *testing.T) {
		nodeSet := base(3, []appsv1.ChainNodeSetValidatorStatus{
			{Name: "test-nodeset-validators-0", Group: "validators", Init: true},
			{Name: "test-nodeset-validators-1", Group: "validators", Init: true},
		})
		_, err := validateForReconcile(nodeSet)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cannot be added with webhooks disabled")
	})

	t.Run("scale down is rejected", func(t *testing.T) {
		nodeSet := base(1, []appsv1.ChainNodeSetValidatorStatus{
			{Name: "test-nodeset-validators-0", Group: "validators", Init: true},
			{Name: "test-nodeset-validators-1", Group: "validators", Init: true},
		})
		_, err := validateForReconcile(nodeSet)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cannot be removed or converted")
	})

	t.Run("rename is rejected", func(t *testing.T) {
		nodeSet := base(2, []appsv1.ChainNodeSetValidatorStatus{
			{Name: "test-nodeset-validators-0", Group: "validators", Init: true},
			{Name: "test-nodeset-validators-1", Group: "validators", Init: true},
		})
		nodeSet.Spec.Nodes[0].Name = "renamed"
		_, err := validateForReconcile(nodeSet)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cannot be removed or converted")
	})
}

// TestValidateForReconcileAllowsLegacyEmptyValidatorStatus verifies that a pre-existing ChainNodeSet
// upgraded into this controller version — genesis already created (chainID set) but .status.validators
// not yet populated (the field is new) — is not rejected on the no-webhook path. Rejecting it would
// strand the running chain, since validation runs before ensureValidator can backfill the slice.
func TestValidateForReconcileAllowsLegacyEmptyValidatorStatus(t *testing.T) {
	initConfig := &appsv1.GenesisInitConfig{ChainID: "test-localnet", Assets: []string{"1unibi"}, StakeAmount: "1unibi"}
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default"},
		Spec: appsv1.ChainNodeSetSpec{Nodes: []appsv1.NodeGroupSpec{{
			Name:      "validators",
			Instances: ptr.To(1),
			Validator: &appsv1.NodeSetValidatorConfig{Init: initConfig},
		}}},
		Status: appsv1.ChainNodeSetStatus{ChainID: "test-localnet"},
	}
	_, err := validateForReconcile(nodeSet)
	require.NoError(t, err)
}

// TestValidateForReconcileRejectsGenesisSigningMaterialChange verifies that, on the no-webhook path,
// changing the resolved signing material of a recorded genesis validator is rejected — its consensus
// key is part of the immutable genesis validator set and cannot change after genesis.
func TestValidateForReconcileRejectsGenesisSigningMaterialChange(t *testing.T) {
	initConfig := &appsv1.GenesisInitConfig{ChainID: "test-localnet", Assets: []string{"1unibi"}, StakeAmount: "1unibi"}
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default"},
		Spec: appsv1.ChainNodeSetSpec{Nodes: []appsv1.NodeGroupSpec{{
			Name:      "validators",
			Instances: ptr.To(1),
			Validator: &appsv1.NodeSetValidatorConfig{Init: initConfig},
		}}},
		Status: appsv1.ChainNodeSetStatus{ChainID: "test-localnet"},
	}
	// Record the genesis validator with the fingerprint of its current signing material, exactly as
	// ensureValidator would.
	cfg := deriveGroupValidatorConfig(nodeSet, "validators", 0, 1, nodeSet.Spec.Nodes[0].Validator)
	nodeSet.Status.Validators = []appsv1.ChainNodeSetValidatorStatus{{
		Name:             "test-nodeset-validators-0",
		Group:            "validators",
		Init:             true,
		SigningKeyDigest: cfg.GenesisSigningFingerprint("test-nodeset-validators-0-priv-key"),
	}}

	// Unchanged signing material is accepted.
	_, err := validateForReconcile(nodeSet)
	require.NoError(t, err)

	// Rotating the validator's private-key secret after genesis is rejected.
	nodeSet.Spec.Nodes[0].Validator.PrivateKeySecret = ptr.To("rotated-priv-key")
	_, err = validateForReconcile(nodeSet)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "signing material")
}

// TestValidateForReconcileRejectsRemovingGenesisInitValidator verifies that, on the no-webhook path,
// removing or converting a recorded genesis-initializing validator is rejected even when the desired
// init set is empty (e.g. switching the group to createValidator and supplying an external genesis).
// Its voting power remains in the immutable genesis, so deleting the ChainNode can halt the chain.
func TestValidateForReconcileRejectsRemovingGenesisInitValidator(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default"},
		Spec: appsv1.ChainNodeSetSpec{
			Genesis: &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
			Nodes: []appsv1.NodeGroupSpec{{
				Name:      "validators",
				Instances: ptr.To(1),
				Validator: &appsv1.NodeSetValidatorConfig{CreateValidator: &appsv1.CreateValidatorConfig{}},
			}},
		},
		Status: appsv1.ChainNodeSetStatus{
			ChainID: "test-localnet",
			Validators: []appsv1.ChainNodeSetValidatorStatus{
				{Name: "test-nodeset-validators-0", Group: "validators", Init: true},
			},
		},
	}
	_, err := validateForReconcile(nodeSet)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot be removed or converted")
}

// TestValidateForReconcileRejectsAddingInitToExternalChain verifies that, on the no-webhook path, adding
// a genesis-initializing validator to a running chain whose recorded validators are all non-init (e.g.
// an external-genesis chain with createValidator validators) is rejected: the immutable genesis was
// already created without it.
func TestValidateForReconcileRejectsAddingInitToExternalChain(t *testing.T) {
	initConfig := &appsv1.GenesisInitConfig{ChainID: "test-localnet", Assets: []string{"1unibi"}, StakeAmount: "1unibi"}
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default"},
		Spec: appsv1.ChainNodeSetSpec{
			Nodes: []appsv1.NodeGroupSpec{
				{Name: "validators", Instances: ptr.To(1), Validator: &appsv1.NodeSetValidatorConfig{Init: initConfig}},
			},
		},
		Status: appsv1.ChainNodeSetStatus{
			ChainID: "test-localnet",
			Validators: []appsv1.ChainNodeSetValidatorStatus{
				{Name: "test-nodeset-joiners-0", Group: "joiners"}, // recorded createValidator, not init
			},
		},
	}
	_, err := validateForReconcile(nodeSet)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot be added with webhooks disabled")
}

// TestValidateForReconcileEmptyStatusGenesisSourceMarker verifies the empty-.status.validators case is
// gated by the recorded genesis source: adding init validators is rejected when the genesis was imported
// externally (GenesisInitGenerated=false), but allowed when it was init-generated or the source is
// unknown (nil, a pre-marker legacy chain whose validator slice gets backfilled).
func TestValidateForReconcileEmptyStatusGenesisSourceMarker(t *testing.T) {
	initConfig := &appsv1.GenesisInitConfig{ChainID: "test-localnet", Assets: []string{"1unibi"}, StakeAmount: "1unibi"}
	mk := func(src *bool) *appsv1.ChainNodeSet {
		return &appsv1.ChainNodeSet{
			ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default"},
			Spec: appsv1.ChainNodeSetSpec{
				Nodes: []appsv1.NodeGroupSpec{
					{Name: "validators", Instances: ptr.To(1), Validator: &appsv1.NodeSetValidatorConfig{Init: initConfig}},
				},
			},
			Status: appsv1.ChainNodeSetStatus{ChainID: "test-localnet", GenesisInitGenerated: src},
		}
	}

	// External genesis source: adding an init validator is rejected.
	_, err := validateForReconcile(mk(ptr.To(false)))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "imported an external genesis")

	// Init-generated source: allowed (this is the running init chain).
	_, err = validateForReconcile(mk(ptr.To(true)))
	require.NoError(t, err)

	// Unknown source (legacy, pre-marker): allowed so ensureValidator can backfill the slice.
	_, err = validateForReconcile(mk(nil))
	require.NoError(t, err)
}

// TestValidateForReconcileAllowsPureCreateValidatorChain verifies that a ChainNodeSet consuming an
// external genesis with only createValidator validators is not falsely rejected: its recorded
// validators are not genesis-initializing (not Init-flagged), so they are not genesis-protected.
func TestValidateForReconcileAllowsPureCreateValidatorChain(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default"},
		Spec: appsv1.ChainNodeSetSpec{
			Genesis: &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
			Nodes: []appsv1.NodeGroupSpec{{
				Name:      "joiners",
				Instances: ptr.To(2),
				Validator: &appsv1.NodeSetValidatorConfig{CreateValidator: &appsv1.CreateValidatorConfig{}},
			}},
		},
		Status: appsv1.ChainNodeSetStatus{
			ChainID: "test-localnet",
			Validators: []appsv1.ChainNodeSetValidatorStatus{
				{Name: "test-nodeset-joiners-0", Group: "joiners"},
				{Name: "test-nodeset-joiners-1", Group: "joiners"},
			},
		},
	}
	_, err := validateForReconcile(nodeSet)
	require.NoError(t, err)
}

// cosmosignerVaultBackend builds a pre-provisioned (uploadGenerated=false) Vault backend.
func cosmosignerVaultBackend() appsv1.CosmosignerBackend {
	return appsv1.CosmosignerBackend{Vault: &appsv1.CosmosignerVaultBackend{
		Address:     "https://vault.example:8200",
		KeyName:     "val-key",
		TokenSecret: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "vault-token"}, Key: "token"},
	}}
}

// TestValidateForReconcileRejectsSentryReplicaChange verifies the no-webhook path enforces raft
// replica immutability for a sentry-mode signer, which never records a signing digest. The recorded
// Status.CosmosignerReplicas is what makes a later replica change rejectable.
func TestValidateForReconcileRejectsSentryReplicaChange(t *testing.T) {
	mk := func(replicas int32) *appsv1.ChainNodeSet {
		return &appsv1.ChainNodeSet{
			ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default"},
			Spec: appsv1.ChainNodeSetSpec{
				Genesis: &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
				Cosmosigner: &appsv1.Cosmosigner{
					NodeGroups: []string{"fullnodes"},
					Replicas:   ptr.To(replicas),
					Backend:    cosmosignerVaultBackend(),
				},
				Nodes: []appsv1.NodeGroupSpec{{Name: "fullnodes", Instances: ptr.To(3)}},
			},
			Status: appsv1.ChainNodeSetStatus{ChainID: "test-localnet", CosmosignerReplicas: ptr.To(int32(3))},
		}
	}

	// Same replica count as recorded: accepted.
	_, err := validateForReconcile(mk(3))
	require.NoError(t, err)

	// Changed replica count: rejected — the raft membership cannot be migrated.
	_, err = validateForReconcile(mk(5))
	require.Error(t, err)
	assert.Contains(t, err.Error(), ".spec.cosmosigner.replicas is immutable")
}

// cosmosignerValidatorNodeSet builds an established (chainID set) ChainNodeSet whose validator group
// is targeted by a cosmosigner with the given backend.
func cosmosignerValidatorNodeSet(backend appsv1.CosmosignerBackend) *appsv1.ChainNodeSet {
	return &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default"},
		Spec: appsv1.ChainNodeSetSpec{
			Genesis: &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
			Cosmosigner: &appsv1.Cosmosigner{
				NodeGroups: []string{"validators"},
				Backend:    backend,
			},
			Nodes: []appsv1.NodeGroupSpec{{
				Name:      "validators",
				Instances: ptr.To(1),
				Validator: &appsv1.NodeSetValidatorConfig{PrivateKeySecret: ptr.To("val-priv-key")},
			}},
		},
		Status: appsv1.ChainNodeSetStatus{ChainID: "test-localnet"},
	}
}

// TestValidateForReconcileAllowsFirstSignerRollout verifies the no-webhook path admits a
// validator-targeted signer that has not yet recorded a rollout digest — including a pre-provisioned
// Vault backend. Blocking it here would deadlock the first rollout (chainID is set before
// ensureCosmosigner can observe rollout and record the digest); add/reroute safety that needs the
// previous spec is left to the admission webhook.
func TestValidateForReconcileAllowsFirstSignerRollout(t *testing.T) {
	// Pre-provisioned Vault key, no recorded digest (first rollout in progress): accepted.
	_, err := validateForReconcile(cosmosignerValidatorNodeSet(cosmosignerVaultBackend()))
	require.NoError(t, err)

	// Software backend, no recorded digest: accepted.
	_, err = validateForReconcile(cosmosignerValidatorNodeSet(appsv1.CosmosignerBackend{Software: &appsv1.CosmosignerSoftwareBackend{}}))
	require.NoError(t, err)
}

// TestValidateForReconcileRejectsRecordedSignerKeyChange verifies that once a validator-targeted
// signer's digest is recorded, changing its signing configuration (here the Vault key name) is
// rejected — the live signer's key is fixed on-chain — while removing the signer is allowed (a safe
// rollback to the validator's own key is judged by the admission webhook, not blocked here).
func TestValidateForReconcileRejectsRecordedSignerKeyChange(t *testing.T) {
	recorded := cosmosignerValidatorNodeSet(cosmosignerVaultBackend())
	recorded.Status.CosmosignerSigningDigest = recorded.CosmosignerSigningDigest()
	recorded.Status.CosmosignerReplicas = ptr.To(recorded.Spec.Cosmosigner.GetReplicas())

	// Unchanged config: accepted.
	_, err := validateForReconcile(recorded)
	require.NoError(t, err)

	// Changed Vault key on the live signer: rejected.
	changed := recorded.DeepCopy()
	changed.Spec.Cosmosigner.Backend.Vault.KeyName = "different-key"
	_, err = validateForReconcile(changed)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "immutable after the chain is established")

	// Removing the signer entirely: allowed on the no-webhook path (deferred to the admission webhook).
	removed := recorded.DeepCopy()
	removed.Spec.Cosmosigner = nil
	_, err = validateForReconcile(removed)
	require.NoError(t, err)
}

// TestValidateForReconcileAllowsRecordedValidatorSigner verifies that once a validator-targeted
// signer's digest is recorded (it rolled out and served), the same spec passes and a pre-provisioned
// backend is no longer refused — the recorded digest proves the key is the one in effect.
func TestValidateForReconcileAllowsRecordedValidatorSigner(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default"},
		Spec: appsv1.ChainNodeSetSpec{
			Genesis: &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
			Cosmosigner: &appsv1.Cosmosigner{
				NodeGroups: []string{"validators"},
				Backend:    cosmosignerVaultBackend(),
			},
			Nodes: []appsv1.NodeGroupSpec{{
				Name:      "validators",
				Instances: ptr.To(1),
				Validator: &appsv1.NodeSetValidatorConfig{PrivateKeySecret: ptr.To("val-priv-key")},
			}},
		},
		Status: appsv1.ChainNodeSetStatus{ChainID: "test-localnet"},
	}
	nodeSet.Status.CosmosignerSigningDigest = nodeSet.CosmosignerSigningDigest()

	_, err := validateForReconcile(nodeSet)
	require.NoError(t, err)
}

// TestValidateForReconcilePostEstablishmentSignerAddition verifies the write-once at-establishment
// marker: a validator-targeted pre-provisioned signer whose identity matches the marker (it was the
// establishing configuration) is admitted even before its rollout digest is recorded, while one whose
// identity was introduced AFTER establishment is rejected unless the backend provably imports the
// registered key.
func TestValidateForReconcilePostEstablishmentSignerAddition(t *testing.T) {
	establishing := cosmosignerValidatorNodeSet(cosmosignerVaultBackend())
	establishing.Status.CosmosignerAtEstablishment = ptr.To(establishing.CosmosignerSigningIdentity())

	// Identity matches the establishment record (first rollout of the establishing signer): admitted.
	_, err := validateForReconcile(establishing)
	require.NoError(t, err)

	// Established with NO signer, pre-provisioned Vault signer added later: rejected.
	added := cosmosignerValidatorNodeSet(cosmosignerVaultBackend())
	added.Status.CosmosignerAtEstablishment = ptr.To("")
	_, err = validateForReconcile(added)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "pre-provisioned Vault/GCP key")

	// Same late addition but with uploadGenerated (import verifies the key): admitted.
	importing := cosmosignerValidatorNodeSet(appsv1.CosmosignerBackend{Vault: &appsv1.CosmosignerVaultBackend{
		Address:         "https://vault.example:8200",
		KeyName:         "val-key",
		TokenSecret:     &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "vault-token"}, Key: "token"},
		UploadGenerated: true,
	}})
	importing.Status.CosmosignerAtEstablishment = ptr.To("")
	_, err = validateForReconcile(importing)
	require.NoError(t, err)

	// Marker not recorded yet (nil): admitted — the controller records it on the same reconcile.
	unrecorded := cosmosignerValidatorNodeSet(cosmosignerVaultBackend())
	_, err = validateForReconcile(unrecorded)
	require.NoError(t, err)
}
