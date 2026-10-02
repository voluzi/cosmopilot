package v1

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/voluzi/cosmopilot/v5/pkg/images"
)

// TestCosmosignerGetImagePrecedence verifies the image resolution order: an explicit per-CR
// .spec.cosmosigner.image always wins; otherwise the operator-wide default (wired from the
// -cosmosigner-image/COSMOSIGNER_IMAGE flag) is used; only when that is also empty does the
// built-in default apply.
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
	empty := ""
	if got := (&Cosmosigner{Image: &empty}).GetImage("operator/default:v2"); got != "operator/default:v2" {
		t.Fatalf("operator default must be used when the resource image is empty, got %q", got)
	}

	if got := unset.GetImage(""); got != images.DefaultCosmosignerImage {
		t.Fatalf("hardcoded default must be used when nothing else is configured, got %q", got)
	}
}

// TestVaultUploadsGeneratedAutoDefaultsForInitTargets pins the auto-default: a genesis-initializing
// validator always generates its consensus key locally, so the Vault backend must import it even
// when uploadGenerated is left unset. Without an init target the field is the only thing that
// enables the import, and a non-Vault backend never imports at all.
func TestVaultUploadsGeneratedAutoDefaultsForInitTargets(t *testing.T) {
	vault := func(uploadGenerated bool) *Cosmosigner {
		return &Cosmosigner{Backend: CosmosignerBackend{Vault: &CosmosignerVaultBackend{
			Address: "https://vault:8200", KeyName: "validator",
			TokenSecret: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "vault-token"}, Key: "token",
			},
			UploadGenerated: uploadGenerated,
		}}}
	}

	if !vault(false).VaultUploadsGenerated(true) {
		t.Fatal("a genesis-initializing target must import its generated key even with uploadGenerated unset")
	}
	if vault(false).VaultUploadsGenerated(false) {
		t.Fatal("without an init target, uploadGenerated=false must not import a generated key")
	}
	if !vault(true).VaultUploadsGenerated(false) {
		t.Fatal("an explicit uploadGenerated=true must import a generated key")
	}

	software := &Cosmosigner{Backend: CosmosignerBackend{Software: &CosmosignerSoftwareBackend{}}}
	if software.VaultUploadsGenerated(true) {
		t.Fatal("a non-Vault backend must never report a Vault import")
	}
}

func TestVaultPinnedKeyVersionsHaveDistinctSigningIdentities(t *testing.T) {
	versionOne, versionTwo := 1, 2
	node := &ChainNode{Spec: ChainNodeSpec{Cosmosigner: &Cosmosigner{Backend: CosmosignerBackend{
		Vault: &CosmosignerVaultBackend{Address: "https://vault:8200", KeyName: "validator", KeyVersion: &versionOne},
	}}}}
	firstIdentity := node.EffectiveSigningIdentity()
	node.Spec.Cosmosigner.Backend.Vault.KeyVersion = &versionTwo
	if firstIdentity == "" || node.EffectiveSigningIdentity() == firstIdentity {
		t.Fatal("different pinned Vault key versions must have distinct signing identities")
	}
}
