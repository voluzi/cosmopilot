package chainnode

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	appsv1 "github.com/voluzi/cosmopilot/v4/api/v1"
)

func claimSelector(name string) *corev1.SecretKeySelector {
	return &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: "value"}
}

func claimSecret(name string) *corev1.Secret {
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}, Data: map[string][]byte{"value": []byte("x")}}
}

func claimTestReconciler(t *testing.T, objects ...client.Object) *Reconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	return &Reconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build(), Scheme: scheme}
}

func signerChainNode(backend appsv1.CosmosignerBackend) *appsv1.ChainNode {
	return &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "sentry", Namespace: "default"},
		Spec:       appsv1.ChainNodeSpec{Cosmosigner: &appsv1.Cosmosigner{Backend: backend}},
		Status:     appsv1.ChainNodeStatus{ChainID: "test-1"},
	}
}

func TestCosmosignerBackendCarriesClusterBindingSettings(t *testing.T) {
	vault := signerChainNode(appsv1.CosmosignerBackend{Vault: &appsv1.CosmosignerVaultBackend{
		Address: "https://vault:8200", KeyName: "validator",
		TokenSecret:      claimSelector("vault-token"),
		BindingMount:     ptr.To("claims"),
		ClaimTokenSecret: claimSelector("vault-admin"),
	}})
	r := claimTestReconciler(t, claimSecret("vault-token"), claimSecret("vault-admin"))
	backend, err := r.cosmosignerBackend(context.Background(), vault)
	require.NoError(t, err)
	require.Equal(t, "claims", backend.Vault.BindingMount)
	require.Equal(t, "vault-admin", backend.Vault.ClaimTokenSecret.Name)

	gcp := signerChainNode(appsv1.CosmosignerBackend{GcpKMS: &appsv1.CosmosignerGcpKmsBackend{
		KeyVersion:             "projects/p/locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1",
		ClaimCredentialsSecret: claimSelector("kms-admin"),
	}})
	backend, err = claimTestReconciler(t, claimSecret("kms-admin")).cosmosignerBackend(context.Background(), gcp)
	require.NoError(t, err)
	require.Equal(t, "kms-admin", backend.GCP.ClaimCredentialsSecret.Name)
}

// A missing claim Secret would roll out signers that can never mount it; refuse before deploying.
func TestCosmosignerBackendRequiresClaimSecrets(t *testing.T) {
	vault := signerChainNode(appsv1.CosmosignerBackend{Vault: &appsv1.CosmosignerVaultBackend{
		Address: "https://vault:8200", KeyName: "validator",
		TokenSecret:      claimSelector("vault-token"),
		ClaimTokenSecret: claimSelector("vault-admin"),
	}})
	_, err := claimTestReconciler(t, claimSecret("vault-token")).cosmosignerBackend(context.Background(), vault)
	require.ErrorContains(t, err, "Vault claim token")

	gcp := signerChainNode(appsv1.CosmosignerBackend{GcpKMS: &appsv1.CosmosignerGcpKmsBackend{
		KeyVersion:             "projects/p/locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1",
		ClaimCredentialsSecret: claimSelector("kms-admin"),
	}})
	_, err = claimTestReconciler(t).cosmosignerBackend(context.Background(), gcp)
	require.ErrorContains(t, err, "GCP claim credentials")
}
