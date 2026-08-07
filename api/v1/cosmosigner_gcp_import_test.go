package v1

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

func gcpImportSigner(mutate ...func(*CosmosignerGcpKmsBackend)) *Cosmosigner {
	backend := &CosmosignerGcpKmsBackend{
		Import: &CosmosignerGcpKmsImport{
			Project: "example-project",
			KeyRing: "validators",
			Key:     "consensus",
		},
	}
	for _, m := range mutate {
		m(backend)
	}
	return &Cosmosigner{Backend: CosmosignerBackend{GcpKMS: backend}}
}

// TestGcpImportCoordinateDefaults pins the defaults the Cosmosigner CLI itself applies, so the
// rendered import args always name the destination explicitly instead of relying on the binary's
// own flag defaults drifting between releases.
func TestGcpImportCoordinateDefaults(t *testing.T) {
	unset := &CosmosignerGcpKmsImport{Project: "p", KeyRing: "r", Key: "consensus"}
	if got := unset.GetLocation(); got != DefaultCosmosignerGcpLocation {
		t.Fatalf("location must default to %q, got %q", DefaultCosmosignerGcpLocation, got)
	}
	if got := unset.GetImportJob(); got != "consensus-import" {
		t.Fatalf("importJob must default to <key>-import, got %q", got)
	}
	if got := unset.GetProtectionLevel(); got != CosmosignerGcpProtectionSoftware {
		t.Fatalf("protectionLevel must default to %q, got %q", CosmosignerGcpProtectionSoftware, got)
	}

	explicit := &CosmosignerGcpKmsImport{
		Project:         "p",
		Location:        ptr.To("europe-west1"),
		KeyRing:         "r",
		Key:             "consensus",
		ImportJob:       ptr.To("rotation-job"),
		ProtectionLevel: ptr.To(CosmosignerGcpProtectionHSM),
	}
	if got := explicit.GetLocation(); got != "europe-west1" {
		t.Fatalf("explicit location must win, got %q", got)
	}
	if got := explicit.GetImportJob(); got != "rotation-job" {
		t.Fatalf("explicit importJob must win, got %q", got)
	}
	if got := explicit.GetProtectionLevel(); got != CosmosignerGcpProtectionHSM {
		t.Fatalf("explicit protectionLevel must win, got %q", got)
	}
}

// TestGcpImportsKeyPredicate keeps the managed import strictly opt-in: only a GCP backend that
// actually carries an import block runs the one-shot import pod. Unlike Vault's uploadGenerated,
// it is never auto-enabled for a genesis-initializing target, because the destination coordinates
// and protection level are operator decisions that cannot be inferred.
func TestGcpImportsKeyPredicate(t *testing.T) {
	if !gcpImportSigner().GcpImportsKey() {
		t.Fatal("a gcpKms backend with an import block must report a managed import")
	}

	preProvisioned := &Cosmosigner{Backend: CosmosignerBackend{GcpKMS: &CosmosignerGcpKmsBackend{
		KeyVersion: "projects/p/locations/global/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1",
	}}}
	if preProvisioned.GcpImportsKey() {
		t.Fatal("a pre-provisioned gcpKms keyVersion must never report a managed import")
	}

	software := &Cosmosigner{Backend: CosmosignerBackend{Software: &CosmosignerSoftwareBackend{}}}
	if software.GcpImportsKey() {
		t.Fatal("a non-GCP backend must never report a GCP managed import")
	}
	if (*Cosmosigner)(nil).GcpImportsKey() {
		t.Fatal("a nil cosmosigner must never report a GCP managed import")
	}
}

// TestValidateGcpImportExclusiveWithKeyVersion pins the API shape decided for VLZ-797: the managed
// import creates the CryptoKeyVersion, so it cannot be declared alongside a pre-provisioned one,
// and one of the two must always be present.
func TestValidateGcpImportExclusiveWithKeyVersion(t *testing.T) {
	neither := &Cosmosigner{Backend: CosmosignerBackend{GcpKMS: &CosmosignerGcpKmsBackend{}}}
	if err := neither.Validate(".spec.cosmosigner", false); err == nil {
		t.Fatal("a gcpKms backend with neither keyVersion nor import must be rejected")
	}

	both := gcpImportSigner(func(b *CosmosignerGcpKmsBackend) {
		b.KeyVersion = "projects/p/locations/global/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1"
	})
	err := both.Validate(".spec.cosmosigner", false)
	if err == nil {
		t.Fatal("a gcpKms backend with both keyVersion and import must be rejected")
	}
	if !strings.Contains(err.Error(), "gcpKms") {
		t.Fatalf("error must name the offending field, got %v", err)
	}

	if err := gcpImportSigner().Validate(".spec.cosmosigner", false); err != nil {
		t.Fatalf("a managed import alone must be accepted: %v", err)
	}
	preProvisioned := &Cosmosigner{Backend: CosmosignerBackend{GcpKMS: &CosmosignerGcpKmsBackend{
		KeyVersion: "projects/p/locations/global/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1",
	}}}
	if err := preProvisioned.Validate(".spec.cosmosigner", false); err != nil {
		t.Fatalf("the pre-provisioned form must keep working unchanged: %v", err)
	}
}

