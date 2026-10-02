package main

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

type sequenceDNSResolver struct {
	responses [][]net.IPAddr
	errors    []error
	calls     int
}

func (r *sequenceDNSResolver) LookupIPAddr(context.Context, string) ([]net.IPAddr, error) {
	index := r.calls
	r.calls++
	if index < len(r.errors) && r.errors[index] != nil {
		return nil, r.errors[index]
	}
	if index < len(r.responses) {
		return r.responses[index], nil
	}
	return nil, nil
}

func TestWaitForDNSAddressRetriesUntilTargetIsPublished(t *testing.T) {
	resolver := &sequenceDNSResolver{
		responses: [][]net.IPAddr{
			nil,
			{{IP: net.ParseIP("10.0.0.1")}},
			{{IP: net.ParseIP("10.0.0.2")}},
		},
		errors: []error{errors.New("not published yet")},
	}

	err := waitForDNSAddress(context.Background(), resolver, "signer-privval.default.svc", "10.0.0.2", time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if resolver.calls != 3 {
		t.Fatalf("DNS lookup calls = %d, want 3", resolver.calls)
	}
}

func TestWaitForDNSAddressStopsWhenContextIsCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	resolver := &sequenceDNSResolver{}

	err := waitForDNSAddress(ctx, resolver, "signer-privval.default.svc", "10.0.0.2", time.Millisecond)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("waitForDNSAddress() error = %v, want context cancellation", err)
	}
	if resolver.calls != 0 {
		t.Fatalf("DNS lookup calls = %d, want 0 after cancellation", resolver.calls)
	}
}

func TestWaitForDNSAddressTimeoutReportsLastLookupState(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	resolver := &sequenceDNSResolver{
		responses: [][]net.IPAddr{{{IP: net.ParseIP("10.0.0.1")}}},
	}

	err := waitForDNSAddress(ctx, resolver, "signer-privval.default.svc", "10.0.0.2", time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waitForDNSAddress() error = %v, want deadline exceeded", err)
	}
	if !strings.Contains(err.Error(), "observed addresses: 10.0.0.1") {
		t.Fatalf("waitForDNSAddress() error = %v, want last observed address", err)
	}
}

func TestWaitForDNSAddressRejectsInvalidTarget(t *testing.T) {
	err := waitForDNSAddress(context.Background(), &sequenceDNSResolver{}, "signer-privval.default.svc", "not-an-ip", time.Millisecond)
	if err == nil {
		t.Fatal("waitForDNSAddress() expected an invalid-address error")
	}
}
