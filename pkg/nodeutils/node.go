package nodeutils

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"emperror.dev/errors"
	"github.com/gorilla/mux"
	"github.com/shirou/gopsutil/process"
	log "github.com/sirupsen/logrus"

	"github.com/voluzi/cosmopilot/v3/internal/chainutils"
	"github.com/voluzi/cosmopilot/v3/pkg/proxy"
	"github.com/voluzi/cosmopilot/v3/pkg/statscollector"
)

const (
	fineStatsCollectorInterval   = 10 * time.Second
	coarseStatsCollectorInterval = 5 * time.Minute
	signerPeerLookupTimeout      = 2 * time.Second
)

type signerPeerResolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

type tmkmsProxy interface {
	Start() error
	Stop() error
}

type NodeUtils struct {
	server                 *http.Server
	router                 *mux.Router
	cfg                    *Options
	client                 *chainutils.Client
	upgradeChecker         *UpgradeChecker
	upgradeMonitor         *upgradeMonitor
	tmkmsActive            atomic.Bool
	signerDiscovered       atomic.Bool
	signerPeerResolver     signerPeerResolver
	trustedSignerAddresses atomic.Pointer[[]net.IPAddr]
	signerPeerLookupActive atomic.Bool
	tmkmsProxy             tmkmsProxy
	nodeBinaryName         string
	processMu              sync.Mutex
	appProcess             *process.Process
	fineStats              *statscollector.Collector
	coarseStats            *statscollector.Collector
	mockStats              *MockStats
	dataSizeMu             sync.Mutex
	statfs                 statfsFunc
	shutdownStarted        atomic.Bool
	forcedShutdown         atomic.Bool
	terminationEvidenceMu  sync.Mutex
	stopNode               func() error
	cancel                 context.CancelFunc
}

func New(nodeBinaryName string, opts ...Option) (*NodeUtils, error) {
	options := defaultOptions()
	for _, opt := range opts {
		opt(options)
	}
	if err := validateShutdownCredential(options.ShutdownToken, options.ExpectedShutdownTokenHash); err != nil {
		return nil, err
	}

	nodeUtils := &NodeUtils{
		cfg:                options,
		router:             mux.NewRouter(),
		nodeBinaryName:     nodeBinaryName,
		signerPeerResolver: net.DefaultResolver,
		fineStats:          statscollector.NewCollector(int(time.Hour / fineStatsCollectorInterval)),
		coarseStats:        statscollector.NewCollector(int((24 * time.Hour) / coarseStatsCollectorInterval)),
		statfs:             syscall.Statfs,
	}
	nodeUtils.stopNode = nodeUtils.StopNode

	uc, err := NewUpgradeChecker(options.UpgradesConfig)
	if err != nil {
		return nil, err
	}
	nodeUtils.upgradeChecker = uc
	client, err := chainutils.NewClient("127.0.0.1")
	if err != nil {
		return nil, err
	}
	nodeUtils.client = client
	nodeUtils.upgradeMonitor = newUpgradeMonitor(
		client,
		uc,
		filepath.Join(options.DataPath, "upgrade-info.json"),
		nodeUtils.stopNode,
	)

	if options.MockMode {
		nodeUtils.mockStats = NewMockStats()
		log.Info("node-utils starting in mock mode")
		return nodeUtils, nil
	}

	if options.TmkmsProxy {
		acceptSigner := func(*net.TCPConn) bool {
			nodeUtils.signerDiscovered.Store(true)
			return true
		}
		if options.SignerPeerDNS != "" {
			acceptSigner = nodeUtils.acceptTrustedSignerPeer
			nodeUtils.refreshTrustedSignerPeers()
		}
		nodeUtils.tmkmsProxy, err = proxy.NewTCPProxy(":26659", "127.0.0.1:5555", true, acceptSigner)
		if err != nil {
			_ = client.Close()
			return nil, err
		}
	}

	return nodeUtils, nil
}

func validateShutdownCredential(token, expectedHash string) error {
	if token == "" && expectedHash == "" {
		return nil
	}
	if !ValidShutdownToken(token) {
		return fmt.Errorf("shutdown credential token is missing or malformed")
	}
	if !ValidShutdownTokenHash(expectedHash) {
		return fmt.Errorf("shutdown credential expected hash is missing or malformed")
	}
	if ShutdownTokenHash(token) != expectedHash {
		return fmt.Errorf("shutdown credential does not match its expected hash")
	}
	return nil
}

