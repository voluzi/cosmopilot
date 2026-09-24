package v1

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"k8s.io/utils/ptr"
)

func TestVerticalAutoscalingRuleGetDurationTreatsNonPositiveAsDefault(t *testing.T) {
	for _, d := range []string{"0s", "0", "-5m"} {
		assert.Equal(t, DefaultVpaCooldown, (&VerticalAutoscalingRule{Duration: ptr.To(d)}).GetDuration(), d)
	}
	assert.Equal(t, 10*time.Minute, (&VerticalAutoscalingRule{Duration: ptr.To("10m")}).GetDuration())
}
