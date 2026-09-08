package nodeutils

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
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

func TestShutdownServerFlushesAcknowledgementBeforeProcessExit(t *testing.T) {
	reservation, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	address := reservation.Addr().String()
	require.NoError(t, reservation.Close())

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestShutdownServerProcessHelper$", "-test.count=1")
	cmd.Env = append(os.Environ(),
		"NODEUTILS_SHUTDOWN_HELPER=1",
		"NODEUTILS_SHUTDOWN_HELPER_ADDR="+address,
	)
	stdout, err := cmd.StdoutPipe()
	require.NoError(t, err)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})

	ready, err := bufio.NewReader(stdout).ReadString('\n')
	if err != nil {
		_ = cmd.Process.Kill()
		waitErr := cmd.Wait()
		t.Fatalf("wait for helper readiness: %v; process wait: %v; stderr: %s", err, waitErr, stderr.String())
	}
	require.Equal(t, "ready\n", ready)

	conn, err := net.Dial("tcp", address)
	require.NoError(t, err)
	defer conn.Close()
	_, err = fmt.Fprintf(conn,
		"POST /shutdown HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nContent-Length: 10\r\n\r\n",
		address, testShutdownToken,
	)
	require.NoError(t, err)

	waitErr := cmd.Wait()
	require.NoError(t, waitErr, stderr.String())
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(time.Second)))
	rawResponse, err := io.ReadAll(conn)
	require.NoError(t, err)
	request, err := http.NewRequest(http.MethodPost, "http://"+address+"/shutdown", nil)
	require.NoError(t, err)
	response, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(rawResponse)), request)
	require.NoError(t, err, "raw response was %q", rawResponse)
	defer response.Body.Close()
	assert.Equal(t, http.StatusAccepted, response.StatusCode)
}

func TestShutdownServerProcessHelper(t *testing.T) {
	if os.Getenv("NODEUTILS_SHUTDOWN_HELPER") != "1" {
		return
	}
	listener, err := net.Listen("tcp", os.Getenv("NODEUTILS_SHUTDOWN_HELPER_ADDR"))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	s := &NodeUtils{
		cfg:      &Options{ShutdownToken: testShutdownToken},
		router:   mux.NewRouter(),
		stopNode: func() error { return nil },
	}
	s.registerRoutes()
	s.server = &http.Server{Handler: s.router}
	fmt.Fprintln(os.Stdout, "ready")
	if err := s.server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(3)
	}
	os.Exit(0)
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
