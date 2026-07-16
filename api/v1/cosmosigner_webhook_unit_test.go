package v1

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func TestValidateCosmosignerReservedNameRejectsStatefulSetChildren(t *testing.T) {
	for _, name := range []string{"foo-signer-0", "foo-signer-12", "data-foo-signer-0"} {
		if err := ValidateCosmosignerReservedName(name, true); err == nil {
			t.Fatalf("metadata.name %q must be reserved for a cosmosigner StatefulSet child", name)
		}
	}
	if err := ValidateCosmosignerReservedName("foo-signer-canary", true); err != nil {
		t.Fatalf("non-ordinal suffix must remain available, got %v", err)
	}
}

// TestNodeSetInitValidatorSameKeyMigration exercises Validate directly (no controller): an
// established genesis-init ChainNodeSet migrating from Vault tmKMS to a cosmosigner Vault backend
// on the SAME transit key must be accepted (both the cosmosigner validation and the genesis
// immutability fingerprint), while migrating to a DIFFERENT key must be rejected.
func TestNodeSetInitValidatorSameKeyMigration(t *testing.T) {
	tokenSecret := &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "vault-token"},
		Key:                  "token",
	}

	base := func() *ChainNodeSet {
		return &ChainNodeSet{
			ObjectMeta: metav1.ObjectMeta{Name: "mychain"},
			Spec: ChainNodeSetSpec{
				App: AppSpec{Image: "img", App: "appd", Version: ptr.To("1.0.0")},
				Validator: &NodeSetValidatorConfig{
					Init: &GenesisInitConfig{ChainID: "test-1", Assets: []string{"1000stake"}, StakeAmount: "100stake"},
					TmKMS: &TmKMS{Provider: TmKmsProvider{Hashicorp: &TmKmsHashicorpProvider{
						Address:     "https://vault:8200",
						Key:         "myval",
						TokenSecret: tokenSecret,
					}}},
				},
				Nodes: []NodeGroupSpec{{Name: "fullnodes"}},
			},
			Status: ChainNodeSetStatus{ChainID: "test-1"},
		}
	}

	migrated := func(keyName string) *ChainNodeSet {
		n := base()
		n.Spec.Validator.TmKMS = nil
		n.Spec.Cosmosigner = &Cosmosigner{Backend: CosmosignerBackend{Vault: &CosmosignerVaultBackend{
			Address:     "https://vault:8200",
			KeyName:     keyName,
			TokenSecret: tokenSecret,
		}}}
		return n
	}

	// Same key: the documented migration must be accepted.
	if _, err := migrated("myval").Validate(base()); err != nil {
		t.Fatalf("same-key tmKMS→cosmosigner migration must be allowed, got: %v", err)
	}

	// Different key: rejected — by the registers rule (a non-matching pre-provisioned key needs
	// software/uploadGenerated) since the same-key waiver doesn't apply.
	if _, err := migrated("other-key").Validate(base()); err == nil {
		t.Fatal("different-key migration must be rejected")
	}
}

// TestInitVaultSignerUploadGeneratedAutoDefault verifies the documented auto-default: a
// genesis-initializing validator with a Vault cosmosigner backend is accepted even when
// uploadGenerated is omitted (the controller imports the generated key implicitly), on both CRDs.
func TestInitVaultSignerUploadGeneratedAutoDefault(t *testing.T) {
	tokenSecret := &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "vault-token"},
		Key:                  "token",
	}
	vaultBackend := CosmosignerBackend{Vault: &CosmosignerVaultBackend{
		Address:     "https://vault:8200",
		KeyName:     "myval",
		TokenSecret: tokenSecret,
		// UploadGenerated deliberately omitted.
	}}
	initCfg := &GenesisInitConfig{ChainID: "test-1", Assets: []string{"1000stake"}, StakeAmount: "100stake"}

	nodeSet := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "mychain"},
		Spec: ChainNodeSetSpec{
			App:         AppSpec{Image: "img", App: "appd", Version: ptr.To("1.0.0")},
			Validator:   &NodeSetValidatorConfig{Init: initCfg},
			Nodes:       []NodeGroupSpec{{Name: "fullnodes"}},
			Cosmosigner: &Cosmosigner{Backend: vaultBackend},
		},
	}
	if _, err := nodeSet.Validate(nil); err != nil {
		t.Fatalf("init + Vault signer without explicit uploadGenerated must be accepted (auto-default), got: %v", err)
	}

	chainNode := &ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "mynode"},
		Spec: ChainNodeSpec{
			App:         AppSpec{Image: "img", App: "appd", Version: ptr.To("1.0.0")},
			Validator:   &ValidatorConfig{Init: initCfg},
			Cosmosigner: &Cosmosigner{Backend: vaultBackend},
		},
	}
	if _, err := chainNode.Validate(nil); err != nil {
		t.Fatalf("standalone init + Vault signer without explicit uploadGenerated must be accepted, got: %v", err)
	}
}

