package v1

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

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

	// Removing the signer entirely: allowed (deferred to the admission webhook).
	removed := recorded.DeepCopy()
	removed.Spec.Cosmosigner = nil
	if _, err := removed.Validate(nil); err != nil {
		t.Fatalf("removing the signer must be allowed on the no-webhook path, got: %v", err)
	}
}
