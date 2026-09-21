package main

import (
	"os"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/voluzi/cosmopilot/v2/pkg/nodeutils"
)

func TestTerminationSignalRearmsDefaultWhenGracefulStopIsHeld(t *testing.T) {
	sigChan := make(chan os.Signal, 1)
	sigChan <- syscall.SIGTERM
	var sequence []string

	err := handleTerminationSignal(
		sigChan,
		func(force bool) (nodeutils.StopResult, error) {
			assert.False(t, force)
			sequence = append(sequence, "evidence-written")
			return nodeutils.StopHeld, nil
		},
		func(c chan<- os.Signal) {
			assert.Equal(t, (chan<- os.Signal)(sigChan), c)
			sequence = append(sequence, "notifications-stopped")
		},
		func(signals ...os.Signal) {
			assert.ElementsMatch(t, []os.Signal{syscall.SIGINT, syscall.SIGTERM}, signals)
			sequence = append(sequence, "defaults-restored")
		},
	)

	require.NoError(t, err)
	assert.Equal(t, []string{"evidence-written", "notifications-stopped", "defaults-restored"}, sequence)
}

func TestTerminationSignalDoesNotRearmDefaultForCompletedStop(t *testing.T) {
	sigChan := make(chan os.Signal, 1)
	sigChan <- syscall.SIGTERM
	var resetCalls int

	err := handleTerminationSignal(
		sigChan,
		func(bool) (nodeutils.StopResult, error) { return nodeutils.StopCompleted, nil },
		func(chan<- os.Signal) { resetCalls++ },
		func(...os.Signal) { resetCalls++ },
	)

	require.NoError(t, err)
	assert.Zero(t, resetCalls)
}
