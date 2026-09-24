package chainnodeset

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	appsv1 "github.com/voluzi/cosmopilot/v3/api/v1"
	"github.com/voluzi/cosmopilot/v3/internal/cosmosigner"
)

func claimSelector(name string) *corev1.SecretKeySelector {
	return &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: "value"}
}

func claimSecret(name string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}, Data: map[string][]byte{"value": []byte("x")}}
}

func claimTestBackend(t *testing.T, backend appsv1.CosmosignerBackend, objects ...client.Object) (cosmosigner.Backend, error) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	r := &Reconciler{Client: fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(), Scheme: scheme}
	nodeSet := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{Name: "nodes", Namespace: "default"}}
	return r.cosmosignerBackend(context.Background(), nodeSet, appsv1.ResolvedSigner{
		Name: "nodes-signer", Spec: &appsv1.Cosmosigner{Backend: backend},
	})
}

func TestCosmosignerBackendCarriesClusterBindingSettings(t *testing.T) {
	backend, err := claimTestBackend(t, appsv1.CosmosignerBackend{Vault: &appsv1.CosmosignerVaultBackend{
		Address: "https://vault:8200", KeyName: "validator",
		TokenSecret:      claimSelector("vault-token"),
		BindingMount:     ptr.To("claims"),
		ClaimTokenSecret: claimSelector("vault-admin"),
	}}, claimSecret("vault-token"), claimSecret("vault-admin"))
	require.NoError(t, err)
	require.Equal(t, "claims", backend.Vault.BindingMount)
	require.Equal(t, "vault-admin", backend.Vault.ClaimTokenSecret.Name)

	backend, err = claimTestBackend(t, appsv1.CosmosignerBackend{GcpKMS: &appsv1.CosmosignerGcpKmsBackend{
		KeyVersion:             "projects/p/locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1",
		ClaimCredentialsSecret: claimSelector("kms-admin"),
	}}, claimSecret("kms-admin"))
	require.NoError(t, err)
	require.Equal(t, "kms-admin", backend.GCP.ClaimCredentialsSecret.Name)
}

// A missing claim Secret would roll out signers that can never mount it; refuse before deploying.
func TestCosmosignerBackendRequiresClaimSecrets(t *testing.T) {
	_, err := claimTestBackend(t, appsv1.CosmosignerBackend{Vault: &appsv1.CosmosignerVaultBackend{
		Address: "https://vault:8200", KeyName: "validator",
		TokenSecret:      claimSelector("vault-token"),
		ClaimTokenSecret: claimSelector("vault-admin"),
	}}, claimSecret("vault-token"))
	require.ErrorContains(t, err, "Vault claim token")

	_, err = claimTestBackend(t, appsv1.CosmosignerBackend{GcpKMS: &appsv1.CosmosignerGcpKmsBackend{
		KeyVersion:             "projects/p/locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1",
		ClaimCredentialsSecret: claimSelector("kms-admin"),
	}})
	require.ErrorContains(t, err, "GCP claim credentials")
}