// TestChainNodeNoWebhookSentryReplicaImmutable exercises the standalone ChainNode no-webhook path
// (Validate with a nil old): a sentry-mode signer records no signing digest, so raft replica
// immutability is enforced from the recorded Status.CosmosignerReplicas instead.
func TestChainNodeNoWebhookSentryReplicaImmutable(t *testing.T) {
	mk := func(replicas int32) *ChainNode {
		return &ChainNode{
			ObjectMeta: metav1.ObjectMeta{Name: "mynode"},
			Spec: ChainNodeSpec{
				App:     AppSpec{Image: "img", App: "appd", Version: ptr.To("1.0.0")},
				Genesis: &GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
				Cosmosigner: &Cosmosigner{
					Replicas: ptr.To(replicas),
					Backend:  CosmosignerBackend{Software: &CosmosignerSoftwareBackend{PrivateKeySecret: ptr.To("sentry-priv-key")}},
				},
			},
			Status: ChainNodeStatus{ChainID: "test-1", CosmosignerReplicas: ptr.To(int32(3))},
		}
	}

	if _, err := mk(3).Validate(nil); err != nil {
		t.Fatalf("unchanged sentry replica count must be accepted, got: %v", err)
	}
	if _, err := mk(5).Validate(nil); err == nil {
		t.Fatal("changing sentry signer replica count with webhooks disabled must be rejected")
	}
}

