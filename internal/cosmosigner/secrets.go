package cosmosigner

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

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
