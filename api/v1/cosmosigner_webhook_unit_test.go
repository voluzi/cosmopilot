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
