package nodeutils

import (
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
