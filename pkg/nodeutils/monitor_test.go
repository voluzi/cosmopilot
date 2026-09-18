package nodeutils

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	abci "github.com/cometbft/cometbft/abci/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeABCIClient struct {
	mu      sync.Mutex
	heights []int64
	errs    []error
	err     error
}

func (f *fakeABCIClient) GetAbciInfo(context.Context) (abci.ResponseInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		if err != nil {
			return abci.ResponseInfo{}, err
		}
	}
	if f.err != nil {
		return abci.ResponseInfo{}, f.err
	}
	height := f.heights[0]
	if len(f.heights) > 1 {
		f.heights = f.heights[1:]
	}
	return abci.ResponseInfo{LastBlockHeight: height}, nil
}

func newMonitorTestChecker(t *testing.T, config string) (*UpgradeChecker, string) {
	t.Helper()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "upgrades.json")
	require.NoError(t, os.WriteFile(configPath, []byte(config), 0o600))
	checker, err := NewUpgradeChecker(configPath)
	require.NoError(t, err)
	return checker, filepath.Join(dir, "upgrade-info.json")
}

func TestUpgradeMonitorStopsLateManualUpgradeOnce(t *testing.T) {
	checker, infoPath := newMonitorTestChecker(t, `{"upgrades":[{"height":100,"name":"v2","status":"scheduled","source":"manual"}]}`)
	client := &fakeABCIClient{heights: []int64{98, 102, 103}}
	var stops atomic.Int32
	monitor := newUpgradeMonitor(client, checker, infoPath, func() error {
		stops.Add(1)
		return nil
	})

	require.NoError(t, monitor.Reconcile(t.Context()))
	require.NoError(t, monitor.Reconcile(t.Context()))
	require.NoError(t, monitor.Reconcile(t.Context()))

	status := monitor.Status()
	require.NotNil(t, status.LatestHeight)
	assert.Equal(t, int64(103), *status.LatestHeight)
	require.NotNil(t, status.RequiredUpgrade)
	assert.Equal(t, RequiredUpgrade{Height: 100, Source: ManualUpgrade, Name: "v2"}, *status.RequiredUpgrade)
	assert.Equal(t, int32(1), stops.Load())
}

func TestUpgradeMonitorRetriesFailedManualStop(t *testing.T) {
	checker, infoPath := newMonitorTestChecker(t, `{"upgrades":[{"height":100,"status":"scheduled","source":"manual"}]}`)
	client := &fakeABCIClient{heights: []int64{99, 99}}
	var stops atomic.Int32
	monitor := newUpgradeMonitor(client, checker, infoPath, func() error {
		if stops.Add(1) == 1 {
			return errors.New("busy")
		}
		return nil
	})

	require.Error(t, monitor.Reconcile(t.Context()))
	require.NoError(t, monitor.Reconcile(t.Context()))
	assert.Equal(t, int32(2), stops.Load())
}

func TestUpgradeMonitorGovernanceRequiresMatchingSDKFile(t *testing.T) {
	checker, infoPath := newMonitorTestChecker(t, `{"upgrades":[{"height":100,"name":"v2","status":"scheduled","source":"on-chain"}]}`)
	client := &fakeABCIClient{heights: []int64{99, 99}}
	monitor := newUpgradeMonitor(client, checker, infoPath, func() error { return nil })

	require.NoError(t, monitor.Reconcile(t.Context()))
	assert.Nil(t, monitor.Status().RequiredUpgrade)
	require.NoError(t, os.WriteFile(infoPath, []byte(`{"name":"v2","height":100}`), 0o600))
	require.NoError(t, monitor.Reconcile(t.Context()))
	assert.Equal(t, &RequiredUpgrade{Height: 100, Source: OnChainUpgrade, Name: "v2"}, monitor.Status().RequiredUpgrade)
}

