package cosmosigner

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
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

// TestRequireRaftTLSSecret verifies the raft mTLS preflight: nil (not configured) is accepted, a
// missing Secret or one lacking any of tls.crt/tls.key/ca.crt errors, and a complete Secret passes.
func TestRequireRaftTLSSecret(t *testing.T) {
	const ns, name = "default", "raft-tls"
	full := map[string][]byte{"tls.crt": []byte("c"), "tls.key": []byte("k"), "ca.crt": []byte("a")}

	// nil: accepted.
	c := fake.NewClientBuilder().WithScheme(lockScheme(t)).Build()
	if err := RequireRaftTLSSecret(context.Background(), c, ns, nil); err != nil {
		t.Fatalf("nil raft TLS secret must be accepted, got %v", err)
	}

	// missing Secret: error.
	if err := RequireRaftTLSSecret(context.Background(), c, ns, ptr.To(name)); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing raft TLS secret must error, got %v", err)
	}

	// missing one key: error naming the key.
	for k := range full {
		partial := map[string][]byte{}
		for kk, vv := range full {
			if kk != k {
				partial[kk] = vv
			}
		}
		sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Data: partial}
		cc := fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(sec).Build()
		if err := RequireRaftTLSSecret(context.Background(), cc, ns, ptr.To(name)); err == nil || !strings.Contains(err.Error(), k) {
			t.Fatalf("raft TLS secret missing %q must error, got %v", k, err)
		}
	}

	// complete: accepted.
	sec := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Data: full}
	cc := fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(sec).Build()
	if err := RequireRaftTLSSecret(context.Background(), cc, ns, ptr.To(name)); err != nil {
		t.Fatalf("complete raft TLS secret must be accepted, got %v", err)
	}
}

// TestRequireServiceAccount verifies the ServiceAccount preflight: empty (namespace default) is
// accepted, a missing named SA errors, and an existing one passes.
func TestRequireServiceAccount(t *testing.T) {
	const ns, name = "default", "signer-sa"
	c := fake.NewClientBuilder().WithScheme(lockScheme(t)).Build()
	if err := RequireServiceAccount(context.Background(), c, ns, ""); err != nil {
		t.Fatalf("empty SA (default) must be accepted, got %v", err)
	}
	if err := RequireServiceAccount(context.Background(), c, ns, name); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing SA must error, got %v", err)
	}
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	cc := fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(sa).Build()
	if err := RequireServiceAccount(context.Background(), cc, ns, name); err != nil {
		t.Fatalf("existing SA must be accepted, got %v", err)
	}
}