// TestValidateGcpImportRequiredFields mirrors the destination coordinates the Cosmosigner CLI
// requires (--gcp-project/--gcp-keyring/--gcp-key) plus the protection levels it accepts, so the
// no-webhook reconcile path rejects a spec that could only fail inside the one-shot pod.
func TestValidateGcpImportRequiredFields(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*CosmosignerGcpKmsImport)
		want   string
	}{
		{"missing project", func(i *CosmosignerGcpKmsImport) { i.Project = "" }, "project"},
		{"missing keyRing", func(i *CosmosignerGcpKmsImport) { i.KeyRing = "" }, "keyRing"},
		{"missing key", func(i *CosmosignerGcpKmsImport) { i.Key = "" }, "key"},
		{"empty location", func(i *CosmosignerGcpKmsImport) { i.Location = ptr.To("") }, "location"},
		{"empty importJob", func(i *CosmosignerGcpKmsImport) { i.ImportJob = ptr.To("") }, "importJob"},
		{"bad protectionLevel", func(i *CosmosignerGcpKmsImport) { i.ProtectionLevel = ptr.To("tpm") }, "protectionLevel"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := gcpImportSigner(func(b *CosmosignerGcpKmsBackend) { tc.mutate(b.Import) })
			err := c.Validate(".spec.cosmosigner", false)
			if err == nil {
				t.Fatalf("%s must be rejected", tc.name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error must name %q, got %v", tc.want, err)
			}
		})
	}

	for _, level := range []string{CosmosignerGcpProtectionSoftware, CosmosignerGcpProtectionHSM} {
		c := gcpImportSigner(func(b *CosmosignerGcpKmsBackend) { b.Import.ProtectionLevel = ptr.To(level) })
		if err := c.Validate(".spec.cosmosigner", false); err != nil {
			t.Fatalf("protectionLevel %q must be accepted: %v", level, err)
		}
	}

	// A credentialsSecret is optional (Workload Identity / ADC) but must be complete when set: the
	// controller projects the selector key to a fixed filename.
	partial := gcpImportSigner(func(b *CosmosignerGcpKmsBackend) {
		b.CredentialsSecret = &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "gcp-creds"}}
	})
	if err := partial.Validate(".spec.cosmosigner", false); err == nil {
		t.Fatal("a credentialsSecret without a key must be rejected")
	}
}

// TestGcpImportFingerprintIsBackendNamespaced keeps the persisted import record unambiguous across
// backends: a Vault record must never be mistaken for proof that the GCP destination already holds
// the key, and vice versa. The two-part form (target + material) is preserved so the absent-source
// fast path can match on the target half alone.
func TestGcpImportFingerprintIsBackendNamespaced(t *testing.T) {
	material := []byte(`{"priv_key":{"value":"abc"}}`)
	gcp := gcpImportSigner().Backend.GcpKMS
	vault := &CosmosignerVaultBackend{Address: "https://vault:8200", KeyName: "validator"}

	if gcp.ImportTargetFingerprint("val-priv-key") == vault.ImportTargetFingerprint("val-priv-key") {
		t.Fatal("GCP and Vault import targets must never share a fingerprint")
	}
	if gcp.ImportRecordMatchesTarget(vault.ImportFingerprint("val-priv-key", material), "val-priv-key") {
		t.Fatal("a Vault import record must never satisfy a GCP import target")
	}

	record := gcp.ImportFingerprint("val-priv-key", material)
	if !gcp.ImportRecordMatchesTarget(record, "val-priv-key") {
		t.Fatal("a GCP import record must match its own target half")
	}
	if !gcp.ImportRecordMatches(record, "val-priv-key", material) {
		t.Fatal("a GCP import record must match its own target and material")
	}
	if gcp.ImportRecordMatches(record, "val-priv-key", []byte("other")) {
		t.Fatal("changed key material must invalidate the import record")
	}
	if gcp.ImportRecordMatchesTarget(record, "other-secret") {
		t.Fatal("a changed source secret must invalidate the import target")
	}

	// Every addressing coordinate takes part in the target half: importing into a different
	// project/location/key ring/key is a different destination and must re-import rather than
	// silently accept the previous record.
	for _, tc := range []struct {
		name   string
		mutate func(*CosmosignerGcpKmsImport)
	}{
		{"project", func(i *CosmosignerGcpKmsImport) { i.Project = "other" }},
		{"location", func(i *CosmosignerGcpKmsImport) { i.Location = ptr.To("europe-west1") }},
		{"keyRing", func(i *CosmosignerGcpKmsImport) { i.KeyRing = "other" }},
		{"key", func(i *CosmosignerGcpKmsImport) { i.Key = "other" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := gcpImportSigner(func(b *CosmosignerGcpKmsBackend) { tc.mutate(b.Import) }).Backend.GcpKMS
			if changed.ImportRecordMatchesTarget(record, "val-priv-key") {
				t.Fatalf("a changed %s must invalidate the import target", tc.name)
			}
		})
	}

	// The import job is a rotatable resource (Cloud KMS import jobs expire) and the protection level
	// only applies when the CryptoKey is first created, so neither may force a second import of an
	// already-imported consensus key.
	for _, tc := range []struct {
		name   string
		mutate func(*CosmosignerGcpKmsImport)
	}{
		{"importJob", func(i *CosmosignerGcpKmsImport) { i.ImportJob = ptr.To("replacement-job") }},
		{"protectionLevel", func(i *CosmosignerGcpKmsImport) { i.ProtectionLevel = ptr.To(CosmosignerGcpProtectionHSM) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := gcpImportSigner(func(b *CosmosignerGcpKmsBackend) { tc.mutate(b.Import) }).Backend.GcpKMS
			if !changed.ImportRecordMatches(record, "val-priv-key", material) {
				t.Fatalf("a changed %s must not invalidate a completed import", tc.name)
			}
		})
	}
}