func TestUpgradeMonitorUsesReplacementPlanIdentityAtSameHeight(t *testing.T) {
	checker, infoPath := newMonitorTestChecker(t, `{"upgrades":[{"height":100,"name":"plan-b","image":"repo/app:v2","status":"scheduled","source":"on-chain"}]}`)
	client := &fakeABCIClient{heights: []int64{99, 99}}
	monitor := newUpgradeMonitor(client, checker, infoPath, func() error { return nil })

	require.NoError(t, os.WriteFile(infoPath, []byte(`{"name":"plan-a","height":100}`), 0o600))
	require.NoError(t, monitor.Reconcile(t.Context()))
	assert.Nil(t, monitor.Status().RequiredUpgrade)

	require.NoError(t, os.WriteFile(infoPath, []byte(`{"name":"plan-b","height":100}`), 0o600))
	require.NoError(t, monitor.Reconcile(t.Context()))
	assert.Equal(t, &RequiredUpgrade{Height: 100, Source: OnChainUpgrade, Name: "plan-b"}, monitor.Status().RequiredUpgrade)
}

func TestUpgradeMonitorRecoversGovernanceTargetWithoutRPC(t *testing.T) {
	checker, infoPath := newMonitorTestChecker(t, `{"upgrades":[{"height":100,"name":"v2","status":"scheduled","source":"on-chain"}]}`)
	require.NoError(t, os.WriteFile(infoPath, []byte(`{"name":"v2","height":100}`), 0o600))
	monitor := newUpgradeMonitor(&fakeABCIClient{err: errors.New("offline")}, checker, infoPath, func() error { return nil })

	require.NoError(t, monitor.Reconcile(t.Context()))
	status := monitor.Status()
	assert.Nil(t, status.LatestHeight)
	assert.Equal(t, &RequiredUpgrade{Height: 100, Source: OnChainUpgrade, Name: "v2"}, status.RequiredUpgrade)
}

func TestUpgradeMonitorClearsRecoveredGovernanceRequirementAfterCompletion(t *testing.T) {
	checker, infoPath := newMonitorTestChecker(t, `{"upgrades":[{"height":100,"name":"v2","status":"ongoing","source":"on-chain"}]}`)
	require.NoError(t, os.WriteFile(infoPath, []byte(`{"name":"v2","height":100}`), 0o600))
	client := &fakeABCIClient{err: errors.New("offline")}
	monitor := newUpgradeMonitor(client, checker, infoPath, func() error { return nil })

	require.NoError(t, monitor.Reconcile(t.Context()))
	require.NotNil(t, monitor.Status().RequiredUpgrade)

	client.mu.Lock()
	client.err = nil
	client.heights = []int64{100}
	client.mu.Unlock()
	require.NoError(t, os.WriteFile(checker.configFile, []byte(`{"upgrades":[{"height":100,"name":"v2","status":"completed","source":"on-chain"}]}`), 0o600))
	require.NoError(t, checker.reload())
	require.NoError(t, monitor.Reconcile(t.Context()))

	status := monitor.Status()
	require.NotNil(t, status.LatestHeight)
	assert.Equal(t, int64(100), *status.LatestHeight)
	assert.Nil(t, status.RequiredUpgrade)
}

