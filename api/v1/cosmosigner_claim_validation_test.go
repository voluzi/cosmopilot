package v1

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

// The claim credentials are projected by selector key into the signer, so a selector with an empty
// name or key must be refused at admission instead of looping in reconciliation.
func TestCosmosignerRejectsIncompleteClaimSettings(t *testing.T) {
	selector := func(name, key string) *corev1.SecretKeySelector {
		return &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: key}
	}
	vault := func(mutate func(*CosmosignerVaultBackend)) *Cosmosigner {
		v := &CosmosignerVaultBackend{Address: "https://vault:8200", KeyName: "validator", TokenSecret: selector("vault-token", "token")}
		mutate(v)
		return &Cosmosigner{Backend: CosmosignerBackend{Vault: v}}
	}
	gcp := func(mutate func(*CosmosignerGcpKmsBackend)) *Cosmosigner {
		g := &CosmosignerGcpKmsBackend{KeyVersion: "projects/p/locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1"}
		mutate(g)
		return &Cosmosigner{Backend: CosmosignerBackend{GcpKMS: g}}
	}

	invalid := map[string]*Cosmosigner{
		"vault claim token without name": vault(func(v *CosmosignerVaultBackend) { v.ClaimTokenSecret = selector("", "token") }),
		"vault claim token without key":  vault(func(v *CosmosignerVaultBackend) { v.ClaimTokenSecret = selector("vault-admin", "") }),
		"empty vault binding mount":      vault(func(v *CosmosignerVaultBackend) { v.BindingMount = ptr.To(" ") }),
		"gcp claim credentials without name": gcp(func(g *CosmosignerGcpKmsBackend) {
			g.ClaimCredentialsSecret = selector("", "credentials.json")
		}),
		"gcp claim credentials without key": gcp(func(g *CosmosignerGcpKmsBackend) {
			g.ClaimCredentialsSecret = selector("kms-admin", "")
		}),
	}
	for name, signer := range invalid {
		if err := signer.Validate(".spec.cosmosigner", false); err == nil {
			t.Errorf("%s must be rejected", name)
		}
	}

	valid := map[string]*Cosmosigner{
		"vault claim settings": vault(func(v *CosmosignerVaultBackend) {
			v.ClaimTokenSecret = selector("vault-admin", "token")
			v.BindingMount = ptr.To("claims")
		}),
		"gcp claim credentials": gcp(func(g *CosmosignerGcpKmsBackend) {
			g.ClaimCredentialsSecret = selector("kms-admin", "credentials.json")
		}),
	}
	for name, signer := range valid {
		if err := signer.Validate(".spec.cosmosigner", false); err != nil {
			t.Errorf("%s must be accepted: %v", name, err)
		}
	}
}
