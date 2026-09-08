package nodeutils

import (
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