func TestUpgradeMonitorRevalidatesRecoveredGovernanceRequirement(t *testing.T) {
	tests := []struct {
		name   string
		config string
	}{
		{name: "completed", config: `{"upgrades":[{"height":100,"name":"v2","status":"completed","source":"on-chain"}]}`},
		{name: "skipped", config: `{"upgrades":[{"height":100,"name":"v2","status":"skipped","source":"on-chain"}]}`},
		{name: "mismatched name", config: `{"upgrades":[{"height":100,"name":"v3","status":"ongoing","source":"on-chain"}]}`},
		{name: "mismatched height", config: `{"upgrades":[{"height":101,"name":"v2","status":"ongoing","source":"on-chain"}]}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checker, infoPath := newMonitorTestChecker(t, `{"upgrades":[{"height":100,"name":"v2","status":"ongoing","source":"on-chain"}]}`)
			require.NoError(t, os.WriteFile(infoPath, []byte(`{"name":"v2","height":100}`), 0o600))
			monitor := newUpgradeMonitor(&fakeABCIClient{err: errors.New("offline")}, checker, infoPath, func() error { return nil })
			require.NoError(t, monitor.Reconcile(t.Context()))
			require.NotNil(t, monitor.Status().RequiredUpgrade)

			require.NoError(t, os.WriteFile(checker.configFile, []byte(tt.config), 0o600))
			require.NoError(t, checker.reload())
			require.NoError(t, monitor.Reconcile(t.Context()))
			assert.Nil(t, monitor.Status().RequiredUpgrade)
		})
	}
}

func TestUpgradeMonitorUsesMarkerWhenCurrentHeightObservationFails(t *testing.T) {
	checker, infoPath := newMonitorTestChecker(t, `{"upgrades":[{"height":100,"name":"v2","status":"ongoing","source":"on-chain"}]}`)
	client := &fakeABCIClient{
		heights: []int64{98},
		errs:    []error{nil, errors.New("offline")},
	}
	monitor := newUpgradeMonitor(client, checker, infoPath, func() error { return nil })

	require.NoError(t, monitor.Reconcile(t.Context()))
	assert.Nil(t, monitor.Status().RequiredUpgrade)
	require.NoError(t, os.WriteFile(infoPath, []byte(`{"name":"v2","height":100}`), 0o600))
	require.NoError(t, monitor.Reconcile(t.Context()))

	status := monitor.Status()
	require.NotNil(t, status.LatestHeight)
	assert.Equal(t, int64(98), *status.LatestHeight)
	assert.Equal(t, &RequiredUpgrade{Height: 100, Source: OnChainUpgrade, Name: "v2"}, status.RequiredUpgrade)
}

func TestUpgradeMonitorIgnoresInvalidSDKUpgradeInfo(t *testing.T) {
	tests := []struct {
		name      string
		config    string
		file      string
		height    int64
		writeFile bool
	}{
		{name: "malformed", config: `{"upgrades":[{"height":100,"name":"v2","status":"scheduled","source":"on-chain"}]}`, file: `{`, height: 99, writeFile: true},
		{name: "zero height", config: `{"upgrades":[{"height":100,"name":"v2","status":"scheduled","source":"on-chain"}]}`, file: `{"name":"v2","height":0}`, height: 99, writeFile: true},
		{name: "wrong name", config: `{"upgrades":[{"height":100,"name":"v2","status":"scheduled","source":"on-chain"}]}`, file: `{"name":"v3","height":100}`, height: 99, writeFile: true},
		{name: "wrong height", config: `{"upgrades":[{"height":100,"name":"v2","status":"scheduled","source":"on-chain"}]}`, file: `{"name":"v2","height":101}`, height: 99, writeFile: true},
		{name: "manual config", config: `{"upgrades":[{"height":100,"name":"v2","status":"scheduled","source":"manual"}]}`, file: `{"name":"v2","height":100}`, height: 98, writeFile: true},
		{name: "completed", config: `{"upgrades":[{"height":100,"name":"v2","status":"completed","source":"on-chain"}]}`, file: `{"name":"v2","height":100}`, height: 99, writeFile: true},
		{name: "skipped", config: `{"upgrades":[{"height":100,"name":"v2","status":"skipped","source":"on-chain"}]}`, file: `{"name":"v2","height":100}`, height: 99, writeFile: true},
		{name: "already committed", config: `{"upgrades":[{"height":100,"name":"v2","status":"scheduled","source":"on-chain"}]}`, file: `{"name":"v2","height":100}`, height: 100, writeFile: true},
		{name: "known far future", config: `{"upgrades":[{"height":100,"name":"v2","status":"scheduled","source":"on-chain"}]}`, file: `{"name":"v2","height":100}`, height: 98, writeFile: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			checker, infoPath := newMonitorTestChecker(t, tt.config)
			if tt.writeFile {
				require.NoError(t, os.WriteFile(infoPath, []byte(tt.file), 0o600))
			}
			monitor := newUpgradeMonitor(&fakeABCIClient{heights: []int64{tt.height}}, checker, infoPath, func() error { return nil })
			require.NoError(t, monitor.Reconcile(t.Context()))
			assert.Nil(t, monitor.Status().RequiredUpgrade)
		})
	}
}

func TestUpgradeMonitorPartialThenValidSDKFileRecovers(t *testing.T) {
	checker, infoPath := newMonitorTestChecker(t, `{"upgrades":[{"height":100,"name":"v2","status":"scheduled","source":"on-chain"}]}`)
	client := &fakeABCIClient{heights: []int64{99, 99}}
	monitor := newUpgradeMonitor(client, checker, infoPath, func() error { return nil })
	require.NoError(t, os.WriteFile(infoPath, []byte(`{"name":`), 0o600))
	require.NoError(t, monitor.Reconcile(t.Context()))
	assert.Nil(t, monitor.Status().RequiredUpgrade)

	require.NoError(t, os.WriteFile(infoPath, []byte(`{"name":"v2","height":100}`), 0o600))
	require.NoError(t, monitor.Reconcile(t.Context()))
	assert.NotNil(t, monitor.Status().RequiredUpgrade)
}

func TestUpgradeMonitorManualConfigReloadTriggersWithoutWebsocket(t *testing.T) {
	for _, height := range []int64{99, 100, 102} {
		t.Run(strconv.FormatInt(height, 10), func(t *testing.T) {
			checker, infoPath := newMonitorTestChecker(t, `{"upgrades":[]}`)
			client := &fakeABCIClient{heights: []int64{height, height}}
			var stops atomic.Int32
			monitor := newUpgradeMonitor(client, checker, infoPath, func() error {
				stops.Add(1)
				return nil
			})
			require.NoError(t, monitor.Reconcile(t.Context()))

			require.NoError(t, os.WriteFile(checker.configFile, []byte(`{"upgrades":[{"height":100,"status":"scheduled","source":"manual"}]}`), 0o600))
			require.NoError(t, checker.reload())
			require.NoError(t, monitor.Reconcile(t.Context()))
			assert.Equal(t, int32(1), stops.Load())
			assert.Equal(t, int64(100), monitor.Status().RequiredUpgrade.Height)
		})
	}
}

func TestUpgradeMonitorRecoversOngoingNamelessGovernanceUpgrade(t *testing.T) {
	checker, infoPath := newMonitorTestChecker(t, `{"upgrades":[{"height":100,"status":"ongoing","source":"on-chain"}]}`)
	require.NoError(t, os.WriteFile(infoPath, []byte(`{"name":"v2","height":100}`), 0o600))
	monitor := newUpgradeMonitor(&fakeABCIClient{err: errors.New("offline")}, checker, infoPath, func() error { return nil })

	require.NoError(t, monitor.Reconcile(t.Context()))
	assert.Equal(t, &RequiredUpgrade{Height: 100, Source: OnChainUpgrade, Name: "v2"}, monitor.Status().RequiredUpgrade)
}

func TestUpgradeStatusUnknownHeightDoesNotInventObservation(t *testing.T) {
	status := UpgradeStatus{RequiredUpgrade: &RequiredUpgrade{Height: 100, Source: OnChainUpgrade, Name: "v2"}}
	b, err := json.Marshal(status)
	require.NoError(t, err)
	assert.JSONEq(t, `{"requiredUpgrade":{"height":100,"source":"on-chain","name":"v2"}}`, string(b))
}

func TestUpgradeStatusObservationTimestampIsRecorded(t *testing.T) {
	checker, infoPath := newMonitorTestChecker(t, `{"upgrades":[]}`)
	monitor := newUpgradeMonitor(&fakeABCIClient{heights: []int64{42}}, checker, infoPath, func() error { return nil })
	before := time.Now()
	require.NoError(t, monitor.Reconcile(t.Context()))
	status := monitor.Status()
	require.NotNil(t, status.HeightObservedAt)
	assert.False(t, status.HeightObservedAt.Before(before))
}
