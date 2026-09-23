package main

import (
	"errors"
	"net"
	"net/http"
	"net/http/pprof"
	"strconv"
	"time"

	log "github.com/sirupsen/logrus"
)

const pprofAddress = "127.0.0.1:6666"

func newPprofMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprofGetOrHead(pprof.Index))
	mux.HandleFunc("/debug/pprof/cmdline", pprofGetOrHead(pprof.Cmdline))
	mux.HandleFunc("/debug/pprof/profile", pprofGetOrHead(pprof.Profile))
	mux.HandleFunc("/debug/pprof/symbol", pprofGetOrHead(pprof.Symbol))
	mux.HandleFunc("/debug/pprof/trace", pprofGetOrHead(pprof.Trace))
	return mux
}

func pprofGetOrHead(handler http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
			return
		}
		handler(w, r)
	}
}

func runWithPprof(address string, primaryPort int, serve func() error) error {
	if _, pprofPort, err := net.SplitHostPort(address); err == nil && pprofPort == strconv.Itoa(primaryPort) {
		log.Warnf("pprof unavailable at %s: port reserved for primary API", address)
		return serve()
	}

	listener, err := net.Listen("tcp", address)
	if err != nil {
		log.Warnf("pprof listener unavailable at %s: %v", address, err)
		return serve()
	}

	server := &http.Server{
		Handler:           newPprofMux(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	served := make(chan struct{})
	go func() {
		defer close(served)
		if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Errorf("pprof listener stopped: %v", err)
		}
	}()
	defer func() {
		if err := server.Close(); err != nil {
			log.Warnf("close pprof listener: %v", err)
		}
		<-served
	}()

	return serve()
}
