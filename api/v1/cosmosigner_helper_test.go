package v1

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// TestCosmosignerGetImagePrecedence verifies the image resolution order: an explicit per-CR
// .spec.cosmosigner.image always wins; otherwise the operator-wide default (wired from the
// -cosmosigner-image/COSMOSIGNER_IMAGE flag) is used; only when that is also empty does the
// hardcoded DefaultCosmosignerImage constant apply.
func TestCosmosignerGetImagePrecedence(t *testing.T) {
	explicit := "explicit/image:v1"
	c := &Cosmosigner{Image: &explicit}
	if got := c.GetImage("operator/default:v2"); got != explicit {
		t.Fatalf("explicit image must win, got %q", got)
	}

	unset := &Cosmosigner{}
	if got := unset.GetImage("operator/default:v2"); got != "operator/default:v2" {
		t.Fatalf("operator default must be used when unset, got %q", got)
	}

	if got := unset.GetImage(""); got != DefaultCosmosignerImage {
		t.Fatalf("hardcoded default must be used when nothing else is configured, got %q", got)
	}
}

func TestVaultVersionOneMatchesTmKMSIdentity(t *testing.T) {
	token := &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "vault-token"}, Key: "token"}
	tmkms := &ChainNode{
		Spec: ChainNodeSpec{Validator: &ValidatorConfig{TmKMS: &TmKMS{Provider: TmKmsProvider{
			Hashicorp: &TmKmsHashicorpProvider{Address: "https://vault:8200", Key: "validator", TokenSecret: token},
		}}}},
	}
	versionOne := 1
	managed := &ChainNode{Spec: ChainNodeSpec{Cosmosigner: &Cosmosigner{Backend: CosmosignerBackend{
		Vault: &CosmosignerVaultBackend{
			Address: "https://vault:8200", KeyName: "validator", KeyVersion: &versionOne, TokenSecret: token,
		},
	}}}}

	if got, want := managed.EffectiveSigningIdentity(), tmkms.EffectiveSigningIdentity(); got != want {
		t.Fatalf("Vault version 1 must preserve the tmKMS signing identity: got %q want %q", got, want)
	}

	versionTwo := 2
	managed.Spec.Cosmosigner.Backend.Vault.KeyVersion = &versionTwo
	if managed.EffectiveSigningIdentity() == tmkms.EffectiveSigningIdentity() {
		t.Fatal("a different pinned Vault version must remain a distinct managed signing identity")
	}
}
