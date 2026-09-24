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
