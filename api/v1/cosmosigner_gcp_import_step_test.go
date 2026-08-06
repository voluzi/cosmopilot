package v1

import (
	"testing"

	"k8s.io/utils/ptr"
)

// gcpStepKey is the destination CryptoKey the fixtures below import into.
const gcpStepKey = "projects/example-project/locations/europe-west1/keyRings/validators/cryptoKeys/consensus"

func gcpStepBackend() *CosmosignerGcpKmsBackend {
	return &CosmosignerGcpKmsBackend{Import: &CosmosignerGcpKmsImport{
		Project:  "example-project",
		Location: ptr.To("europe-west1"),
		KeyRing:  "validators",
		Key:      "consensus",
	}}
}

// TestGcpImportOwnsKeyVersion pins the containment check the whole durability gate rests on: a
// recorded key version is only usable when it provably lives under the destination the CURRENT spec
// names. Anything else would configure the validator's signer against a foreign identity.
func TestGcpImportOwnsKeyVersion(t *testing.T) {
	i := gcpStepBackend().Import
	if got := i.CryptoKeyName(); got != gcpStepKey {
		t.Fatalf("CryptoKeyName() = %q, want %q", got, gcpStepKey)
	}
	if !i.OwnsKeyVersion(gcpStepKey + "/cryptoKeyVersions/7") {
		t.Fatal("a numbered version of the destination key must be owned")
	}
	for _, bad := range []string{
		"",
		gcpStepKey,
		gcpStepKey + "/cryptoKeyVersions/",
		gcpStepKey + "/cryptoKeyVersions/latest",
		"projects/example-project/locations/europe-west1/keyRings/validators/cryptoKeys/other/cryptoKeyVersions/1",
		"projects/other/locations/europe-west1/keyRings/validators/cryptoKeys/consensus/cryptoKeyVersions/1",
	} {
		if i.OwnsKeyVersion(bad) {
			t.Fatalf("key version %q must not be accepted as a version of the destination key", bad)
		}
	}
	if (*CosmosignerGcpKmsImport)(nil).OwnsKeyVersion(gcpStepKey + "/cryptoKeyVersions/1") {
		t.Fatal("a nil import owns no key version")
	}
}

// TestGcpImportResolvedKeyVersion is what keeps a fabricated `/cryptoKeyVersions/1` out of the
// signer: until the import reports a version under the configured destination, the managed backend
// has NO key version at all, and a version left over from a previous destination is not one either.
func TestGcpImportResolvedKeyVersion(t *testing.T) {
	b := gcpStepBackend()
	if got := b.ResolvedKeyVersion(""); got != "" {
		t.Fatalf("unresolved import must have no key version, got %q", got)
	}
	stale := "projects/example-project/locations/europe-west1/keyRings/validators/cryptoKeys/old/cryptoKeyVersions/1"
	if got := b.ResolvedKeyVersion(stale); got != "" {
		t.Fatalf("a version of a previous destination must not be served, got %q", got)
	}
	version := gcpStepKey + "/cryptoKeyVersions/3"
	if got := b.ResolvedKeyVersion(version); got != version {
		t.Fatalf("ResolvedKeyVersion(%q) = %q", version, got)
	}

	// A pre-provisioned signer keeps its explicitly configured version and ignores status entirely.
	pre := &CosmosignerGcpKmsBackend{KeyVersion: gcpStepKey + "/cryptoKeyVersions/1"}
	if got := pre.ResolvedKeyVersion(version); got != pre.KeyVersion {
		t.Fatalf("pre-provisioned key version = %q, want %q", got, pre.KeyVersion)
	}
}

