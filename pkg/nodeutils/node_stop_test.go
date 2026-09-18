package nodeutils

import (
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStopOnlyStopsApplicationWhenForced(t *testing.T) {
	for _, tt := range []struct {
		name      string
		force     bool
		wantStops int32
	}{
		{name: "graceful sidecar stop leaves application running", force: false},
		{name: "forced authenticated stop terminates application", force: true, wantStops: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			checker, infoPath := newMonitorTestChecker(t, `{"upgrades":[]}`)
			monitor := newUpgradeMonitor(
				&fakeABCIClient{err: errors.New("offline")},
				checker,
				infoPath,
				func() error { return nil },
			)
			var stops atomic.Int32
			server := &NodeUtils{
				cfg:            &Options{},
				server:         &http.Server{},
				upgradeMonitor: monitor,
				stopNode: func() error {
					stops.Add(1)
					return nil
				},
			}

			require.NoError(t, server.Stop(tt.force))
			assert.Equal(t, tt.wantStops, stops.Load())
		})
	}
}
