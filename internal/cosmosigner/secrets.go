package cosmosigner

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// RequireServiceAccount errors when a non-empty ServiceAccount name does not exist in the namespace —
// the signer StatefulSet and its one-shot import pod run as it, so a missing one keeps Kubernetes from
// starting/creating them. An empty name (the namespace default ServiceAccount) is accepted.
func RequireServiceAccount(ctx context.Context, c client.Client, namespace, name string) error {
	if name == "" {
		return nil
	}
	sa := &corev1.ServiceAccount{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, sa); err != nil {
		if errors.IsNotFound(err) {
			return fmt.Errorf("cosmosigner serviceAccountName %q not found: create it before deploying the signer", name)
		}
		return err
	}
	return nil
}

// raftTLSKeys are the files the signer pod mounts from its raft mTLS Secret (the whole Secret is
// mounted, so its data keys must be exactly these file names).
var raftTLSKeys = []string{"tls.crt", "tls.key", "ca.crt"}

// RequireRaftTLSSecret errors when the named raft mTLS Secret is missing or lacks any of tls.crt,
// tls.key, ca.crt — which the signer pod mounts at startup, so an incomplete Secret keeps every signer
// pod from coming up. Preflighted before ChainNodeSet children are retargeted. A nil name (raft TLS not
// configured) is accepted.
func RequireRaftTLSSecret(ctx context.Context, c client.Client, namespace string, name *string) error {
	if name == nil {
		return nil
	}
	secret := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: *name}, secret); err != nil {
		if errors.IsNotFound(err) {
			return fmt.Errorf("cosmosigner raft TLS secret %q not found: provide it before deploying the signer", *name)
		}
		return err
	}
	for _, k := range raftTLSKeys {
		if len(secret.Data[k]) == 0 {
			return fmt.Errorf("cosmosigner raft TLS secret %q is missing key %q: it must contain tls.crt, tls.key and ca.crt", *name, k)
		}
	}
	return nil
}

// RequireSecretSelector errors when the Secret key referenced by sel is absent or empty, so a signer
// backend that mounts a missing auth Secret (a Vault token/certificate or GCP credentials) is caught at
// preflight rather than crash-looping after deploy and leaving the validator with neither its local key
// nor a working signer. A nil selector (an optional reference left unset) is accepted. Shared by the
// standalone ChainNode and the ChainNodeSet signing paths so their Secret preflight stays consistent.
// `purpose` names the reference in the error (e.g. "Vault token").
func RequireSecretSelector(ctx context.Context, c client.Client, namespace, purpose string, sel *corev1.SecretKeySelector) error {
	if sel == nil {
		return nil
	}
	secret := &corev1.Secret{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: sel.Name}, secret); err != nil {
		if errors.IsNotFound(err) {
			return fmt.Errorf("cosmosigner %s secret %q not found: provide it before deploying the signer", purpose, sel.Name)
		}
		return err
	}
	if len(secret.Data[sel.Key]) == 0 {
		return fmt.Errorf("cosmosigner %s secret %q is missing key %q: provide it before deploying the signer", purpose, sel.Name, sel.Key)
	}
	return nil
}
