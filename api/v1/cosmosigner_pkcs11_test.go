package v1

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
	set.Spec.Nodes[1].Cosmosigner.Backend.PKCS11.PublicKey = set.Spec.Nodes[0].Cosmosigner.Backend.PKCS11.PublicKey
	require.ErrorContains(t, set.validateUniqueSigningKeys(), "same PKCS#11 signing key")
}

func TestPKCS11InitialPublicKeyCorrectionAdmission(t *testing.T) {
	for _, validator := range []bool{false, true} {
		for _, tc := range []struct {
			name             string
			recorded, module bool
		}{
			{name: "initial"}, {name: "recorded", recorded: true}, {name: "another field", module: true},
		} {
			t.Run(fmt.Sprintf("validator=%t/%s", validator, tc.name), func(t *testing.T) {
				node := &ChainNode{ObjectMeta: metav1.ObjectMeta{Name: "node"}, Spec: ChainNodeSpec{
					App: AppSpec{Image: "img", App: "appd", Version: ptr.To("1.0.0")}, Genesis: &GenesisConfig{Url: ptr.To("https://example.com/genesis.json")}, Cosmosigner: pkcs11Signer(),
				}}
				if validator {
					node.Spec.Validator = &ValidatorConfig{}
				}
				node.SetEstablishedChainID("test-1")
				if tc.recorded {
					node.Status.CosmosignerSigningDigest = "served"
				}
				next := node.DeepCopy()
				next.Spec.Cosmosigner.Backend.PKCS11.PublicKey = "AQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
				if tc.module {
					next.Spec.Cosmosigner.Backend.PKCS11.Module = "/other.so"
				}
				t.Run("ChainNode", func(t *testing.T) {
					_, err := next.Validate(node)
					if tc.recorded || tc.module {
						require.ErrorContains(t, err, "records its applied public key")
						require.ErrorContains(t, err, "documented PKCS#11 manual recovery procedure")
						require.NotContains(t, err.Error(), "wait for one reconcile")
					} else {
						require.NoError(t, err)
					}
				})
				set := &ChainNodeSet{ObjectMeta: metav1.ObjectMeta{Name: "nodes"}, Spec: ChainNodeSetSpec{
					App: node.Spec.App, Genesis: node.Spec.Genesis, Cosmosigner: pkcs11Signer(), Nodes: []NodeGroupSpec{{Name: "sentries", Instances: ptr.To(1)}},
				}}
				if validator {
					set.Spec.Validator = &NodeSetValidatorConfig{}
				} else {
					set.Spec.Cosmosigner.NodeGroups = []string{"sentries"}
				}
				set.SetEstablishedChainID("test-1")
				if tc.recorded {
					set.Status.Cosmosigners[0].SigningDigest = "served"
				}
				updated := set.DeepCopy()
				updated.Spec.Cosmosigner.Backend.PKCS11 = next.Spec.Cosmosigner.Backend.PKCS11.DeepCopy()
				t.Run("ChainNodeSet", func(t *testing.T) {
					_, err := updated.Validate(set)
					if tc.recorded || tc.module {
						require.ErrorContains(t, err, "records its applied public key")
						require.ErrorContains(t, err, "documented PKCS#11 manual recovery procedure")
						require.NotContains(t, err.Error(), "wait for one reconcile")
					} else {
						require.NoError(t, err)
					}
				})
			})
		}
	}
}
