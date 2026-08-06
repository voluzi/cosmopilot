package cosmosigner

import (
	"slices"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

// gcpImportParams renders a signer whose GCP KMS key is produced by a controller-managed import.
// KeyVersion is deliberately empty: it does not exist until the import pod runs.
func gcpImportParams() Params {
	p := testParams()
	p.Replicas = 1
	p.Backend = Backend{GCP: &GcpBackend{
		Import: &GcpImport{
			Project:         "example-project",
			Location:        "europe-west1",
			KeyRing:         "validators",
			Key:             "consensus",
			ImportJob:       "consensus-import",
			ProtectionLevel: "hsm",
		},
	}}
	p.ServiceAccountName = "signer-sa"
	p.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "registry-creds"}}
	return p
}

func argValue(args []string, flag string) (string, bool) {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1], true
		}
	}
	return "", false
}

// TestGcpImportPodArgs pins the exact `cosmosigner import` invocation. The GCP destination is
// addressed by FLAGS only (the backend env carries no import coordinates), and --gcp-key-version is
// never passed: the version is what the import produces.
func TestGcpImportPodArgs(t *testing.T) {
	p := gcpImportParams()
	args := p.Backend.importArgs()

	want := []string{
		"import", "--from", importSourceFile,
		"--gcp-project", "example-project",
		"--gcp-location", "europe-west1",
		"--gcp-keyring", "validators",
		"--gcp-key", "consensus",
		"--gcp-protection", "hsm",
		"--gcp-import-job", "consensus-import",
	}
	if !slices.Equal(args, want) {
		t.Fatalf("import args =\n%q\nwant\n%q", args, want)
	}

	// A Vault import is untouched by the GCP arm.
	vault := testParams()
	if got := vault.Backend.importArgs(); !slices.Equal(got, []string{"import", "--from", importSourceFile}) {
		t.Fatalf("vault import args = %q", got)
	}
}

// TestGcpImportPodMountsOnlySource verifies the one-shot import pod exposes the source consensus key
// and nothing else: the referenced Secret may hold unrelated material (an account mnemonic in a
// shared Secret), so only priv_validator_key.json is projected.
func TestGcpImportPodMountsOnlySource(t *testing.T) {
	j := JobRunner{Params: gcpImportParams()}
	pod := j.buildImportPod("val-priv-key")

	if pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		t.Fatalf("import pod restart policy = %q", pod.Spec.RestartPolicy)
	}
	if pod.Spec.SecurityContext == nil || pod.Spec.Containers[0].SecurityContext == nil {
		t.Fatal("import pod must carry the restricted pod and container security contexts")
	}
	if pod.Spec.ServiceAccountName != "signer-sa" {
		t.Fatalf("import pod service account = %q, want the signer's", pod.Spec.ServiceAccountName)
	}
	if len(pod.Spec.ImagePullSecrets) != 1 || pod.Spec.ImagePullSecrets[0].Name != "registry-creds" {
		t.Fatalf("import pod image pull secrets = %v", pod.Spec.ImagePullSecrets)
	}

	if len(pod.Spec.Volumes) != 1 {
		t.Fatalf("import pod volumes = %d, want only the source key: %+v", len(pod.Spec.Volumes), pod.Spec.Volumes)
	}
	src := pod.Spec.Volumes[0]
	if src.Name != importSourceVolume || src.Secret == nil || src.Secret.SecretName != "val-priv-key" {
		t.Fatalf("unexpected source volume %+v", src)
	}
	if len(src.Secret.Items) != 1 || src.Secret.Items[0].Key != "priv_validator_key.json" {
		t.Fatalf("source Secret must project ONLY priv_validator_key.json, got %+v", src.Secret.Items)
	}

	mounts := pod.Spec.Containers[0].VolumeMounts
	if len(mounts) != 1 {
		t.Fatalf("import pod mounts = %+v", mounts)
	}
	if mounts[0].MountPath != importSourceDir || !mounts[0].ReadOnly {
		t.Fatalf("source must be mounted read-only at %q, got %+v", importSourceDir, mounts[0])
	}

	// Credentials are added only when a credentialsSecret is configured (Workload Identity otherwise).
	withCreds := gcpImportParams()
	withCreds.Backend.GCP.CredentialsSecret = &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "gcp-sa"}, Key: "key.json",
	}
	pod = JobRunner{Params: withCreds}.buildImportPod("val-priv-key")
	if len(pod.Spec.Volumes) != 2 {
		t.Fatalf("credentialed import pod volumes = %+v", pod.Spec.Volumes)
	}
	env := pod.Spec.Containers[0].Env
	if _, ok := envValue(env, "COSMOSIGNER_GCP_CREDENTIALS_FILE"); !ok {
		t.Fatal("credentialed import pod must point cosmosigner at the mounted credentials file")
	}
}

// TestGcpImportPodOmitsUnresolvedKeyVersion guards against configuring the import pod with an empty
// COSMOSIGNER_GCP_KEY_VERSION, which would look like a deliberate (invalid) backend selection.
func TestGcpImportPodOmitsUnresolvedKeyVersion(t *testing.T) {
	pod := JobRunner{Params: gcpImportParams()}.buildImportPod("val-priv-key")
	env := pod.Spec.Containers[0].Env
	if v, ok := envValue(env, "COSMOSIGNER_BACKEND"); !ok || v != backendGcpKms {
		t.Fatalf("import pod backend env = %q", v)
	}
	if _, ok := envValue(env, "COSMOSIGNER_GCP_KEY_VERSION"); ok {
		t.Fatal("a managed import has no key version yet; the env var must be omitted, not empty")
	}
}

