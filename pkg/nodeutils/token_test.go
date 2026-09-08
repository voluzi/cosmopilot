package nodeutils

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGenerateShutdownToken(t *testing.T) {
	first, err := GenerateShutdownToken()
	require.NoError(t, err)
	second, err := GenerateShutdownToken()
	require.NoError(t, err)

	assert.True(t, ValidShutdownToken(first))
	assert.True(t, ValidShutdownToken(second))
	assert.NotEqual(t, first, second)
}

func TestValidShutdownTokenRejectsMalformedValues(t *testing.T) {
	for _, token := range []string{"", "short", testShutdownToken + "A", "!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!!"} {
		t.Run(token, func(t *testing.T) {
			assert.False(t, ValidShutdownToken(token))
		})
	}
}

func TestShutdownTokenHashIsCanonical(t *testing.T) {
	assert.Equal(t, "0f007385b6f9d4b7eeb2748605afe1a984a0a3bfa3f014d09e2a784ce9e5cd1a", ShutdownTokenHash(testShutdownToken))
	assert.True(t, ValidShutdownTokenHash(ShutdownTokenHash(testShutdownToken)))

	for _, hash := range []string{"", "short", strings.Repeat("A", 64), strings.Repeat("g", 64)} {
		t.Run(hash, func(t *testing.T) {
			assert.False(t, ValidShutdownTokenHash(hash))
		})
	}
}
