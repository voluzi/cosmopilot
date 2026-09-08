package nodeutils

import (
	"strings"
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
				WithTraceStore("/path/that/must/not/exist/trace.fifo"),
			)

			require.Error(t, err)
			assert.Contains(t, err.Error(), "shutdown credential")
		})
	}
}

func TestMissingShutdownCredentialRemainsValidStandaloneConfiguration(t *testing.T) {
	require.NoError(t, validateShutdownCredential("", ""))
}

func TestNewAcceptsMatchingShutdownCredential(t *testing.T) {
	_, err := New("chaind",
		WithShutdownToken(testShutdownToken),
		WithExpectedShutdownTokenHash(ShutdownTokenHash(testShutdownToken)),
		WithTraceStore("/path/that/must/not/exist/trace.fifo"),
	)

	require.Error(t, err)
	assert.NotContains(t, strings.ToLower(err.Error()), "shutdown credential")
}
