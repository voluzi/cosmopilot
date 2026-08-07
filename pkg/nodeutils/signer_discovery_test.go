package nodeutils

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/voluzi/cosmopilot/v3/pkg/proxy"
)

type blockingSignerPeerResolver struct {
	started chan struct{}
	release chan struct{}
}

func (r *blockingSignerPeerResolver) LookupIPAddr(ctx context.Context, _ string) ([]net.IPAddr, error) {
	select {
	case r.started <- struct{}{}:
	default:
	}
	select {
	case <-r.release:
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func TestSignerDiscoveredStatus(t *testing.T) {
	server := &NodeUtils{}
	request := httptest.NewRequest(http.MethodGet, "/signer_discovered", nil)

	response := httptest.NewRecorder()
	server.signerDiscoveredStatus(response, request)
	if response.Code != http.StatusNotAcceptable {
		t.Fatalf("status before discovery = %d, want %d", response.Code, http.StatusNotAcceptable)
	}

	server.signerDiscovered.Store(true)
	response = httptest.NewRecorder()
	server.signerDiscoveredStatus(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status after discovery = %d, want %d", response.Code, http.StatusOK)
	}
}

func TestTrustedSignerPeer(t *testing.T) {
	addresses := []net.IPAddr{{IP: net.ParseIP("10.0.0.2")}, {IP: net.ParseIP("fd00::2")}}
	if !trustedSignerPeer(net.ParseIP("10.0.0.2"), addresses) {
		t.Fatal("expected listed IPv4 signer peer to be trusted")
	}
	if !trustedSignerPeer(net.ParseIP("fd00::2"), addresses) {
		t.Fatal("expected listed IPv6 signer peer to be trusted")
	}
	if trustedSignerPeer(net.ParseIP("10.0.0.3"), addresses) {
		t.Fatal("unexpected trust for an unlisted peer")
	}
}

func TestAcceptTrustedSignerPeerDoesNotBlockOnDNS(t *testing.T) {
	resolver := &blockingSignerPeerResolver{started: make(chan struct{}, 1), release: make(chan struct{})}
	server := &NodeUtils{
		cfg:                &Options{SignerPeerDNS: "signer.example"},
		signerPeerResolver: resolver,
	}

	listener, err := net.ListenTCP("tcp", &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	dialed := make(chan error, 1)
	go func() {
		conn, dialErr := net.DialTCP("tcp", nil, listener.Addr().(*net.TCPAddr))
		if conn != nil {
			defer conn.Close()
		}
		dialed <- dialErr
	}()
	conn, err := listener.AcceptTCP()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	started := time.Now()
	if server.acceptTrustedSignerPeer(conn) {
		t.Fatal("peer was trusted before DNS completed")
	}
	if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
		t.Fatalf("accept callback blocked for %v", elapsed)
	}
	select {
	case <-resolver.started:
	case <-time.After(time.Second):
		t.Fatal("DNS refresh did not start")
	}
	close(resolver.release)
	deadline := time.Now().Add(time.Second)
	for server.trustedSignerAddresses.Load() == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if server.trustedSignerAddresses.Load() == nil {
		t.Fatal("DNS refresh did not publish trusted addresses")
	}
	if !server.acceptTrustedSignerPeer(conn) {
		t.Fatal("peer was not trusted after DNS refresh")
	}
	if !server.signerDiscovered.Load() {
		t.Fatal("trusted peer did not release signer discovery gate")
	}
	if err := <-dialed; err != nil {
		t.Fatal(err)
	}
}

func TestAcceptTrustedSignerPeerRejectsPeerAbsentFromTrustedSnapshot(t *testing.T) {
	resolver := &blockingSignerPeerResolver{started: make(chan struct{}, 1), release: make(chan struct{})}
	server := &NodeUtils{
		cfg:                &Options{SignerPeerDNS: "signer.example"},
		signerPeerResolver: resolver,
	}
	trusted := []net.IPAddr{{IP: net.ParseIP("192.0.2.10")}}
	server.trustedSignerAddresses.Store(&trusted)

	if server.acceptTrustedSignerIP(net.ParseIP("127.0.0.1")) {
		t.Fatal("peer absent from trusted snapshot was accepted")
	}
	if server.signerDiscovered.Load() {
		t.Fatal("untrusted peer released signer discovery gate")
	}
	close(resolver.release)
}

type stoppedTmkmsProxy struct {
	starts    atomic.Int32
	started   chan struct{}
	stopped   chan struct{}
	startOnce sync.Once
	stopOnce  sync.Once
}

func (p *stoppedTmkmsProxy) Start() error {
	p.starts.Add(1)
	p.startOnce.Do(func() { close(p.started) })
	<-p.stopped
	return proxy.ErrStopped
}

func (p *stoppedTmkmsProxy) Stop() error {
	p.stopOnce.Do(func() { close(p.stopped) })
	return nil
}

func TestStartProxyLoopExitsAfterStopReturnsErrStopped(t *testing.T) {
	tmkmsProxy := &stoppedTmkmsProxy{started: make(chan struct{}), stopped: make(chan struct{})}
	server := &NodeUtils{tmkmsProxy: tmkmsProxy}
	done := make(chan struct{})
	go func() {
		server.runTmkmsProxy()
		close(done)
	}()
	select {
	case <-tmkmsProxy.started:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("proxy loop did not start")
	}
	if err := tmkmsProxy.Stop(); err != nil {
		t.Fatal(err)
	}

	select {
	case <-done:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("proxy loop did not exit after ErrStopped")
	}
	if got := tmkmsProxy.starts.Load(); got != 1 {
		t.Fatalf("proxy Start calls = %d, want 1 after ErrStopped", got)
	}
	if server.tmkmsActive.Load() {
		t.Fatal("tmkms proxy remained active after ErrStopped")
	}
}

func TestRefreshTrustedSignerPeersKeepsLastSuccessfulAddresses(t *testing.T) {
	resolver := &blockingSignerPeerResolver{started: make(chan struct{}, 2), release: make(chan struct{})}
	server := &NodeUtils{
		cfg:                &Options{SignerPeerDNS: "signer.example"},
		signerPeerResolver: resolver,
	}
	initial := []net.IPAddr{{IP: net.ParseIP("10.0.0.2")}}
	server.trustedSignerAddresses.Store(&initial)

	server.refreshTrustedSignerPeers()
	select {
	case <-resolver.started:
	case <-time.After(time.Second):
		t.Fatal("DNS refresh did not start")
	}
	if addresses := server.trustedSignerAddresses.Load(); addresses == nil || !trustedSignerPeer(net.ParseIP("10.0.0.2"), *addresses) {
		t.Fatal("refresh discarded last successful addresses while lookup was pending")
	}
	close(resolver.release)
}
