package nodeutils

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewRejectsUntrustedShutdownCredentialBeforeInitialization(t *testing.T) {
	tests := []struct {
		name string
		hash string
	}{
		{name: "missing expected hash"},
		{name: "malformed expected hash", hash: "not-a-digest"},
		{name: "mismatched expected hash", hash: ShutdownTokenHash(wrongTestShutdownToken)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New("chaind",
				WithShutdownToken(testShutdownToken),
				WithExpectedShutdownTokenHash(tt.hash),
			)

			require.Error(t, err)
			assert.Contains(t, err.Error(), "shutdown credential")
		})
	}
}

func TestNewMockModeConstructsChainClientWithoutFIFO(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "upgrades.json")
	require.NoError(t, os.WriteFile(configPath, []byte(`{"upgrades":[]}`), 0o600))

	server, err := New("chaind", WithMockMode(true), WithDataPath(dir), WithUpgradesConfig(configPath))
	require.NoError(t, err)
	t.Cleanup(func() { _ = server.client.Close() })
	assert.NotNil(t, server.client)
	assert.NotNil(t, server.upgradeMonitor)

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "upgrades.json", entries[0].Name())
}

func TestMissingShutdownCredentialRemainsValidStandaloneConfiguration(t *testing.T) {
	require.NoError(t, validateShutdownCredential("", ""))
}

func TestValidateShutdownCredentialAcceptsMatchingToken(t *testing.T) {
	require.NoError(t, validateShutdownCredential(testShutdownToken, ShutdownTokenHash(testShutdownToken)))
}
