package v1

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

func pkcs11Signer() *Cosmosigner {
	return &Cosmosigner{Backend: CosmosignerBackend{PKCS11: &CosmosignerPKCS11Backend{
		Module: "/vendor/module.so", TokenLabel: "validator", KeyLabel: "consensus", KeyID: "aB", PublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		PINSecret: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "pin"}, Key: "pin"},
	}}}
}

func TestPKCS11SigningIdentity(t *testing.T) {
	c := pkcs11Signer()
	require.NoError(t, c.Validate(".spec.cosmosigner", false))
	require.False(t, c.UsesPubkeyPod(false))
	require.False(t, c.ImportsGeneratedKey(true))
	require.False(t, c.RequiresLocalPrivKey())
	original := c.effectiveSigningIdentity("")
	c.Backend.PKCS11.KeyID = "Ab"
	c.Backend.PKCS11.PINSecret.Name = "other-pin"
	require.Equal(t, original, c.effectiveSigningIdentity(""))
	for name, change := range map[string]func(*CosmosignerPKCS11Backend){
		"module":    func(p *CosmosignerPKCS11Backend) { p.Module = "/other.so" },
		"slot zero": func(p *CosmosignerPKCS11Backend) { p.TokenLabel = ""; p.Slot = ptr.To(int64(0)) },
		"key label": func(p *CosmosignerPKCS11Backend) { p.KeyLabel = "other" },
		"key id":    func(p *CosmosignerPKCS11Backend) { p.KeyID = "01" },
	} {
		t.Run(name, func(t *testing.T) {
			c := pkcs11Signer()
			change(c.Backend.PKCS11)
			require.Equal(t, original, c.effectiveSigningIdentity(""))
		})
	}
	c.Backend.PKCS11.PublicKey = "AQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	require.NotEqual(t, original, c.effectiveSigningIdentity(""))
}

func TestPKCS11UniqueSigningCoordinates(t *testing.T) {
	set := &ChainNodeSet{Spec: ChainNodeSetSpec{Nodes: []NodeGroupSpec{
		{Name: "first", Instances: ptr.To(1), Cosmosigner: pkcs11Signer()},
		{Name: "second", Instances: ptr.To(1), Cosmosigner: pkcs11Signer()},
	}}}
	require.ErrorContains(t, set.validateUniqueSigningKeys(), "same PKCS#11 signing key")
	set.Spec.Nodes[1].Cosmosigner.Backend.PKCS11.PublicKey = "AQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	require.ErrorContains(t, set.validateUniqueSigningKeys(), "same PKCS#11 signing key")
	set.Spec.Nodes[1].Cosmosigner.Backend.PKCS11.KeyID = "01"
	require.NoError(t, set.validateUniqueSigningKeys())
}