func TestGcpImportFingerprintIgnoresEquivalentJSONSerialization(t *testing.T) {
	gcp := gcpImportSigner().Backend.GcpKMS
	compact := []byte(`{"address":"AA","pub_key":{"type":"tendermint/PubKeyEd25519","value":"pub"},"priv_key":{"type":"tendermint/PrivKeyEd25519","value":"priv"}}`)
	reformatted := []byte(`{
  "priv_key": {"value": "priv", "type": "tendermint/PrivKeyEd25519"},
  "pub_key": {"value": "pub", "type": "tendermint/PubKeyEd25519"},
  "address": "AA"
}`)

	record := gcp.ImportFingerprint("val-priv-key", compact)
	if record != gcp.ImportFingerprint("val-priv-key", reformatted) {
		t.Fatal("equivalent private-validator key JSON must have the same import fingerprint")
	}
	if !gcp.ImportRecordMatches(record, "val-priv-key", reformatted) {
		t.Fatal("reserializing the same consensus key must not be treated as a key rotation")
	}
}

// TestGcpImportSigningIdentityStability protects two invariants at once: existing pre-provisioned
// GCP signers keep their exact identity string (and therefore their persisted digests and
// lifecycle fingerprints), and a managed import's identity is derived from the destination
// coordinates so it does not change when the controller finally resolves the key version.
func TestGcpImportSigningIdentityStability(t *testing.T) {
	keyVersion := "projects/p/locations/global/keyRings/r/cryptoKeys/k/cryptoKeyVersions/3"
	preProvisioned := &Cosmosigner{Backend: CosmosignerBackend{GcpKMS: &CosmosignerGcpKmsBackend{KeyVersion: keyVersion}}}
	if got := preProvisioned.effectiveSigningIdentity(""); got != "gcpkms\x00"+keyVersion {
		t.Fatalf("pre-provisioned GCP identity must stay byte-identical, got %q", got)
	}

	managed := gcpImportSigner()
	identity := managed.effectiveSigningIdentity("")
	if identity == "" {
		t.Fatal("a managed GCP import must have a signing identity")
	}
	if identity == preProvisioned.effectiveSigningIdentity("") {
		t.Fatal("a managed import must not collide with an unrelated pre-provisioned key version")
	}
	// Neither the rotatable import job nor the protection level changes which key is signed with.
	rotated := gcpImportSigner(func(b *CosmosignerGcpKmsBackend) {
		b.Import.ImportJob = ptr.To("replacement-job")
		b.Import.ProtectionLevel = ptr.To(CosmosignerGcpProtectionHSM)
	})
	if got := rotated.effectiveSigningIdentity(""); got != identity {
		t.Fatalf("importJob/protectionLevel must not change the signing identity, got %q want %q", got, identity)
	}
	elsewhere := gcpImportSigner(func(b *CosmosignerGcpKmsBackend) { b.Import.KeyRing = "other" })
	if got := elsewhere.effectiveSigningIdentity(""); got == identity {
		t.Fatal("a different destination key ring must be a different signing identity")
	}
}
