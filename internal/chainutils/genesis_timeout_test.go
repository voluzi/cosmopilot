package chainutils

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestGenesisPodRunningTimeoutScalesWithExtraValidators(t *testing.T) {
	assert.Equal(t, time.Minute, genesisPodRunningTimeout(nil))

	extraValidators := []*GenesisValidator{
		{},
		{},
		{},
	}

	assert.GreaterOrEqual(t, genesisPodRunningTimeout(extraValidators), 7*time.Minute)
}
