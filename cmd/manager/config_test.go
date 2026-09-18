package main

import (
	"flag"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/voluzi/cosmopilot/v3/internal/k8s"
)

func TestManagerImageFlagDefaults(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{name: "nodeutils-image", want: "ghcr.io/voluzi/node-utils:2.10.0"},
		{name: "cosmoseed-image", want: "ghcr.io/voluzi/cosmoseed:0.11.0"},
		{name: "utility-image", want: k8s.DefaultUtilityImage},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := flag.Lookup(tt.name)
			require.NotNil(t, f)
			assert.Equal(t, tt.want, f.DefValue)
		})
	}
}

func TestUtilityImageFlagUpdatesControllerOptions(t *testing.T) {
	f := flag.Lookup("utility-image")
	require.NotNil(t, f)
	original := runOpts.UtilityImage
	t.Cleanup(func() {
		runOpts.UtilityImage = original
		_ = f.Value.Set(original)
	})

	require.NoError(t, f.Value.Set("registry.example.com/tools:custom"))
	assert.Equal(t, "registry.example.com/tools:custom", runOpts.UtilityImage)
}
