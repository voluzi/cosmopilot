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
		{name: "ordinary sidecar stop leaves application running", force: false},
		{name: "explicit shutdown terminates application", force: true, wantStops: 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			checker, infoPath := newMonitorTestChecker(t, `{"upgrades":[]}`)
			monitor := newUpgradeMonitor(&fakeABCIClient{errs: []error{errors.New("offline")}}, checker, infoPath, func() error { return nil })
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

func TestStopWritesBoundedTerminationEvidence(t *testing.T) {
	for _, tt := range []struct {
		name       string
		force      bool
		wantForced bool
	}{
		{name: "graceful sidecar exit records halt intent"},
		{name: "forced shutdown is explicit", force: true, wantForced: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			checker, infoPath := newMonitorTestChecker(t, `{"upgrades":[]}`)
			height := int64(98)
			monitor := newUpgradeMonitor(&fakeABCIClient{errs: []error{errors.New("offline")}}, checker, infoPath, func() error { return nil })
			monitor.status.LatestHeight = &height
			terminationPath := filepath.Join(t.TempDir(), "termination.log")
			server := &NodeUtils{
				cfg: &Options{
					HaltHeight:             100,
					TerminationMessagePath: terminationPath,
				},
				server:             &http.Server{},
				upgradeMonitor:     monitor,
				stopNode:           func() error { return nil },
				shutdownHTTPServer: func() error { return nil },
			}

			require.NoError(t, server.Stop(tt.force))
			body, err := os.ReadFile(terminationPath)
			require.NoError(t, err)
			assert.LessOrEqual(t, len(body), 4096)
			var evidence TerminationEvidence
			require.NoError(t, json.Unmarshal(body, &evidence))
			require.NotNil(t, evidence.LatestHeight)
			assert.Equal(t, height, *evidence.LatestHeight)
			assert.Equal(t, int64(100), evidence.HaltHeight)
			assert.Equal(t, tt.wantForced, evidence.ForcedShutdown)
		})
	}
}

func TestGracefulStopStaysAliveForRequiredUpgradeAndHaltBoundary(t *testing.T) {
	for _, tt := range []struct {
		name     string
		height   int64
		required *RequiredUpgrade
	}{
		{name: "required upgrade", height: 50, required: &RequiredUpgrade{Height: 100, Source: OnChainUpgrade}},
		{name: "halt height minus one", height: 99},
		{name: "halt height", height: 100},
	} {
		t.Run(tt.name, func(t *testing.T) {
			config := `{"upgrades":[]}`
			if tt.required != nil {
				config = `{"upgrades":[{"height":100,"status":"scheduled","source":"on-chain"}]}`
			}
			checker, infoPath := newMonitorTestChecker(t, config)
			if tt.required != nil {
				require.NoError(t, os.WriteFile(infoPath, []byte(`{"name":"v2","height":100}`), 0o600))
			}
			monitor := newUpgradeMonitor(&fakeABCIClient{errs: []error{errors.New("offline")}}, checker, infoPath, func() error { return nil })
			monitor.status.LatestHeight = &tt.height
			monitor.status.RequiredUpgrade = tt.required
			var shutdowns atomic.Int32
			server := &NodeUtils{
				cfg:            &Options{HaltHeight: 100},
				server:         &http.Server{},
				upgradeMonitor: monitor,
				shutdownHTTPServer: func() error {
					shutdowns.Add(1)
					return nil
				},
				stopNode: func() error { return nil },
			}

			require.NoError(t, server.Stop(false))
			assert.Zero(t, shutdowns.Load())
		})
	}
}

func TestGracefulStopReportsHeldAfterWritingRequiredUpgradeEvidence(t *testing.T) {
	checker, infoPath := newMonitorTestChecker(t, `{"upgrades":[{"height":100,"status":"scheduled","source":"on-chain"}]}`)
	require.NoError(t, os.WriteFile(infoPath, []byte(`{"name":"v2","height":100}`), 0o600))
	height := int64(99)
	monitor := newUpgradeMonitor(&fakeABCIClient{errs: []error{errors.New("offline")}}, checker, infoPath, func() error { return nil })
	monitor.status.LatestHeight = &height
	monitor.status.RequiredUpgrade = &RequiredUpgrade{Height: 100, Source: OnChainUpgrade}
	terminationPath := filepath.Join(t.TempDir(), "termination.log")
	var shutdowns atomic.Int32
	server := &NodeUtils{
		cfg: &Options{
			TerminationMessagePath: terminationPath,
		},
		server:         &http.Server{},
		upgradeMonitor: monitor,
		shutdownHTTPServer: func() error {
			shutdowns.Add(1)
			return nil
		},
		stopNode: func() error { return nil },
	}

	result, err := server.StopWithResult(false)
	require.NoError(t, err)
	assert.Equal(t, StopHeld, result)
	assert.Zero(t, shutdowns.Load())
	body, err := os.ReadFile(terminationPath)
	require.NoError(t, err)
	var evidence TerminationEvidence
	require.NoError(t, json.Unmarshal(body, &evidence))
	require.NotNil(t, evidence.RequiredUpgrade)
	assert.Equal(t, int64(100), evidence.RequiredUpgrade.Height)
	assert.False(t, evidence.ForcedShutdown)
}

func TestGracefulStopReportsCompletedForOrdinaryShutdown(t *testing.T) {
	checker, infoPath := newMonitorTestChecker(t, `{"upgrades":[]}`)
	monitor := newUpgradeMonitor(&fakeABCIClient{errs: []error{errors.New("offline")}}, checker, infoPath, func() error { return nil })
	var shutdowns atomic.Int32
	server := &NodeUtils{
		cfg:            &Options{},
		server:         &http.Server{},
		upgradeMonitor: monitor,
		shutdownHTTPServer: func() error {
			shutdowns.Add(1)
			return nil
		},
		stopNode: func() error { return nil },
	}

	result, err := server.StopWithResult(false)
	require.NoError(t, err)
	assert.Equal(t, StopCompleted, result)
	assert.Equal(t, int32(1), shutdowns.Load())
}
