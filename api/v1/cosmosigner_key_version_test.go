package v1

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func vaultKeySigner(keyName string, version int) *Cosmosigner {
	return &Cosmosigner{Backend: CosmosignerBackend{Vault: &CosmosignerVaultBackend{
		Address: "https://vault:8200", KeyName: keyName, KeyVersion: ptr.To(version),
		TokenSecret: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "vault-token"}, Key: "token"},
	}}}
}

func gcpKeySigner(keyVersion string) *Cosmosigner {
	return &Cosmosigner{Backend: CosmosignerBackend{GcpKMS: &CosmosignerGcpKmsBackend{KeyVersion: keyVersion}}}
}

func TestValidateCosmosignerKeyVersionChange(t *testing.T) {
	const kms = "projects/p/locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/"
	require.Error(t, validateCosmosignerKeyVersionChange("p", vaultKeySigner("val", 1), vaultKeySigner("val", 2)))
	require.Error(t, validateCosmosignerKeyVersionChange("p", gcpKeySigner(kms+"1"), gcpKeySigner(kms+"2")))
	require.NoError(t, validateCosmosignerKeyVersionChange("p", vaultKeySigner("val", 1), vaultKeySigner("val-2", 1)))
	require.NoError(t, validateCosmosignerKeyVersionChange("p", vaultKeySigner("val", 1), vaultKeySigner("val", 1)))
	require.NoError(t, validateCosmosignerKeyVersionChange("p", gcpKeySigner(kms+"1"), gcpKeySigner("projects/p/locations/l/keyRings/r/cryptoKeys/k2/cryptoKeyVersions/1")))
	require.NoError(t, validateCosmosignerKeyVersionChange("p", nil, vaultKeySigner("val", 2)))
}

func TestWebhooksRejectAKeyVersionChangeOfTheSameKey(t *testing.T) {
	oldNode := &ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "sentry", Namespace: "default"},
		Spec: ChainNodeSpec{
			App:         AppSpec{Image: "ghcr.io/example/app", App: "appd"},
			Genesis:     &GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
			Cosmosigner: vaultKeySigner("val", 1),
		},
	}
	newNode := oldNode.DeepCopy()
	newNode.Spec.Cosmosigner.Backend.Vault.KeyVersion = ptr.To(2)
	_, err := newNode.Validate(oldNode)
	require.ErrorContains(t, err, "same Vault key or Cloud KMS CryptoKey")

	oldSet := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "ns"},
		Spec: ChainNodeSetSpec{Nodes: []NodeGroupSpec{{
			Name: "sentries", Instances: ptr.To(2),
			Cosmosigner: gcpKeySigner("projects/p/locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1"),
		}}},
	}
	newSet := oldSet.DeepCopy()
	newSet.Spec.Nodes[0].Cosmosigner.Backend.GcpKMS.KeyVersion = "projects/p/locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/2"
	require.ErrorContains(t, newSet.validateCosmosignerUpdate(oldSet), "same Vault key or Cloud KMS CryptoKey")
}

func TestWebhooksRejectGcpImportKeyVersionSwitchWithAccurateMessage(t *testing.T) {
	const keyVersion = "projects/p/locations/global/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1"
	imported := &Cosmosigner{Backend: CosmosignerBackend{GcpKMS: &CosmosignerGcpKmsBackend{Import: &CosmosignerGcpKmsImport{Project: "p", KeyRing: "r", Key: "k"}}}}
	for _, reverse := range []bool{false, true} {
		t.Run(map[bool]string{false: "import to keyVersion", true: "keyVersion to import"}[reverse], func(t *testing.T) {
			oldSigner, newSigner := imported, gcpKeySigner(keyVersion)
			if reverse {
				oldSigner, newSigner = newSigner, oldSigner
			}
			t.Run("ChainNode", func(t *testing.T) {
				oldNode := &ChainNode{ObjectMeta: metav1.ObjectMeta{Name: "validator", Namespace: "default"}, Spec: ChainNodeSpec{
					App: AppSpec{Image: "ghcr.io/example/app", App: "appd"}, Genesis: &GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
					Validator: &ValidatorConfig{PrivateKeySecret: ptr.To("validator-key")}, Cosmosigner: oldSigner,
				}, Status: ChainNodeStatus{CosmosignerKeyImported: "verified", CosmosignerImportedKeyVersion: keyVersion}}
				newNode := oldNode.DeepCopy()
				newNode.Spec.Cosmosigner = newSigner
				_, err := newNode.Validate(oldNode)
				require.ErrorContains(t, err, "switching between gcpKms.import and gcpKms.keyVersion")
				require.ErrorContains(t, err, "even when keyVersion is the imported version")
				require.NotContains(t, err.Error(), "moving to another version")
			})
			for _, group := range []bool{false, true} {
				t.Run(map[bool]string{false: "ChainNodeSet top-level", true: "ChainNodeSet group"}[group], func(t *testing.T) {
					oldSet := &ChainNodeSet{ObjectMeta: metav1.ObjectMeta{Name: "set"}, Spec: ChainNodeSetSpec{Validator: &NodeSetValidatorConfig{PrivateKeySecret: ptr.To("validator-key")}, Cosmosigner: oldSigner}}
					if group {
						oldSet.Spec.Validator = nil
						oldSet.Spec.Cosmosigner = nil
						oldSet.Spec.Nodes = []NodeGroupSpec{{Name: "validators", Instances: ptr.To(1), Validator: &NodeSetValidatorConfig{PrivateKeySecret: ptr.To("validator-key")}, Cosmosigner: oldSigner}}
					}
					signer := oldSet.ResolveCosmosigners()[0]
					st := oldSet.EnsureCosmosignerStatus(signer.Name)
					st.KeyImported = "verified"
					st.ImportedKeyVersion = keyVersion
					newSet := oldSet.DeepCopy()
					if group {
						newSet.Spec.Nodes[0].Cosmosigner = newSigner
					} else {
						newSet.Spec.Cosmosigner = newSigner
					}
					err := newSet.validateCosmosignerUpdate(oldSet)
					require.ErrorContains(t, err, "switching between gcpKms.import and gcpKms.keyVersion")
					require.ErrorContains(t, err, "even when keyVersion is the imported version")
					require.NotContains(t, err.Error(), "moving to another version")
				})
			}
		})
	}
}
