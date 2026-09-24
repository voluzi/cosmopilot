package main

import (
	"flag"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/voluzi/cosmopilot/v4/pkg/images"
)

type managerImageFlagTest struct {
	name       string
	env        string
	want       string
	configured func() string
}

func managerImageFlagTests() []managerImageFlagTest {
	return []managerImageFlagTest{
		{name: "nodeutils-image", env: "NODE_UTILS_IMAGE", want: images.DefaultNodeUtilsImage, configured: func() string { return runOpts.NodeUtilsImage }},
		{name: "cosmoseed-image", env: "COSMOSEED_IMAGE", want: images.DefaultCosmoseedImage, configured: func() string { return runOpts.CosmoseedImage }},
		{name: "cosmoguard-image", env: "COSMOGUARD_IMAGE", want: images.DefaultCosmoGuardImage, configured: func() string { return runOpts.CosmoGuardImage }},
		{name: "cosmosigner-image", env: "COSMOSIGNER_IMAGE", want: images.DefaultCosmosignerImage, configured: func() string { return runOpts.CosmosignerImage }},
		{name: "dataexporter-image", env: "DATA_EXPORTER_IMAGE", want: images.DefaultDataExporterImage, configured: func() string { return runOpts.DataExporterImage }},
		{name: "utility-image", env: "UTILITY_IMAGE", want: images.DefaultUtilityImage, configured: func() string { return runOpts.UtilityImage }},
		{name: "tmkms-image", env: "TMKMS_IMAGE", want: images.DefaultTmKmsImage, configured: func() string { return runOpts.TmKmsImage }},
		{name: "vault-token-renewer-image", env: "VAULT_TOKEN_RENEWER_IMAGE", want: images.DefaultVaultTokenRenewerImage, configured: func() string { return runOpts.VaultTokenRenewerImage }},
	}
}

func TestManagerImageFlagDefaultsAndEnvironment(t *testing.T) {
	if selectedEnv := os.Getenv("TEST_MANAGER_IMAGE_ENV"); selectedEnv != "" {
		for _, tt := range managerImageFlagTests() {
			want := tt.want
			if tt.env == selectedEnv {
				want = "registry.example.com/image@sha256:abcdef"
			}
			f := flag.Lookup(tt.name)
			require.NotNil(t, f)
			assert.Equal(t, want, f.DefValue)
			assert.Equal(t, want, tt.configured())
		}
		return
	}

	tests := append([]managerImageFlagTest{{name: "defaults"}}, managerImageFlagTests()...)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestManagerImageFlagDefaultsAndEnvironment$")
			for _, variable := range os.Environ() {
				keep := true
				for _, imageFlag := range managerImageFlagTests() {
					if strings.HasPrefix(variable, imageFlag.env+"=") {
						keep = false
						break
					}
				}
				if keep {
					cmd.Env = append(cmd.Env, variable)
				}
			}
			selectedEnv := tt.env
			if selectedEnv == "" {
				selectedEnv = "DEFAULTS"
			} else {
				cmd.Env = append(cmd.Env, selectedEnv+"=registry.example.com/image@sha256:abcdef")
			}
			cmd.Env = append(cmd.Env, "TEST_MANAGER_IMAGE_ENV="+selectedEnv)
			output, err := cmd.CombinedOutput()
			require.NoErrorf(t, err, "%s", output)
		})
	}
}

func TestManagerImageFlagOverrides(t *testing.T) {
	tests := managerImageFlagTests()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := flag.Lookup(tt.name)
			require.NotNil(t, f)

			original := tt.configured()
			t.Cleanup(func() { require.NoError(t, f.Value.Set(original)) })
			require.NoError(t, f.Value.Set("registry.example.com/image@sha256:abcdef"))
			assert.Equal(t, "registry.example.com/image@sha256:abcdef", tt.configured())
		})
	}
}

func TestValidateDisruptionMaxUnavailable(t *testing.T) {
	for _, value := range []int{-1, 0} {
		require.Error(t, validateDisruptionMaxUnavailable(value))
	}
	require.NoError(t, validateDisruptionMaxUnavailable(1))
	require.NoError(t, validateDisruptionMaxUnavailable(3))
}
