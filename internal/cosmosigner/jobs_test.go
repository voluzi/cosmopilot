package cosmosigner

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/voluzi/cosmopilot/v2/internal/cometbft"
)

func TestParsePublicKeyOutput(t *testing.T) {
	const want = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	got, err := ParsePublicKeyOutput("address:        0000000000000000000000000000000000000000\npubkey (base64): " + want + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("ParsePublicKeyOutput() = %q, want %q", got, want)
	}

	if _, err := ParsePublicKeyOutput("pubkey (base64): not-base64\n"); err == nil {
		t.Fatal("malformed public key output must be rejected")
	}
	if _, err := ParsePublicKeyOutput("address: missing-key\n"); err == nil {
		t.Fatal("missing public key output must be rejected")
	}
}

func TestPublicKeyFromSecret(t *testing.T) {
	key, err := cometbft.GeneratePrivKey()
	if err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "validator-key", Namespace: "default"}, Data: map[string][]byte{
		"priv_validator_key.json": key,
	}}
	c := fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(secret).Build()

	got, err := PublicKeyFromSecret(context.Background(), c, "default", secret.Name)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := cometbft.LoadPrivKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if got != parsed.PubKey.Value {
		t.Fatalf("PublicKeyFromSecret() = %q, want %q", got, parsed.PubKey.Value)
	}
}