func trustedSignerPeer(peer net.IP, addresses []net.IPAddr) bool {
	for _, address := range addresses {
		if address.IP.Equal(peer) {
			return true
		}
	}
	return false
}

func (s *NodeUtils) refreshTrustedSignerPeers() {
	if !s.signerPeerLookupActive.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer s.signerPeerLookupActive.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), signerPeerLookupTimeout)
		defer cancel()
		addresses, err := s.signerPeerResolver.LookupIPAddr(ctx, s.cfg.SignerPeerDNS)
		if err != nil {
			log.WithError(err).WithField("hostname", s.cfg.SignerPeerDNS).Warn("failed to resolve trusted signer peers")
			return
		}
		resolved := append([]net.IPAddr(nil), addresses...)
		s.trustedSignerAddresses.Store(&resolved)
	}()
}

func (s *NodeUtils) acceptTrustedSignerPeer(conn *net.TCPConn) bool {
	peer, ok := conn.RemoteAddr().(*net.TCPAddr)
	if !ok || peer.IP == nil {
		return false
	}
	return s.acceptTrustedSignerIP(peer.IP)
}

func (s *NodeUtils) acceptTrustedSignerIP(peer net.IP) bool {
	if peer == nil {
		return false
	}
	addresses := s.trustedSignerAddresses.Load()
	if addresses != nil && trustedSignerPeer(peer, *addresses) {
		s.signerDiscovered.Store(true)
		return true
	}
	s.refreshTrustedSignerPeers()
	log.WithFields(log.Fields{"peer": peer.String(), "hostname": s.cfg.SignerPeerDNS}).Warn("remote-signer connection came from an untrusted peer")
	return false
}

func (s *NodeUtils) runTmkmsProxy() {
	for {
		s.tmkmsActive.Store(true)
		err := s.tmkmsProxy.Start()
		s.tmkmsActive.Store(false)
		if errors.Is(err, proxy.ErrStopped) {
			return
		}
		log.Errorf("tmkms connection finished with error: %v", err)

		// If an upgrade is required lets not restart proxy
		if s.upgradeMonitor.RequiresUpgrade() {
			return
		}

		// Wait one second before restarting
		time.Sleep(time.Second)
	}
}

