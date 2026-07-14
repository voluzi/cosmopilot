package cosmosigner

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestRequireSecretSelector verifies the shared signer-Secret preflight: a nil selector is accepted, a
// missing Secret or an empty/absent key errors, and a present non-empty key passes — the single
// behaviour both the ChainNode and ChainNodeSet signing paths now rely on.
func TestRequireSecretSelector(t *testing.T) {
	const ns = "default"
	sel := &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "vault-token"}, Key: "token"}

	// nil selector: accepted.
	c := fake.NewClientBuilder().WithScheme(lockScheme(t)).Build()
	if err := RequireSecretSelector(context.Background(), c, ns, "Vault token", nil); err != nil {
		t.Fatalf("nil selector must be accepted, got %v", err)
	}

	// missing Secret: error mentions the purpose and "not found".
	if err := RequireSecretSelector(context.Background(), c, ns, "Vault token", sel); err == nil ||
		!strings.Contains(err.Error(), "not found") || !strings.Contains(err.Error(), "Vault token") {
		t.Fatalf("missing secret must error, got %v", err)
	}

	// present Secret but empty key: error mentions "missing key".
	empty := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "vault-token", Namespace: ns}, Data: map[string][]byte{"token": {}}}
	c = fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(empty).Build()
	if err := RequireSecretSelector(context.Background(), c, ns, "Vault token", sel); err == nil ||
		!strings.Contains(err.Error(), "missing key") {
		t.Fatalf("empty key must error, got %v", err)
	}

	// present non-empty key: accepted.
	ok := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "vault-token", Namespace: ns}, Data: map[string][]byte{"token": []byte("s3cret")}}
	c = fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(ok).Build()
	if err := RequireSecretSelector(context.Background(), c, ns, "Vault token", sel); err != nil {
		t.Fatalf("valid secret must be accepted, got %v", err)
	}
}