// TestChainNodeNoWebhookSignerLifecycle exercises the standalone ChainNode no-webhook path (Validate
// with a nil old): a first signer rollout (no recorded digest) is admitted — including a
// pre-provisioned Vault backend, so the rollout is not deadlocked — while modifying a signer that has
// already recorded a digest is rejected, and removal is allowed (deferred to the admission webhook).
func TestChainNodeNoWebhookSignerLifecycle(t *testing.T) {
	tokenSecret := &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "vault-token"},
		Key:                  "token",
	}
	base := func(backend CosmosignerBackend) *ChainNode {
		return &ChainNode{
			ObjectMeta: metav1.ObjectMeta{Name: "mynode"},
			Spec: ChainNodeSpec{
				App:         AppSpec{Image: "img", App: "appd", Version: ptr.To("1.0.0")},
				Genesis:     &GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
				Validator:   &ValidatorConfig{PrivateKeySecret: ptr.To("val-priv-key")},
				Cosmosigner: &Cosmosigner{Backend: backend},
			},
			Status: ChainNodeStatus{ChainID: "test-1"},
		}
	}

	// First rollout of a pre-provisioned Vault validator signer (no recorded digest): admitted.
	preProvisioned := base(CosmosignerBackend{Vault: &CosmosignerVaultBackend{Address: "https://vault:8200", KeyName: "val-key", TokenSecret: tokenSecret}})
	if _, err := preProvisioned.Validate(nil); err != nil {
		t.Fatalf("first rollout of a pre-provisioned validator signer must be admitted, got: %v", err)
	}

	// Once its digest is recorded, changing the live signer's Vault key is rejected.
	recorded := preProvisioned.DeepCopy()
	recorded.Status.CosmosignerSigningDigest = recorded.CosmosignerSigningDigest()
	changed := recorded.DeepCopy()
	changed.Spec.Cosmosigner.Backend.Vault.KeyName = "different-key"
	if _, err := changed.Validate(nil); err == nil {
		t.Fatal("changing a recorded signer's key with webhooks disabled must be rejected")
	}

	// Removing a signer whose digest predates the serving-identity field (identity unverifiable):
	// rejected conservatively.
	removedLegacy := recorded.DeepCopy()
	removedLegacy.Spec.Cosmosigner = nil
	if _, err := removedLegacy.Validate(nil); err == nil {
		t.Fatal("removing a legacy-digest signer with no recorded serving identity must be rejected")
	}

	// Removing a Vault signer whose serving identity is recorded but unreachable through the
	// validator's own path: rejected.
	removedVault := recorded.DeepCopy()
	removedVault.Status.CosmosignerServingIdentity = recorded.CosmosignerSigningIdentity()
	removedVault.Spec.Cosmosigner = nil
	if _, err := removedVault.Validate(nil); err == nil {
		t.Fatal("removing a pre-provisioned Vault signer must be rejected: the validator would fall back to a different local key")
	}

	// A post-establishment migration whose locks exist but whose rollout identity has not been
	// recorded yet must also fail closed: the signer may already have started serving.
	removedPending := preProvisioned.DeepCopy()
	removedPending.Status.CosmosignerReplicas = ptr.To(int32(1))
	removedPending.Status.CosmosignerStateStorageSize = "1Gi"
	removedPending.Spec.Cosmosigner = nil
	if _, err := removedPending.Validate(nil); err == nil {
		t.Fatal("removing a migrated signer before its serving identity is recorded must be rejected")
	}

	// Removing both the signer and validator in the same no-webhook edit must not erase the evidence
	// that the in-flight signer targeted an on-chain validator.
	removedPendingAndValidator := preProvisioned.DeepCopy()
	removedPendingAndValidator.Status.CosmosignerReplicas = ptr.To(int32(1))
	removedPendingAndValidator.Status.CosmosignerStateStorageSize = "1Gi"
	removedPendingAndValidator.Status.CosmosignerValidatorTargeted = ptr.To(true)
	removedPendingAndValidator.Spec.Cosmosigner = nil
	removedPendingAndValidator.Spec.Validator = nil
	if _, err := removedPendingAndValidator.Validate(nil); err == nil {
		t.Fatal("removing an in-flight validator signer together with the validator must be rejected")
	}

	// A pre-rollout sentry signer protects no in-cluster validator identity, so its recorded false
	// marker keeps no-webhook removal available.
	pendingSentry := &ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "sentry"},
		Spec: ChainNodeSpec{
			App:         AppSpec{Image: "img", App: "appd", Version: ptr.To("1.0.0")},
			Genesis:     &GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
			Cosmosigner: &Cosmosigner{Backend: CosmosignerBackend{Software: &CosmosignerSoftwareBackend{PrivateKeySecret: ptr.To("sentry-key")}}},
		},
		Status: ChainNodeStatus{
			ChainID:                      "test-1",
			CosmosignerReplicas:          ptr.To(int32(1)),
			CosmosignerStateStorageSize:  "1Gi",
			CosmosignerValidatorTargeted: ptr.To(false),
		},
	}
	pendingSentry.Spec.Cosmosigner = nil
	if _, err := pendingSentry.Validate(nil); err != nil {
		t.Fatalf("removing a pre-rollout sentry signer must remain allowed, got: %v", err)
	}

	// Removing a software signer that used the validator's own key: the serving identity is still
	// resolved by the validator's local path, so removal is a safe rollback.
	softwareServed := base(CosmosignerBackend{Software: &CosmosignerSoftwareBackend{}})
	softwareServed.Status.CosmosignerSigningDigest = softwareServed.CosmosignerSigningDigest()
	softwareServed.Status.CosmosignerServingIdentity = softwareServed.CosmosignerSigningIdentity()
	softwareServed.Spec.Cosmosigner = nil
	if _, err := softwareServed.Validate(nil); err != nil {
		t.Fatalf("removing a software signer backed by the validator's own key must be allowed, got: %v", err)
	}
}
