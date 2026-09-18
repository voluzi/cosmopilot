package nodeutils

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"time"

	abci "github.com/cometbft/cometbft/abci/types"
	log "github.com/sirupsen/logrus"
)

const (
	abciPollInterval   = time.Second
	abciRequestTimeout = time.Second
)

type RequiredUpgrade struct {
	Height int64         `json:"height"`
	Source UpgradeSource `json:"source"`
	Name   string        `json:"name,omitempty"`
}

type UpgradeStatus struct {
	LatestHeight          *int64           `json:"latestHeight,omitempty"`
	HeightObservedAt      *time.Time       `json:"heightObservedAt,omitempty"`
	RequiredUpgrade       *RequiredUpgrade `json:"requiredUpgrade"`
	LegacyUpgradeRequired bool             `json:"-"`
}

type abciInfoClient interface {
	GetAbciInfo(context.Context) (abci.ResponseInfo, error)
}

type upgradeMonitor struct {
	reconcileMu sync.Mutex
	mu          sync.RWMutex

	client          abciInfoClient
	checker         *UpgradeChecker
	upgradeInfoPath string
	stopNode        func() error
	status          UpgradeStatus
	stopSucceeded   bool
}

func newUpgradeMonitor(client abciInfoClient, checker *UpgradeChecker, upgradeInfoPath string, stopNode func() error) *upgradeMonitor {
	return &upgradeMonitor{
		client:          client,
		checker:         checker,
		upgradeInfoPath: upgradeInfoPath,
		stopNode:        stopNode,
	}
}

func (m *upgradeMonitor) Status() UpgradeStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	status := m.status
	if status.LatestHeight != nil {
		height := *status.LatestHeight
		status.LatestHeight = &height
	}
	if status.HeightObservedAt != nil {
		observed := *status.HeightObservedAt
		status.HeightObservedAt = &observed
	}
	if status.RequiredUpgrade != nil {
		required := *status.RequiredUpgrade
		status.RequiredUpgrade = &required
	}
	return status
}

func (m *upgradeMonitor) RequiresUpgrade() bool {
	return m.Status().RequiredUpgrade != nil
}

func (m *upgradeMonitor) Run(ctx context.Context, wake <-chan struct{}) {
	ticker := time.NewTicker(abciPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-wake:
		case <-m.checker.Changed():
		}
		if err := m.Reconcile(ctx); err != nil {
			log.WithError(err).Warn("upgrade reconciliation did not complete")
		}
	}
}

func (m *upgradeMonitor) Reconcile(ctx context.Context) error {
	m.reconcileMu.Lock()
	defer m.reconcileMu.Unlock()

	freshHeight := m.observeHeight(ctx)
	m.mu.Lock()
	if required := m.status.RequiredUpgrade; required != nil && required.Source == OnChainUpgrade &&
		!m.governanceRequirementValidLocked(*required, freshHeight) {
		m.status.RequiredUpgrade = nil
	}
	if m.status.RequiredUpgrade == nil {
		m.status.RequiredUpgrade = m.requiredUpgradeLocked(freshHeight)
	}
	required := m.status.RequiredUpgrade
	stopSucceeded := m.stopSucceeded
	m.mu.Unlock()

	if required == nil || required.Source != ManualUpgrade || stopSucceeded {
		return nil
	}
	if err := m.stopNode(); err != nil {
		return fmt.Errorf("stop node for upgrade at height %d: %w", required.Height, err)
	}
	m.mu.Lock()
	m.stopSucceeded = true
	m.mu.Unlock()
	return nil
}

func (m *upgradeMonitor) observeHeight(ctx context.Context) bool {
	requestCtx, cancel := context.WithTimeout(ctx, abciRequestTimeout)
	defer cancel()
	info, err := m.client.GetAbciInfo(requestCtx)
	if err != nil {
		return false
	}
	height := info.LastBlockHeight
	observed := time.Now()
	m.mu.Lock()
	m.status.LatestHeight = &height
	m.status.HeightObservedAt = &observed
	m.mu.Unlock()
	return true
}

func (m *upgradeMonitor) requiredUpgradeLocked(freshHeight bool) *RequiredUpgrade {
	config := m.checker.Snapshot()
	for _, upgrade := range config.Upgrades {
		if upgrade.Source != ManualUpgrade || upgrade.Status != UpgradeScheduled || upgrade.Height <= 0 {
			continue
		}
		if m.status.LatestHeight != nil && *m.status.LatestHeight >= upgrade.Height-1 {
			return &RequiredUpgrade{Height: upgrade.Height, Source: upgrade.Source, Name: upgrade.Name}
		}
	}

	info, err := readSDKUpgradeInfo(m.upgradeInfoPath)
	if err != nil {
		return nil
	}
	if !matchesGovernanceUpgrade(config, info) || !m.governanceHeightEligibleLocked(info.Height, freshHeight) {
		return nil
	}
	return &RequiredUpgrade{Height: info.Height, Source: OnChainUpgrade, Name: info.Name}
}

func (m *upgradeMonitor) governanceRequirementValidLocked(required RequiredUpgrade, freshHeight bool) bool {
	info, err := readSDKUpgradeInfo(m.upgradeInfoPath)
	if err != nil || info.Height != required.Height || info.Name != required.Name {
		return false
	}
	return matchesGovernanceUpgrade(m.checker.Snapshot(), info) &&
		m.governanceHeightEligibleLocked(info.Height, freshHeight)
}

func (m *upgradeMonitor) governanceHeightEligibleLocked(target int64, freshHeight bool) bool {
	if m.status.LatestHeight == nil {
		return true
	}
	height := *m.status.LatestHeight
	if height >= target {
		return false
	}
	return !freshHeight || height >= target-1
}

func matchesGovernanceUpgrade(config UpgradesConfig, info sdkUpgradeInfo) bool {
	for _, upgrade := range config.Upgrades {
		if upgrade.Source != OnChainUpgrade ||
			(upgrade.Status != UpgradeScheduled && upgrade.Status != UpgradeOnGoing) ||
			upgrade.Height != info.Height {
			continue
		}
		if upgrade.Name == "" || upgrade.Name == info.Name {
			return true
		}
	}
	return false
}

type sdkUpgradeInfo struct {
	Name   string `json:"name"`
	Height int64  `json:"height"`
}

func readSDKUpgradeInfo(path string) (sdkUpgradeInfo, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return sdkUpgradeInfo{}, err
	}
	var info sdkUpgradeInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return sdkUpgradeInfo{}, err
	}
	if info.Height <= 0 || info.Name == "" {
		return sdkUpgradeInfo{}, fmt.Errorf("invalid SDK upgrade info")
	}
	return info, nil
}
