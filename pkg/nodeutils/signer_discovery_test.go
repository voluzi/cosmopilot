package nodeutils

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

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
