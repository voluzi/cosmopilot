package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestPprofMuxOnlyServesDiagnosticRoutes(t *testing.T) {
	previousDefaultMux := http.DefaultServeMux
	http.DefaultServeMux = http.NewServeMux()
	t.Cleanup(func() { http.DefaultServeMux = previousDefaultMux })
	defaultRoute := "/pprof-default-mux-sentinel"
	http.DefaultServeMux.HandleFunc(defaultRoute, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})

	mux := newPprofMux()
	for _, path := range []string{
		"/debug/pprof/", "/debug/pprof/allocs", "/debug/pprof/block",
		"/debug/pprof/goroutine", "/debug/pprof/heap", "/debug/pprof/mutex",
		"/debug/pprof/threadcreate", "/debug/pprof/cmdline",
		"/debug/pprof/profile?seconds=1", "/debug/pprof/symbol",
		"/debug/pprof/trace?seconds=0.01",
	} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			t.Run(method+" "+path, func(t *testing.T) {
				response := httptest.NewRecorder()
				mux.ServeHTTP(response, httptest.NewRequest(method, path, nil))
				if response.Code != http.StatusOK {
					t.Fatalf("%s %s = %d, want 200", method, path, response.Code)
				}
			})
		}
	}

	for _, path := range []string{"/health", "/data_size", "/shutdown", "/not-found", defaultRoute} {
		t.Run("GET "+path, func(t *testing.T) {
			response := httptest.NewRecorder()
			mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
			if response.Code != http.StatusNotFound {
				t.Fatalf("GET %s = %d, want 404", path, response.Code)
			}
		})
	}

	for _, path := range []string{
		"/debug/pprof/", "/debug/pprof/heap", "/debug/pprof/profile",
		"/debug/pprof/trace", "/debug/pprof/cmdline", "/debug/pprof/symbol",
	} {
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodOptions, http.MethodPatch} {
			t.Run(method+" "+path, func(t *testing.T) {
				response := httptest.NewRecorder()
				mux.ServeHTTP(response, httptest.NewRequest(method, path, nil))
				if response.Code != http.StatusMethodNotAllowed {
					t.Fatalf("%s %s = %d, want 405", method, path, response.Code)
				}
				if got := response.Header().Get("Allow"); got != "GET, HEAD" {
					t.Fatalf("%s %s Allow = %q, want %q", method, path, got, "GET, HEAD")
				}
			})
		}
	}
}

func TestPprofMuxCompatibility(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"0", "1"} {
		t.Run("httpmuxgo121="+mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, executable, "-test.run=^TestPprofMuxOnlyServesDiagnosticRoutes$", "-test.count=1")
			var environment []string
			var godebug []string
			for _, variable := range os.Environ() {
				if !strings.HasPrefix(variable, "GODEBUG=") {
					environment = append(environment, variable)
					continue
				}
				for _, option := range strings.Split(strings.TrimPrefix(variable, "GODEBUG="), ",") {
					if option != "" && !strings.HasPrefix(option, "httpmuxgo121=") {
						godebug = append(godebug, option)
					}
				}
			}
			command.Env = append(environment, "GODEBUG="+strings.Join(append(godebug, "httpmuxgo121="+mode), ","))
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("diagnostic routes with httpmuxgo121=%s: %v\n%s", mode, err, output)
			}
		})
	}
}

func TestPprofPortCollisionDoesNotStopMainServer(t *testing.T) {
	held, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()

	want := errors.New("main server error")
	called := false
	got := runWithPprof(held.Addr().String(), 0, func() error {
		called = true
		return want
	})
	if !called || !errors.Is(got, want) {
		t.Fatalf("callback called = %v, error = %v; want callback and %v", called, got, want)
	}
	if err := held.(*net.TCPListener).SetDeadline(time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := held.Accept(); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("held listener was modified or closed: %v", err)
	}
}

func TestPrimaryServerKeepsConfiguredPprofPort(t *testing.T) {
	address := unusedLoopbackAddress(t)
	_, portText, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	primaryPort, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatal(err)
	}
	got := runWithPprof(address, primaryPort, func() error {
		listener, err := net.Listen("tcp", address)
		if err != nil {
			return err
		}
		return listener.Close()
	})
	if got != nil {
		t.Fatalf("primary server could not bind %s: %v", address, got)
	}
}

func TestPprofListenerClosesWhenMainServerReturns(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
	}{
		{name: "success"},
		{name: "failure", err: errors.New("main server error")},
	} {
		t.Run(tt.name, func(t *testing.T) {
			address := unusedLoopbackAddress(t)
			got := runWithPprof(address, 0, func() error { return tt.err })
			if !errors.Is(got, tt.err) {
				t.Fatalf("runWithPprof() = %v, want %v", got, tt.err)
			}
			listener, err := net.Listen("tcp", address)
			if err != nil {
				t.Fatalf("profiling listener remained bound: %v", err)
			}
			listener.Close()
		})
	}
}

func TestPprofServesProfilesWhileMainServerRuns(t *testing.T) {
	address := unusedLoopbackAddress(t)
	client := &http.Client{Timeout: 3 * time.Second}
	for _, path := range []string{"/debug/pprof/heap", "/debug/pprof/profile?seconds=1", "/debug/pprof/trace?seconds=0.01"} {
		t.Run(path, func(t *testing.T) {
			got := runWithPprof(address, 0, func() error {
				response, err := client.Get("http://" + address + path)
				if err != nil {
					return err
				}
				defer response.Body.Close()
				if response.StatusCode != http.StatusOK {
					t.Errorf("GET %s = %d, want 200", path, response.StatusCode)
				}
				profile, err := io.ReadAll(response.Body)
				if err != nil {
					return err
				}
				if len(profile) == 0 {
					t.Error("empty profile")
				}
				if strings.HasPrefix(path, "/debug/pprof/trace") {
					return nil
				}
				reader, err := gzip.NewReader(bytes.NewReader(profile))
				if err != nil {
					return err
				}
				defer reader.Close()
				decoded, err := io.ReadAll(reader)
				if err != nil {
					return err
				}
				if len(decoded) == 0 {
					t.Error("empty profile")
				}
				return nil
			})
			if got != nil {
				t.Fatal(got)
			}
		})
	}
}

func unusedLoopbackAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return address
}
