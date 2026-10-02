package main

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

func TestWaitForSignerReleasesOnInboundConnection(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- waitForSignerConnection(ctx, listener) }()
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("gate left the signer connection open")
	}
	if conn, err := net.DialTimeout("tcp", listener.Addr().String(), 100*time.Millisecond); err == nil {
		conn.Close()
		t.Fatal("gate left its listener open")
	}
}

func TestWaitForSignerIgnoresConnectionWithoutData(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- waitForSignerConnection(ctx, listener) }()
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-result:
		t.Fatalf("bare connection released gate: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	conn.Close()
	conn, err = net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	if err := <-result; err != nil {
		t.Fatal(err)
	}
}

func TestWaitForSignerTimesOutWithDNSDiagnostics(t *testing.T) {
	resolver := &sequenceDNSResolver{errors: []error{errors.New("DNS unavailable")}}
	err := runWaitForSignerCommand(context.Background(), resolver,
		[]string{"0", "signer-privval.default.svc", "10.0.0.2", "20ms"}, time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want timeout, got %v", err)
	}
	for _, want := range []string{"signer connection", "pod-local DNS", "DNS unavailable", "10.0.0.2"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %v does not report %q", err, want)
		}
	}
}

func TestWaitForSignerCancellationClosesIdleConnection(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- waitForSignerConnection(ctx, listener) }()
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("want cancellation, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("idle connection blocked cancellation")
	}
}
