package cosmosigner

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/voluzi/cosmopilot/v4/internal/cometbft"
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

func TestJobPodPullPolicyFollowsTheImageTag(t *testing.T) {
	tests := []struct {
		image string
		want  corev1.PullPolicy
	}{
		{image: "ghcr.io/voluzi/cosmosigner:0.2.1", want: ""},
		{image: "ghcr.io/voluzi/cosmosigner:edge", want: corev1.PullAlways},
	}
	for _, tt := range tests {
		j := JobRunner{Params: Params{Name: "signer", Namespace: "ns", Image: tt.image}}
		if got := j.buildPod("pubkey", nil, nil, nil, 60).Spec.Containers[0].ImagePullPolicy; got != tt.want {
			t.Errorf("%s: ImagePullPolicy = %q, want %q", tt.image, got, tt.want)
		}
	}
}
