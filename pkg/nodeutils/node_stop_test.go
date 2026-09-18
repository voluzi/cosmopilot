package nodeutils

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
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

func TestGracefulStopPreservesFinalHaltBoundaryObservation(t *testing.T) {
	for _, tt := range []struct {
		name         string
		height       int64
		force        bool
		wantRunning  bool
		wantStops    int32
		wantSnapshot bool
	}{
		{name: "at previous committed height", height: 99, wantRunning: true, wantSnapshot: true},
		{name: "at configured height", height: 100, wantRunning: true, wantSnapshot: true},
		{name: "well before configured height", height: 98, wantSnapshot: true},
		{name: "forced stop at boundary", height: 99, force: true, wantStops: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			checker, infoPath := newMonitorTestChecker(t, `{"upgrades":[]}`)
			monitor := newUpgradeMonitor(&fakeABCIClient{err: errors.New("offline")}, checker, infoPath, func() error { return nil })
			monitor.status.LatestHeight = &tt.height
			terminationPath := filepath.Join(t.TempDir(), "termination.log")
			var cancels, stops atomic.Int32
			server := &NodeUtils{
				cfg: &Options{
					HaltHeight:             100,
					TerminationMessagePath: terminationPath,
				},
				server:         &http.Server{},
				upgradeMonitor: monitor,
				cancel:         func() { cancels.Add(1) },
				stopNode: func() error {
					stops.Add(1)
					return nil
				},
			}

			require.NoError(t, server.Stop(tt.force))
			if tt.wantRunning {
				assert.Zero(t, cancels.Load())
			} else {
				assert.Equal(t, int32(1), cancels.Load())
			}
			assert.Equal(t, tt.wantStops, stops.Load())

			body, err := os.ReadFile(terminationPath)
			if !tt.wantSnapshot {
				assert.ErrorIs(t, err, os.ErrNotExist)
				return
			}
			require.NoError(t, err)
			var status UpgradeStatus
			require.NoError(t, json.Unmarshal(body, &status))
			require.NotNil(t, status.LatestHeight)
			assert.Equal(t, tt.height, *status.LatestHeight)
		})
	}
}

func TestForcedStopClearsGracefulTerminationSnapshot(t *testing.T) {
	height := int64(99)
	checker, infoPath := newMonitorTestChecker(t, `{"upgrades":[]}`)
	monitor := newUpgradeMonitor(&fakeABCIClient{err: errors.New("offline")}, checker, infoPath, func() error { return nil })
	monitor.status.LatestHeight = &height
	terminationPath := filepath.Join(t.TempDir(), "termination.log")
	var stops atomic.Int32
	server := &NodeUtils{
		cfg: &Options{
			HaltHeight:             100,
			TerminationMessagePath: terminationPath,
		},
		server:         &http.Server{},
		upgradeMonitor: monitor,
		stopNode: func() error {
			stops.Add(1)
			return nil
		},
	}

	require.NoError(t, server.Stop(false))
	body, err := os.ReadFile(terminationPath)
	require.NoError(t, err)
	assert.NotEmpty(t, body)

	require.NoError(t, server.Stop(true))
	body, err = os.ReadFile(terminationPath)
	require.NoError(t, err)
	assert.Empty(t, body)
	assert.Equal(t, int32(1), stops.Load())
}

func TestGracefulStopDoesNotStopApplicationForNewManualRequirement(t *testing.T) {
	checker, infoPath := newMonitorTestChecker(t, `{"upgrades":[{"height":100,"name":"v2","status":"scheduled","source":"manual"}]}`)
	var stops atomic.Int32
	monitor := newUpgradeMonitor(&fakeABCIClient{heights: []int64{99}}, checker, infoPath, func() error {
		stops.Add(1)
		return nil
	})
	server := &NodeUtils{
		cfg:            &Options{},
		server:         &http.Server{},
		upgradeMonitor: monitor,
		stopNode: func() error {
			stops.Add(1)
			return nil
		},
	}

	require.NoError(t, server.Stop(false))
	assert.Zero(t, stops.Load())
	assert.Equal(t, &RequiredUpgrade{Height: 100, Source: ManualUpgrade, Name: "v2"}, monitor.Status().RequiredUpgrade)
}
