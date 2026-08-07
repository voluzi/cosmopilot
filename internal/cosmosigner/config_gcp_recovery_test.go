package cosmosigner

import "testing"

func TestGcpKeyVersionFromConfigYAML(t *testing.T) {
	const version = "projects/p/locations/global/keyRings/r/cryptoKeys/k/cryptoKeyVersions/7"
	config := "backend:\n  type: gcpkms\n  gcp:\n    key_version: " + version + "\n"
	got, err := GcpKeyVersionFromConfigYAML(config)
	if err != nil {
		t.Fatal(err)
	}
	if got != version {
		t.Fatalf("GcpKeyVersionFromConfigYAML() = %q, want %q", got, version)
	}
	got, err = GcpKeyVersionFromConfigYAML("backend:\n  type: vault\n")
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("non-GCP config returned %q", got)
	}
}
