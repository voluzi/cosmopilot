package controllers

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/voluzi/cosmopilot/v4/pkg/images"
)

func TestImageGetters(t *testing.T) {
	tests := []struct {
		name       string
		defaultImg string
		getter     func(*ControllerRunOptions) string
		configured func(string) *ControllerRunOptions
	}{
		{name: "node-utils", defaultImg: images.DefaultNodeUtilsImage, getter: (*ControllerRunOptions).GetNodeUtilsImage, configured: func(image string) *ControllerRunOptions { return &ControllerRunOptions{NodeUtilsImage: image} }},
		{name: "cosmoseed", defaultImg: images.DefaultCosmoseedImage, getter: (*ControllerRunOptions).GetCosmoseedImage, configured: func(image string) *ControllerRunOptions { return &ControllerRunOptions{CosmoseedImage: image} }},
		{name: "cosmoguard", defaultImg: images.DefaultCosmoGuardImage, getter: (*ControllerRunOptions).GetCosmoGuardImage, configured: func(image string) *ControllerRunOptions { return &ControllerRunOptions{CosmoGuardImage: image} }},
		{name: "cosmosigner", defaultImg: images.DefaultCosmosignerImage, getter: (*ControllerRunOptions).GetCosmosignerImage, configured: func(image string) *ControllerRunOptions { return &ControllerRunOptions{CosmosignerImage: image} }},
		{name: "data-exporter", defaultImg: images.DefaultDataExporterImage, getter: (*ControllerRunOptions).GetDataExporterImage, configured: func(image string) *ControllerRunOptions { return &ControllerRunOptions{DataExporterImage: image} }},
		{name: "utility", defaultImg: images.DefaultUtilityImage, getter: (*ControllerRunOptions).GetUtilityImage, configured: func(image string) *ControllerRunOptions { return &ControllerRunOptions{UtilityImage: image} }},
		{name: "tmkms", defaultImg: images.DefaultTmKmsImage, getter: (*ControllerRunOptions).GetTmKmsImage, configured: func(image string) *ControllerRunOptions { return &ControllerRunOptions{TmKmsImage: image} }},
		{name: "vault-token-renewer", defaultImg: images.DefaultVaultTokenRenewerImage, getter: (*ControllerRunOptions).GetVaultTokenRenewerImage, configured: func(image string) *ControllerRunOptions { return &ControllerRunOptions{VaultTokenRenewerImage: image} }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var nilOpts *ControllerRunOptions
			assert.Equal(t, tt.defaultImg, tt.getter(nilOpts))
			assert.Equal(t, tt.defaultImg, tt.getter(&ControllerRunOptions{}))
			assert.Equal(t, "registry.example.com:5000/image:custom", tt.getter(tt.configured("registry.example.com:5000/image:custom")))
			assert.Equal(t, "registry.example.com/image@sha256:abcdef", tt.getter(tt.configured("registry.example.com/image@sha256:abcdef")))
		})
	}
}

func TestWaitForRootProtectionBlocksUntilMigrationCompletes(t *testing.T) {
	ready := make(chan struct{})
	opts := &ControllerRunOptions{RootProtectionReady: ready}
	done := make(chan error, 1)
	go func() { done <- opts.WaitForRootProtection(context.Background()) }()

	select {
	case err := <-done:
		t.Fatalf("controller gate opened before migration completed: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	close(ready)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestMatchesWorkerUsesEmptyNameForUnpartitionedRoots(t *testing.T) {
	assert.True(t, MatchesWorker(nil, ""))
	assert.True(t, MatchesWorker(map[string]string{LabelWorkerName: "worker-a"}, "worker-a"))
	assert.False(t, MatchesWorker(map[string]string{LabelWorkerName: "worker-b"}, "worker-a"))
	assert.False(t, MatchesWorker(map[string]string{LabelWorkerName: "worker-a"}, ""))
}