// TestNextGcpImportStep is the safety matrix of the managed import. The property that matters most:
// a recorded key version alone NEVER means the import is done — the import pod exits zero while
// Cloud KMS is still finalizing the version, so completion additionally requires a verified record.
func TestNextGcpImportStep(t *testing.T) {
	b := gcpStepBackend()
	const source = "validator-key"
	material := []byte("priv-validator-key-bytes")
	other := []byte("a-different-consensus-key")
	version := gcpStepKey + "/cryptoKeyVersions/2"
	foreign := "projects/example-project/locations/europe-west1/keyRings/validators/cryptoKeys/old/cryptoKeyVersions/9"
	done := b.ImportFingerprint(source, material)

	for _, tc := range []struct {
		name       string
		record     string
		keyVersion string
		material   []byte
		want       GcpImportStep
	}{
		{
			name: "first reconcile imports", want: GcpImportRun, material: material,
		},
		{
			// The import pod succeeded and the version was persisted, but nothing has read the public
			// key back yet. Treating this as complete is exactly the PENDING_IMPORT hazard.
			name: "recorded version alone is not completion", keyVersion: version, material: material,
			want: GcpImportVerify,
		},
		{
			name: "verified import is complete", record: done, keyVersion: version, material: material,
			want: GcpImportComplete,
		},
		{
			// Status lost the version (or it belongs to a previous destination): re-importing the SAME
			// bytes creates another version of the same identity, so it is safe and self-healing.
			name:   "verified record without a usable version re-imports the same key",
			record: done, keyVersion: foreign, material: material, want: GcpImportRun,
		},
		{
			// Same destination and source secret, different key bytes. Cloud KMS would happily add a
			// version holding another identity; adopting it would retarget the validator.
			name: "changed source material is terminal", record: done, keyVersion: version, material: other,
			want: GcpImportMismatch,
		},
		{
			// The source is gone but the destination+source half of the record still matches, so the
			// backend provably already holds the registered key: a deleted bootstrap Secret must not
			// re-open the import (which would quiesce a healthy signer).
			name: "completed import survives source deletion", record: done, keyVersion: version,
			want: GcpImportComplete,
		},
		{
			// Source gone, record matches, but the usable key version was lost from status. There is no
			// way to configure the signer, so this cannot be treated as complete.
			name: "source deleted and usable version lost", record: done, keyVersion: "",
			want: GcpImportSourceMissing,
		},
		{
			// The version exists but was never verified, and the source needed to verify it is gone.
			// The backup must be retained through the verification gate, so this is not completion.
			name: "source deleted before verification", keyVersion: version, want: GcpImportSourceMissing,
		},
		{
			name: "no source and no record", want: GcpImportSourceMissing,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := b.NextGcpImportStep(tc.record, tc.keyVersion, source, tc.material); got != tc.want {
				t.Fatalf("NextGcpImportStep() = %q, want %q", got, tc.want)
			}
		})
	}

	// A record written for a DIFFERENT source secret proves nothing about this one.
	if got := b.NextGcpImportStep(b.ImportFingerprint("other-key", material), version, source, material); got != GcpImportVerify {
		t.Fatalf("record from another source secret = %q, want %q", got, GcpImportVerify)
	}
	// A Vault import record can never satisfy a GCP destination.
	vault := &CosmosignerVaultBackend{Address: "https://vault:8200", KeyName: "consensus", UploadGenerated: true}
	if got := b.NextGcpImportStep(vault.ImportFingerprint(source, material), "", source, material); got != GcpImportRun {
		t.Fatalf("Vault record against a GCP destination = %q, want %q", got, GcpImportRun)
	}
}

// TestGcpImportUsesPubkeyPod records that a managed import reserves BOTH one-shot pod names: it runs
// `cosmosigner import` and then reads the imported version back with `cosmosigner pubkey`. Vault's
// uploadGenerated resolves its public key from the source Secret and needs no pubkey pod.
func TestGcpImportUsesPubkeyPod(t *testing.T) {
	gcp := gcpImportSigner()
	if !gcp.ImportsGeneratedKey(false) || !gcp.UsesPubkeyPod(false) {
		t.Fatal("a managed GCP import runs both the import and the pubkey pod")
	}

	preProvisioned := &Cosmosigner{Backend: CosmosignerBackend{GcpKMS: &CosmosignerGcpKmsBackend{
		KeyVersion: gcpStepKey + "/cryptoKeyVersions/1",
	}}}
	if preProvisioned.ImportsGeneratedKey(false) || !preProvisioned.UsesPubkeyPod(false) {
		t.Fatal("a pre-provisioned GCP signer only reads its public key back")
	}

	upload := &Cosmosigner{Backend: CosmosignerBackend{Vault: &CosmosignerVaultBackend{
		Address: "https://vault:8200", KeyName: "consensus", UploadGenerated: true,
	}}}
	if !upload.ImportsGeneratedKey(false) || upload.UsesPubkeyPod(false) {
		t.Fatal("Vault uploadGenerated imports but resolves its public key from the source Secret")
	}

	software := &Cosmosigner{Backend: CosmosignerBackend{Software: &CosmosignerSoftwareBackend{}}}
	if software.ImportsGeneratedKey(true) || software.UsesPubkeyPod(true) {
		t.Fatal("a software signer runs no one-shot pods")
	}
}
