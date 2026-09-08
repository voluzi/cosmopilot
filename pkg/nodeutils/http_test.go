package nodeutils

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testShutdownToken = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
const wrongTestShutdownToken = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE"

func newShutdownTestServer(token string, stop func() error) *NodeUtils {
	s := &NodeUtils{
		cfg:      &Options{ShutdownToken: token},
		router:   mux.NewRouter(),
		server:   &http.Server{},
		stopNode: stop,
	}
	s.registerRoutes()
	return s
}

func TestShutdownServerRequiresBearerToken(t *testing.T) {
	tests := []struct {
		name          string
		configured    string
		authorization string
		wantStatus    int
		wantStops     int32
	}{
		{name: "no configured token", authorization: "Bearer " + testShutdownToken, wantStatus: http.StatusUnauthorized},
		{name: "missing authorization", configured: testShutdownToken, wantStatus: http.StatusUnauthorized},
		{name: "wrong scheme", configured: testShutdownToken, authorization: "Basic " + testShutdownToken, wantStatus: http.StatusUnauthorized},
		{name: "empty bearer token", configured: testShutdownToken, authorization: "Bearer ", wantStatus: http.StatusUnauthorized},
		{name: "malformed bearer token", configured: testShutdownToken, authorization: "Bearer " + testShutdownToken + " extra", wantStatus: http.StatusUnauthorized},
		{name: "wrong bearer token", configured: testShutdownToken, authorization: "Bearer " + wrongTestShutdownToken, wantStatus: http.StatusUnauthorized},
		{name: "valid bearer token", configured: testShutdownToken, authorization: "Bearer " + testShutdownToken, wantStatus: http.StatusAccepted, wantStops: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stops atomic.Int32
			stopped := make(chan struct{}, 1)
			s := newShutdownTestServer(tt.configured, func() error {
				stops.Add(1)
				stopped <- struct{}{}
				return nil
			})
			req := httptest.NewRequest(http.MethodPost, "/shutdown", nil)
			if tt.authorization != "" {
				req.Header.Set("Authorization", tt.authorization)
			}
			resp := httptest.NewRecorder()

			s.router.ServeHTTP(resp, req)

			assert.Equal(t, tt.wantStatus, resp.Code)
			if tt.wantStops > 0 {
				select {
				case <-stopped:
				case <-time.After(time.Second):
					t.Fatal("authorized shutdown did not start")
				}
			}
			assert.Equal(t, tt.wantStops, stops.Load())
		})
	}
}

func TestShutdownServerIsPostOnly(t *testing.T) {
	s := newShutdownTestServer(testShutdownToken, func() error {
		t.Fatal("non-POST request started shutdown")
		return nil
	})
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			req := httptest.NewRequest(method, "/shutdown", nil)
			req.Header.Set("Authorization", "Bearer "+testShutdownToken)
			resp := httptest.NewRecorder()

			s.router.ServeHTTP(resp, req)

			assert.Equal(t, http.StatusMethodNotAllowed, resp.Code)
		})
	}
}

func TestShutdownServerAcknowledgesBeforeStopCompletes(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	s := newShutdownTestServer(testShutdownToken, func() error {
		close(started)
		<-release
		return nil
	})
	req := httptest.NewRequest(http.MethodPost, "/shutdown", nil)
	req.Header.Set("Authorization", "Bearer "+testShutdownToken)
	resp := httptest.NewRecorder()
	served := make(chan struct{})
	go func() {
		s.router.ServeHTTP(resp, req)
		close(served)
	}()

	select {
	case <-served:
	case <-time.After(time.Second):
		t.Fatal("shutdown response waited for stop completion")
	}
	require.Equal(t, http.StatusAccepted, resp.Code)
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not start after acknowledgement")
	}
	close(release)
}

func TestShutdownServerAcknowledgesBeforeServerStops(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	stopStarted := make(chan struct{})
	releaseStop := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(releaseStop) })

	s := &NodeUtils{
		cfg:    &Options{ShutdownToken: testShutdownToken},
		router: mux.NewRouter(),
		stopNode: func() error {
			close(stopStarted)
			<-releaseStop
			return nil
		},
	}
	s.registerRoutes()
	s.server = &http.Server{Handler: s.router}
	defer s.server.Close()

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- s.server.Serve(listener)
	}()

	response := make(chan *http.Response, 1)
	requestErr := make(chan error, 1)
	go func() {
		req, reqErr := http.NewRequest(http.MethodPost, "http://"+listener.Addr().String()+"/shutdown", nil)
		if reqErr != nil {
			requestErr <- reqErr
			return
		}
		req.Header.Set("Authorization", "Bearer "+testShutdownToken)
		resp, doErr := http.DefaultClient.Do(req)
		if doErr != nil {
			requestErr <- doErr
			return
		}
		response <- resp
	}()

	select {
	case resp := <-response:
		require.Equal(t, http.StatusAccepted, resp.StatusCode)
		require.NoError(t, resp.Body.Close())
	case err := <-requestErr:
		t.Fatalf("shutdown request failed: %v", err)
	case <-time.After(time.Second):
		t.Fatal("shutdown response waited for process stop completion")
	}

	select {
	case <-stopStarted:
	case <-time.After(time.Second):
		t.Fatal("process stop did not start")
	}

	releaseOnce.Do(func() { close(releaseStop) })
	select {
	case err := <-serveErr:
		require.True(t, errors.Is(err, http.ErrServerClosed), "Serve returned %v", err)
	case <-time.After(time.Second):
		t.Fatal("HTTP server did not complete shutdown")
	}
}

func TestShutdownServerStartsStopOnce(t *testing.T) {
	var stops atomic.Int32
	release := make(chan struct{})
	s := newShutdownTestServer(testShutdownToken, func() error {
		stops.Add(1)
		<-release
		return nil
	})

	const requests = 12
	var wg sync.WaitGroup
	for range requests {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/shutdown", nil)
			req.Header.Set("Authorization", "Bearer "+testShutdownToken)
			resp := httptest.NewRecorder()
			s.router.ServeHTTP(resp, req)
			assert.Equal(t, http.StatusAccepted, resp.Code)
		}()
	}
	wg.Wait()
	require.Eventually(t, func() bool { return stops.Load() == 1 }, time.Second, time.Millisecond)
	close(release)
}

func TestReadOnlyEndpointRemainsUnauthenticated(t *testing.T) {
	s := newShutdownTestServer(testShutdownToken, func() error { return nil })
	resp := httptest.NewRecorder()

	s.router.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/latest_height", nil))

	assert.Equal(t, http.StatusOK, resp.Code)
	assert.Equal(t, "0", strings.TrimSpace(resp.Body.String()))
}
