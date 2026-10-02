package main

import (
	"context"
	"fmt"
	"net"
	"os/signal"
	"strconv"
	"syscall"
	"time"
)

func handleWaitForSignerCommand(args []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return runWaitForSignerCommand(ctx, net.DefaultResolver, args, dnsLookupInterval)
}

func runWaitForSignerCommand(ctx context.Context, resolver dnsResolver, args []string, interval time.Duration) error {
	if len(args) != 4 {
		return fmt.Errorf("usage: node-utils wait-for-signer <port> <hostname> <ip-address> <timeout>")
	}
	port, err := strconv.Atoi(args[0])
	if err != nil || port < 0 || port > 65535 {
		return fmt.Errorf("invalid signer port %q", args[0])
	}
	if net.ParseIP(args[2]) == nil {
		return fmt.Errorf("invalid IP address %q", args[2])
	}
	timeout, err := time.ParseDuration(args[3])
	if err != nil || timeout <= 0 {
		return fmt.Errorf("invalid signer wait timeout %q", args[3])
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", net.JoinHostPort("", args[0]))
	if err != nil {
		return fmt.Errorf("listen for signer: %w", err)
	}
	defer func() { _ = listener.Close() }()
	diagnosticCtx, stopDiagnostic := context.WithCancel(ctx)
	defer stopDiagnostic()
	dnsObservations := make(chan error, 1)
	go func() {
		dnsObservations <- waitForDNSAddress(diagnosticCtx, resolver, args[1], args[2], interval)
	}()
	signerErr := waitForSignerConnection(ctx, listener)
	stopDiagnostic()
	dnsErr := <-dnsObservations
	if signerErr == nil {
		return nil
	}
	dnsStatus := "address published"
	if dnsErr != nil {
		dnsStatus = dnsErr.Error()
	}
	return fmt.Errorf("signer connection was not confirmed (pod-local DNS: %s): %w", dnsStatus, signerErr)
}

func waitForSignerConnection(ctx context.Context, listener net.Listener) error {
	defer func() { _ = listener.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stop()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("accept signer connection: %w", err)
		}
		stopRead := context.AfterFunc(ctx, func() { _ = conn.Close() })
		// A bare TCP probe must not release the gate or hold the accept loop indefinitely.
		if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			stopRead()
			_ = conn.Close()
			return fmt.Errorf("set signer read deadline: %w", err)
		}
		var data [1]byte
		n, _ := conn.Read(data[:])
		stopRead()
		_ = conn.Close()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if n > 0 {
			return nil
		}
	}
}
