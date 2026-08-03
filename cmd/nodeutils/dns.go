package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os/signal"
	"strings"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	dnsLookupInterval     = time.Second
	dnsDiagnosticInterval = 5 * time.Second
	signerDiscoveryURL    = "http://127.0.0.1:8000/signer_discovered"
)

type dnsResolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
}

func handleWaitForDNSCommand(args []string) error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return runWaitForDNSCommand(ctx, net.DefaultResolver, http.DefaultClient, args, dnsLookupInterval)
}

type httpDoer interface {
	Do(*http.Request) (*http.Response, error)
}

func runWaitForDNSCommand(ctx context.Context, resolver dnsResolver, client httpDoer, args []string, interval time.Duration) error {
	if len(args) != 3 {
		return fmt.Errorf("usage: node-utils wait-for-dns <hostname> <ip-address> <timeout>")
	}
	timeout, err := time.ParseDuration(args[2])
	if err != nil || timeout <= 0 {
		return fmt.Errorf("invalid DNS wait timeout %q", args[2])
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// An inbound signer connection is definitive evidence that the signer has
	// discovered this Pod. Keep the Pod-local DNS lookup running for diagnostics,
	// but do not make it a prerequisite: the target and signer may use different
	// DNS caches, and either cache can observe publication first.
	dnsCtx, stopDNS := context.WithCancel(ctx)
	defer stopDNS()
	dnsResult := make(chan error, 1)
	go func() {
		dnsResult <- waitForDNSAddress(dnsCtx, resolver, args[0], args[1], interval)
	}()

	if err := waitForSignerDiscovery(ctx, client, signerDiscoveryURL, interval); err != nil {
		select {
		case dnsErr := <-dnsResult:
			if dnsErr != nil && !errors.Is(dnsErr, context.Canceled) {
				return fmt.Errorf("%w; pod-local DNS observation: %v", err, dnsErr)
			}
		default:
		}
		return err
	}
	return nil
}

func waitForDNSAddress(ctx context.Context, resolver dnsResolver, hostname, address string, interval time.Duration) error {
	target := net.ParseIP(address)
	if target == nil {
		return fmt.Errorf("invalid IP address %q", address)
	}
	if interval <= 0 {
		return fmt.Errorf("DNS lookup interval must be positive")
	}

	lookupTicker := time.NewTicker(interval)
	defer lookupTicker.Stop()
	diagnosticTicker := time.NewTicker(dnsDiagnosticInterval)
	defer diagnosticTicker.Stop()

	var lastLookupErr error
	var lastObservedAddresses []net.IPAddr
lookupLoop:
	for {
		if err := ctx.Err(); err != nil {
			return dnsWaitError(hostname, address, lastLookupErr, lastObservedAddresses, err)
		}

		addresses, err := resolver.LookupIPAddr(ctx, hostname)
		if err != nil {
			lastLookupErr = err
		} else {
			if len(addresses) > 0 {
				lastObservedAddresses = append(lastObservedAddresses[:0], addresses...)
			}
			for _, candidate := range addresses {
				if target.Equal(candidate.IP) {
					return nil
				}
			}
		}

		for {
			select {
			case <-ctx.Done():
				return dnsWaitError(hostname, address, lastLookupErr, lastObservedAddresses, ctx.Err())
			case <-diagnosticTicker.C:
				log.WithFields(log.Fields{
					"hostname":           hostname,
					"target_address":     address,
					"last_lookup_error":  formatDNSLookupError(lastLookupErr),
					"observed_addresses": formatDNSAddresses(lastObservedAddresses),
				}).Info("waiting for DNS address publication")
			case <-lookupTicker.C:
				continue lookupLoop
			}
		}
	}
}

func dnsWaitError(hostname, address string, lastLookupErr error, observed []net.IPAddr, cause error) error {
	return fmt.Errorf("waiting for %s to publish %s (last lookup error: %s; observed addresses: %s): %w",
		hostname, address, formatDNSLookupError(lastLookupErr), formatDNSAddresses(observed), cause)
}

func formatDNSLookupError(err error) string {
	if err == nil {
		return "none"
	}
	return err.Error()
}

func formatDNSAddresses(addresses []net.IPAddr) string {
	if len(addresses) == 0 {
		return "none"
	}
	formatted := make([]string, 0, len(addresses))
	for _, address := range addresses {
		formatted = append(formatted, address.String())
	}
	return strings.Join(formatted, ",")
}

func waitForSignerDiscovery(ctx context.Context, client httpDoer, url string, interval time.Duration) error {
	if interval <= 0 {
		return fmt.Errorf("signer discovery poll interval must be positive")
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var lastStatus string
	var lastErr error
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err
		} else {
			lastStatus = resp.Status
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for remote signer discovery confirmation (last status: %s; last error: %s): %w",
				formatDiagnosticValue(lastStatus), formatDNSLookupError(lastErr), ctx.Err())
		case <-ticker.C:
		}
	}
}

func formatDiagnosticValue(value string) string {
	if value == "" {
		return "none"
	}
	return value
}
