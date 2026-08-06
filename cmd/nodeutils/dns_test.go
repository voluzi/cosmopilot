package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
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

type sequenceHTTPDoer struct {
	statuses []int
	errors   []error
	calls    int
}

func (d *sequenceHTTPDoer) Do(*http.Request) (*http.Response, error) {
	index := d.calls
	d.calls++
	if index < len(d.errors) && d.errors[index] != nil {
		return nil, d.errors[index]
	}
	status := http.StatusNotAcceptable
	if index < len(d.statuses) {
		status = d.statuses[index]
	}
	return &http.Response{StatusCode: status, Status: http.StatusText(status), Body: io.NopCloser(strings.NewReader(""))}, nil
}

type deadlineDNSResolver struct {
	deadline    time.Time
	hasDeadline bool
}

func (r *deadlineDNSResolver) LookupIPAddr(ctx context.Context, _ string) ([]net.IPAddr, error) {
	r.deadline, r.hasDeadline = ctx.Deadline()
	return []net.IPAddr{{IP: net.ParseIP("10.0.0.2")}}, nil
}

func TestWaitForDNSCommandAppliesTimeoutArgument(t *testing.T) {
	resolver := &deadlineDNSResolver{}
	started := time.Now()

	err := runWaitForDNSCommand(context.Background(), resolver, &sequenceHTTPDoer{statuses: []int{http.StatusNotAcceptable, http.StatusOK}},
		[]string{"signer-privval.default.svc", "10.0.0.2", "25s"}, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !resolver.hasDeadline {
		t.Fatal("resolver context has no deadline")
	}
	remaining := resolver.deadline.Sub(started)
	if remaining <= 20*time.Second || remaining > 26*time.Second {
		t.Fatalf("resolver deadline remaining = %s, want a 25s command timeout", remaining)
	}
}

func TestWaitForDNSCommandAcceptsSignerConfirmationWhileLocalDNSIsStale(t *testing.T) {
	resolver := &sequenceDNSResolver{
		responses: [][]net.IPAddr{{{IP: net.ParseIP("10.0.0.1")}}},
	}
	client := &sequenceHTTPDoer{statuses: []int{http.StatusOK}}

	err := runWaitForDNSCommand(context.Background(), resolver, client,
		[]string{"signer-privval.default.svc", "10.0.0.2", "25s"}, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if client.calls != 1 {
		t.Fatalf("signer discovery calls = %d, want 1", client.calls)
	}
}

// TestWaitForDNSCommandRequiresSignerConfirmationDespiteLocalDNSPublication is the converse of the
// stale-DNS case: this Pod resolving its own published address only proves its own DNS cache caught
// up. The signer reads a different cache, so local publication is diagnostic and an authenticated
// signer connection stays mandatory.
func TestWaitForDNSCommandRequiresSignerConfirmationDespiteLocalDNSPublication(t *testing.T) {
	resolver := &sequenceDNSResolver{
		responses: [][]net.IPAddr{{{IP: net.ParseIP("10.0.0.2")}}},
	}
	client := &sequenceHTTPDoer{errors: []error{errors.New("node-utils connection refused")}}

	err := runWaitForDNSCommand(context.Background(), resolver, client,
		[]string{"signer-privval.default.svc", "10.0.0.2", "20ms"}, time.Millisecond)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runWaitForDNSCommand() error = %v, want deadline exceeded", err)
	}
	for _, want := range []string{"signer connection", "pod-local DNS: address published"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("runWaitForDNSCommand() error = %v, want it to report %q", err, want)
		}
	}
}

func TestWaitForDNSCommandRejectsInvalidPodAddress(t *testing.T) {
	// An unexpanded or unset downward-API address is a pod-spec problem: report it now instead of
	// spending the whole discovery timeout on it.
	for _, address := range []string{"", "$(POD_IP)", "not-an-ip"} {
		t.Run(address, func(t *testing.T) {
			err := runWaitForDNSCommand(context.Background(), &sequenceDNSResolver{}, &sequenceHTTPDoer{},
				[]string{"signer-privval.default.svc", address, "20ms"}, time.Millisecond)
			if err == nil || !strings.Contains(err.Error(), "invalid IP address") {
				t.Fatalf("runWaitForDNSCommand() error = %v, want invalid-address error", err)
			}
		})
	}
}

func TestWaitForDNSCommandRejectsWrongArgumentCount(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "no arguments"},
		{name: "missing timeout", args: []string{"signer-privval.default.svc", "10.0.0.2"}},
		{name: "extra argument", args: []string{"signer-privval.default.svc", "10.0.0.2", "25s", "extra"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := runWaitForDNSCommand(context.Background(), &sequenceDNSResolver{}, &sequenceHTTPDoer{}, tt.args, time.Millisecond)
			if err == nil || !strings.Contains(err.Error(), "usage: node-utils wait-for-dns") {
				t.Fatalf("runWaitForDNSCommand() error = %v, want usage error", err)
			}
		})
	}
}

func TestWaitForDNSCommandRejectsInvalidTimeout(t *testing.T) {
	for _, timeout := range []string{"not-a-duration", "0s", "-1s"} {
		t.Run(timeout, func(t *testing.T) {
			err := runWaitForDNSCommand(context.Background(), &sequenceDNSResolver{}, &sequenceHTTPDoer{},
				[]string{"signer-privval.default.svc", "10.0.0.2", timeout}, time.Millisecond)
			if err == nil || !strings.Contains(err.Error(), "invalid DNS wait timeout") {
				t.Fatalf("runWaitForDNSCommand() error = %v, want invalid-timeout error", err)
			}
		})
	}
}

func TestWaitForDNSCommandTimesOutWithDiscoveryDiagnostics(t *testing.T) {
	client := &sequenceHTTPDoer{
		statuses: []int{0, http.StatusNotAcceptable},
		errors:   []error{errors.New("node-utils connection refused")},
	}
	resolver := &sequenceDNSResolver{responses: [][]net.IPAddr{{{IP: net.ParseIP("10.0.0.1")}}}}
	started := time.Now()
	err := runWaitForDNSCommand(context.Background(), resolver, client,
		[]string{"signer-privval.default.svc", "10.0.0.2", "20ms"}, time.Millisecond)

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("runWaitForDNSCommand() error = %v, want deadline exceeded", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("runWaitForDNSCommand() took %s, want bounded timeout", elapsed)
	}
	// The signer never confirmed, so the diagnostic lookup must be reported alongside it: whether
	// the address was published locally is exactly what separates a signer that never dialed from a
	// Service that never published this Pod.
	for _, want := range []string{
		"signer connection",
		"last status: Not Acceptable",
		"last error: node-utils connection refused",
		"pod-local DNS",
		"observed addresses: 10.0.0.1",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("runWaitForDNSCommand() error = %v, want it to report %q", err, want)
		}
	}
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

func TestWaitForSignerDiscoveryRetriesUntilConfirmed(t *testing.T) {
	client := &sequenceHTTPDoer{
		errors:   []error{errors.New("node-utils not ready")},
		statuses: []int{0, http.StatusNotAcceptable, http.StatusOK},
	}
	err := waitForSignerDiscovery(context.Background(), client, signerDiscoveryURL, time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if client.calls != 3 {
		t.Fatalf("signer discovery calls = %d, want 3", client.calls)
	}
}
