package main

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	dnsLookupInterval     = time.Second
	dnsDiagnosticInterval = 5 * time.Second
)

type dnsResolver interface {
	LookupIPAddr(context.Context, string) ([]net.IPAddr, error)
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