func (s *NodeUtils) Start() error {
	s.registerRoutes()
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	defer cancel()
	defer s.client.Close()

	go func() {
		if err := s.upgradeChecker.WatchConfigFile(ctx); err != nil {
			log.Errorf("error watching config file: %v", err)
		}
	}()
	if err := s.upgradeMonitor.Reconcile(ctx); err != nil {
		log.WithError(err).Warn("initial upgrade reconciliation did not complete")
	}
	blockWake := make(chan struct{}, 1)
	go runNewBlockWatcher(ctx, fmt.Sprintf("ws://127.0.0.1:%d/websocket", chainutils.RpcPort), blockWake)
	go s.upgradeMonitor.Run(ctx, blockWake)

	if s.tmkmsProxy != nil {
		go s.runTmkmsProxy()
	}

	// Fine-grained collector (1h window)
	go func() {
		ticker := time.NewTicker(fineStatsCollectorInterval)
		defer ticker.Stop()
		for range ticker.C {
			if err := s.collectProcessStats(s.fineStats); err != nil {
				log.Errorf("error collecting process fine-grained stats: %v", err)
			}
		}
	}()

	// Coarse-grained collector (24h window)
	go func() {
		ticker := time.NewTicker(coarseStatsCollectorInterval)
		defer ticker.Stop()
		for range ticker.C {
			if err := s.collectProcessStats(s.coarseStats); err != nil {
				log.Errorf("error collecting process coarse-grained stats: %v", err)
			}
		}
	}()

	s.server = &http.Server{Addr: fmt.Sprintf("%s:%d", s.cfg.Host, s.cfg.Port), Handler: s.router}
	log.Infof("server started listening on %s:%d ...\n\n", s.cfg.Host, s.cfg.Port)
	if err := s.server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (s *NodeUtils) Stop(force bool) error {
	log.WithField("force", force).Info("stopping server")
	var status UpgradeStatus
	if force {
		s.prepareForcedShutdown()
		status = s.upgradeMonitor.Status()
	} else {
		s.terminationEvidenceMu.Lock()
		if s.forcedShutdown.Load() {
			s.terminationEvidenceMu.Unlock()
			return nil
		}
		if err := s.upgradeMonitor.reconcile(context.Background(), false); err != nil {
			log.WithError(err).Warn("final upgrade reconciliation did not complete")
		}
		status = s.upgradeMonitor.Status()
		if !s.forcedShutdown.Load() && s.cfg.TerminationMessagePath != "" {
			if body, err := json.Marshal(status); err != nil {
				log.WithError(err).Warn("failed to marshal final node-utils status")
			} else if err := os.WriteFile(s.cfg.TerminationMessagePath, body, 0o600); err != nil {
				log.WithError(err).Warn("failed to persist final node-utils status")
			}
		}
		s.terminationEvidenceMu.Unlock()
		if s.forcedShutdown.Load() {
			return nil
		}
	}

	// When Stop is not forced, in the case of an upgrade being required we ignore
	// the Stop call. This is most likely coming from SIGINT or SIGTERM signals and
	// we need to wait for cosmopilot to read /requires_upgrade before shutting down.
	// When stop is forced (coming from /shutdown endpoint mostly) we ignore the upgrade
	// requirement.
	// Another case is when respecting halt-height. We want to keep node-utils alive a bit
	// more so that cosmopilot can retrieve latest height before total shutdown.
	// Note: Only check halt-height if it's actually configured (> 0), otherwise
	// halt-height=0 would match latestBlockHeight=0 and prevent shutdown.
	var latestHeight int64
	if status.LatestHeight != nil {
		latestHeight = *status.LatestHeight
	}
	atHaltBoundary := s.cfg.HaltHeight > 0 &&
		(latestHeight == s.cfg.HaltHeight || latestHeight == s.cfg.HaltHeight-1)
	if !force && (status.RequiredUpgrade != nil || atHaltBoundary) {
		log.Warn("node requires upgrade or is set to halt on specific height. ignoring stop call")
		return nil
	}

	if s.server == nil {
		return fmt.Errorf("server was not started")
	}
	if s.cancel != nil {
		s.cancel()
	}

	// Stop tmkms proxy if it is still alive
	if s.tmkmsProxy != nil {
		log.Debug("stopping tmkms proxy")
		if err := s.tmkmsProxy.Stop(); err != nil {
			log.Errorf("failed to stop tmkms proxy: %v", err)
		}
	}

	if force {
		log.Debug("stopping node")
		if err := s.stopNode(); err != nil {
			log.Errorf("failed to stop node: %v", err)
		}
	}

	// Shutdown main server
	log.Debug("shutting down http server")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return s.server.Shutdown(ctx)
}

func (s *NodeUtils) prepareForcedShutdown() {
	s.forcedShutdown.Store(true)
	s.terminationEvidenceMu.Lock()
	defer s.terminationEvidenceMu.Unlock()
	if s.cfg.TerminationMessagePath == "" {
		return
	}
	if err := os.Truncate(s.cfg.TerminationMessagePath, 0); err != nil && !errors.Is(err, os.ErrNotExist) {
		log.WithError(err).Warn("failed to clear final node-utils status")
	}
}

func (s *NodeUtils) getNodeProcess() (*process.Process, error) {
	s.processMu.Lock()
	defer s.processMu.Unlock()
	if s.appProcess != nil {
		return s.appProcess, nil
	}
	var err error
	s.appProcess, err = findProcessByName(s.nodeBinaryName)
	return s.appProcess, err
}

func (s *NodeUtils) StopNode() error {
	p, err := s.getNodeProcess()
	if err != nil {
		return err
	}
	return p.Terminate()
}

func (s *NodeUtils) collectProcessStats(collector *statscollector.Collector) error {
	p, err := s.getNodeProcess()
	if err != nil {
		return err
	}

	stats, err := GetProcessStats(p)
	if err != nil {
		return err
	}

	collector.AddSample(statscollector.ProcessStats{
		CPUTimeSec: stats.CPUTimeSec,
		MemoryRSS:  stats.MemoryRSS,
	})
	return nil
}
