package framework

import "testing"

func TestWithCosmosignerImage(t *testing.T) {
	cfg := DefaultConfig()
	WithCosmosignerImage("ghcr.io/voluzi/cosmosigner:test")(cfg)

	if cfg.CosmosignerImage != "ghcr.io/voluzi/cosmosigner:test" {
		t.Fatalf("CosmosignerImage = %q", cfg.CosmosignerImage)
	}
}