// TestGcpPubkeyPodPinsKeyVersion verifies the verification probe reads back the EXACT version the
// import produced rather than whatever the backend would otherwise resolve.
func TestGcpPubkeyPodPinsKeyVersion(t *testing.T) {
	const version = "projects/example-project/locations/europe-west1/keyRings/validators/cryptoKeys/consensus/cryptoKeyVersions/3"
	pod := JobRunner{Params: gcpImportParams()}.buildPubkeyPod(version)
	got, ok := argValue(pod.Spec.Containers[0].Args, "--gcp-key-version")
	if !ok || got != version {
		t.Fatalf("pubkey pod --gcp-key-version = %q, want %q", got, version)
	}
	if pod.Spec.Containers[0].Args[0] != "pubkey" {
		t.Fatalf("pubkey pod args = %q", pod.Spec.Containers[0].Args)
	}
	// The probe reads a key version; it must never mount the source consensus key.
	for _, v := range pod.Spec.Volumes {
		if v.Name == importSourceVolume {
			t.Fatal("the pubkey probe must not mount the source consensus key")
		}
	}
}

// TestImagePullSecretsStayOffStatefulSet keeps the signer's lifecycle digest stable: threading pull
// secrets to the one-shot pods must not alter the StatefulSet of an already-running signer, which
// would force every existing signer through a break-before-make migration on upgrade.
func TestImagePullSecretsStayOffStatefulSet(t *testing.T) {
	base := testParams()
	withSecrets := testParams()
	withSecrets.ImagePullSecrets = []corev1.LocalObjectReference{{Name: "registry-creds"}}

	baseDigest, err := base.LifecycleDigest("signing")
	if err != nil {
		t.Fatal(err)
	}
	gotDigest, err := withSecrets.LifecycleDigest("signing")
	if err != nil {
		t.Fatal(err)
	}
	if gotDigest != baseDigest {
		t.Fatal("image pull secrets must not change the signer lifecycle digest")
	}

	yaml, err := withSecrets.ConfigYAML()
	if err != nil {
		t.Fatal(err)
	}
	sts, err := withSecrets.StatefulSet(yaml)
	if err != nil {
		t.Fatal(err)
	}
	if len(sts.Spec.Template.Spec.ImagePullSecrets) != 0 {
		t.Fatalf("signer StatefulSet must not gain image pull secrets: %v", sts.Spec.Template.Spec.ImagePullSecrets)
	}
}

// TestParseImportedKeyVersion is the durability gate's first hop: a successful import pod is only
// meaningful if the EXACT key version it created can be read back out of its logs and provably
// belongs to the destination the spec asked for.
func TestParseImportedKeyVersion(t *testing.T) {
	const key = "projects/example-project/locations/europe-west1/keyRings/validators/cryptoKeys/consensus"
	const version = key + "/cryptoKeyVersions/2"

	out := "imported key version: " + version + "\n" +
		"run cosmosigner with --backend gcpkms --gcp-key-version " + version + "\n" +
		"import accepted, but the key version is still finalizing in KMS — identity not verified yet.\n"
	got, err := ParseImportedKeyVersion(out, key)
	if err != nil {
		t.Fatal(err)
	}
	if got != version {
		t.Fatalf("ParseImportedKeyVersion() = %q, want %q", got, version)
	}

	if _, err := ParseImportedKeyVersion("verified: backend public key matches the source file\n", key); err == nil {
		t.Fatal("output without the key version line must be rejected")
	}
	if _, err := ParseImportedKeyVersion("imported key version: "+key+"\n", key); err == nil {
		t.Fatal("a crypto key name is not a key version and must be rejected")
	}
	if _, err := ParseImportedKeyVersion("imported key version: "+key+"/cryptoKeyVersions/abc\n", key); err == nil {
		t.Fatal("a non-numeric version must be rejected")
	}
	// A version under a DIFFERENT crypto key would retarget the validator silently.
	other := "projects/example-project/locations/europe-west1/keyRings/validators/cryptoKeys/other/cryptoKeyVersions/1"
	if _, err := ParseImportedKeyVersion("imported key version: "+other+"\n", key); err == nil {
		t.Fatal("a key version outside the configured destination must be rejected")
	}
}

// TestGcpImportCryptoKeyName pins the destination resource name used for the containment check above.
func TestGcpImportCryptoKeyName(t *testing.T) {
	g := gcpImportParams().Backend.GCP.Import
	want := "projects/example-project/locations/europe-west1/keyRings/validators/cryptoKeys/consensus"
	if got := g.CryptoKeyName(); got != want {
		t.Fatalf("CryptoKeyName() = %q, want %q", got, want)
	}
	if got := (*GcpImport)(nil).CryptoKeyName(); got != "" {
		t.Fatalf("nil import CryptoKeyName() = %q", got)
	}
}

func envValue(env []corev1.EnvVar, name string) (string, bool) {
	for _, e := range env {
		if e.Name == name {
			return e.Value, true
		}
	}
	return "", false
}
