package framework

import "testing"

func TestWithCosmosignerImage(t *testing.T) {
	cfg := DefaultConfig()
	want := "ghcr.io/voluzi/cosmosigner:test"
	WithCosmosignerImage(want)(cfg)

	if cfg.CosmosignerImage != want {
		t.Fatalf("CosmosignerImage = %q, want %q", cfg.CosmosignerImage, want)
	}
}
