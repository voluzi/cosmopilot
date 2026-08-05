package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/voluzi/cosmopilot/v2/pkg/nodeutils"
)

// testCommands fails the test if the node-utils server is ever started. Standalone subcommands run
// in containers that mount none of the server's runtime configuration, so any argument list that
// reaches startServer fails on that missing configuration rather than on the arguments themselves.
func testCommands(t *testing.T) commands {
	t.Helper()
	return commands{
		waitForDNS: func([]string) error {
			t.Fatal("run() dispatched wait-for-dns unexpectedly")
			return nil
		},
		mock: func([]string) { t.Fatal("run() dispatched the mock command unexpectedly") },
		serve: func() error {
			t.Fatalf("run() reached node-utils server startup, which requires %s", nodeutils.DefaultUpgradesConfig)
			return nil
		},
	}
}

// requireNoServerConfiguration asserts the premise the wait-for-dns tests rely on: the default
// upgrades config the server would load is genuinely absent, exactly as it is in the generated
// discovery gate container.
func requireNoServerConfiguration(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(nodeutils.DefaultUpgradesConfig); err == nil {
		t.Skipf("%s exists in this environment; cannot assert the no-config guarantee", nodeutils.DefaultUpgradesConfig)
	}
}

func TestRunWaitForDNSRunsWithoutServerConfiguration(t *testing.T) {
	requireNoServerConfiguration(t)

	var forwarded []string
	cmds := testCommands(t)
	cmds.waitForDNS = func(args []string) error {
		forwarded = append([]string(nil), args...)
		// The command is dispatched directly without touching server configuration. Simulate the
		// authenticated signer confirmation that releases the gate.
		return runWaitForDNSCommand(
			context.Background(),
			&sequenceDNSResolver{responses: [][]net.IPAddr{{{IP: net.ParseIP("10.0.0.2")}}}},
			&sequenceHTTPDoer{statuses: []int{http.StatusOK}},
			args,
			time.Millisecond,
		)
	}

	if err := run([]string{"wait-for-dns", "signer-privval.default.svc", "10.0.0.2", "25s"}, cmds); err != nil {
		t.Fatal(err)
	}
	want := []string{"signer-privval.default.svc", "10.0.0.2", "25s"}
	if !reflect.DeepEqual(forwarded, want) {
		t.Fatalf("wait-for-dns arguments = %q, want %q", forwarded, want)
	}
}

// TestRunRejectsUnknownSubcommand covers the failure this binary used to report as a missing
// /config/upgrades.json: flag.Parse stops at the first non-flag argument without reporting it, so an
// unrecognized subcommand fell through to server startup. It must name the argument instead.
func TestRunRejectsUnknownSubcommand(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{name: "subcommand from a newer operator", args: []string{"wait-for-signer", "signer-privval.default.svc", "10.0.0.2", "25s"}},
		{name: "misspelled subcommand", args: []string{"wait-for-dnss", "signer-privval.default.svc", "10.0.0.2", "25s"}},
		{name: "bare argument", args: []string{"serve"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := run(tt.args, testCommands(t))
			if err == nil {
				t.Fatal("run() accepted an unknown subcommand")
			}
			if !strings.Contains(err.Error(), "unknown subcommand") || !strings.Contains(err.Error(), tt.args[0]) {
				t.Fatalf("run() error = %v, want it to name the unknown subcommand", err)
			}
			for _, name := range subcommands {
				if !strings.Contains(err.Error(), name) {
					t.Fatalf("run() error = %v, want it to list the supported subcommand %q", err, name)
				}
			}
		})
	}
}

func TestRunRejectsMalformedMockArguments(t *testing.T) {
	for _, args := range [][]string{
		{"mock"},
		{"mock", "get", "extra"},
		{"mock", "set-cpu"},
		{"mock", "set-cpu", "500", "extra"},
		{"mock", "set-memory"},
		{"mock", "set-memory", "512", "extra"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			err := run(args, testCommands(t))
			if err == nil || !strings.Contains(err.Error(), "node-utils mock") {
				t.Fatalf("run(%q) error = %v, want mock usage error", args, err)
			}
		})
	}
}

func TestRunDispatchesStandaloneSubcommands(t *testing.T) {
	t.Run("mock", func(t *testing.T) {
		var forwarded []string
		cmds := testCommands(t)
		cmds.mock = func(args []string) { forwarded = append([]string(nil), args...) }

		if err := run([]string{"mock", "set-cpu", "500"}, cmds); err != nil {
			t.Fatal(err)
		}
		if want := []string{"set-cpu", "500"}; !reflect.DeepEqual(forwarded, want) {
			t.Fatalf("mock arguments = %q, want %q", forwarded, want)
		}
	})

	for _, args := range [][]string{{"help"}, {"--help"}, {"-h"}} {
		t.Run(args[0], func(t *testing.T) {
			if err := run(args, testCommands(t)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRunStartsServerWithoutSubcommand(t *testing.T) {
	for _, tt := range []struct {
		name string
		args []string
	}{
		{name: "no arguments"},
		{name: "flags only", args: []string{"-log-level", "debug"}},
		{name: "long flags only", args: []string{"--tmkms-proxy=true"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			started := false
			cmds := testCommands(t)
			cmds.serve = func() error {
				started = true
				return nil
			}

			if err := run(tt.args, cmds); err != nil {
				t.Fatal(err)
			}
			if !started {
				t.Fatal("run() did not start the node-utils server")
			}
		})
	}
}

func TestRejectPositionalArgs(t *testing.T) {
	if err := rejectPositionalArgs(nil); err != nil {
		t.Fatalf("rejectPositionalArgs(nil) = %v, want nil", err)
	}
	err := rejectPositionalArgs([]string{"wait-for-dns", "signer-privval.default.svc"})
	if err == nil || !strings.Contains(err.Error(), "wait-for-dns") {
		t.Fatalf("rejectPositionalArgs() error = %v, want it to name the leftover argument", err)
	}
}
