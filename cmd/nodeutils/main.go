package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	log "github.com/sirupsen/logrus"
	_ "go.uber.org/automaxprocs"

	"github.com/voluzi/cosmopilot/v3/pkg/environ"
	"github.com/voluzi/cosmopilot/v3/pkg/nodeutils"
)

var (
	host             string
	port             int
	dataPath         string
	upgradesConfig   string
	blockThreshold   time.Duration
	traceStore       string
	logLevel         string
	createFifo       bool
	enableTmkmsProxy bool
	signerPeerDNS    string
	nodeBinaryName   string
	haltHeight       int64
)

// subcommands are the standalone entry points this binary implements. They run in containers that
// mount none of the server's runtime configuration, so they must never reach startServer.
var subcommands = []string{"help", "mock", "wait-for-dns"}

// mockCommandArity is the single command contract used before mock dispatch. Keeping command
// recognition and exact arity together prevents validation from drifting from execution.
var mockCommandArity = map[string]int{
	"get":        1,
	"set-cpu":    2,
	"set-memory": 2,
}

// commands are the entry points run dispatches to. Tests replace them to assert which one a given
// argument list selects.
type commands struct {
	waitForDNS func([]string) error
	mock       func([]string)
	serve      func() error
}

func defaultCommands() commands {
	return commands{
		waitForDNS: handleWaitForDNSCommand,
		mock:       handleMockCommand,
		serve:      startServer,
	}
}

func main() {
	if err := run(os.Args[1:], defaultCommands()); err != nil {
		log.Fatal(err)
	}
}

// run selects a standalone subcommand before any server configuration is touched, and rejects a
// leading argument that is neither a flag nor a subcommand this binary implements.
//
// The rejection is the point: flag.Parse stops at the first non-flag argument and reports nothing,
// so an argument list this build does not recognise — a typo, or a subcommand emitted by a newer
// cosmopilot than the node-utils image it deployed — used to fall through to startServer. The
// containers that run subcommands mount no /config volume, so that fall-through surfaced as a
// missing default /config/upgrades.json instead of the argument that was never understood.
func run(args []string, cmds commands) error {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		switch args[0] {
		case "help":
			printHelp()
			return nil
		case "mock":
			if err := validateMockArgs(args[1:]); err != nil {
				return err
			}
			cmds.mock(args[1:])
			return nil
		case "wait-for-dns":
			return cmds.waitForDNS(args[1:])
		default:
			return fmt.Errorf("unknown subcommand %q: this node-utils build implements %s",
				args[0], strings.Join(subcommands, ", "))
		}
	}

	if len(args) > 0 && (args[0] == "--help" || args[0] == "-h") {
		printHelp()
		return nil
	}

	return cmds.serve()
}

// rejectPositionalArgs turns the arguments flag.Parse stopped at into an error, so a stray
// positional after the flags cannot be silently dropped either.
func rejectPositionalArgs(args []string) error {
	if len(args) == 0 {
		return nil
	}
	return fmt.Errorf("unexpected argument %q after flags: run `node-utils help`", args[0])
}

func validateMockArgs(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: node-utils mock <set-cpu <millicores>|set-memory <mib>|get>")
	}
	want, ok := mockCommandArity[args[0]]
	if !ok {
		return fmt.Errorf("unknown node-utils mock command %q", args[0])
	}
	if len(args) != want {
		return fmt.Errorf("invalid arguments for node-utils mock %s", args[0])
	}
	return nil
}

func startServer() error {
	flag.Parse()
	if err := rejectPositionalArgs(flag.Args()); err != nil {
		return err
	}

	if level, err := log.ParseLevel(logLevel); err == nil {
		log.SetLevel(level)
	}

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	nodeUtilsServer, err := nodeutils.New(
		nodeBinaryName,
		nodeutils.WithHost(host),
		nodeutils.WithPort(port),
		nodeutils.WithBlockThreshold(blockThreshold),
		nodeutils.WithDataPath(dataPath),
		nodeutils.WithUpgradesConfig(upgradesConfig),
		nodeutils.WithTraceStore(traceStore),
		nodeutils.CreateFifo(createFifo),
		nodeutils.WithTmkmsProxy(enableTmkmsProxy),
		nodeutils.WithSignerPeerDNS(signerPeerDNS),
		nodeutils.WithHaltHeight(haltHeight),
		nodeutils.WithMockMode(mockMode),
	)
	if err != nil {
		return err
	}

	go func() {
		sig := <-sigChan
		log.Infof("received signal: %v", sig)
		if err := nodeUtilsServer.Stop(false); err != nil {
			log.Errorf("failed to stop nodeutils server: %v", err)
		}
	}()

	return nodeUtilsServer.Start()
}

func printHelp() {
	fmt.Println(`node-utils - Node utilities sidecar for cosmopilot

Usage:
  node-utils [flags]           Start the node-utils server
  node-utils mock <command>    Control mock mode (use from kubectl exec)
  node-utils wait-for-dns <hostname> <ip-address> <timeout>
                               Wait until DNS publishes an address
  node-utils help              Show this help

Mock Commands (for E2E testing):
  node-utils mock set-cpu <millicores>     Set mock CPU usage (e.g., 500 for 500m)
  node-utils mock set-memory <mib>         Set mock memory usage in MiB (e.g., 512)
  node-utils mock get                      Get current mock stats

Flags:`)
	flag.PrintDefaults()
}

// handleMockCommand processes mock subcommands for controlling mock mode via CLI.
// This is useful for E2E tests running kubectl exec against distroless containers.
func handleMockCommand(args []string) {
	if len(args) == 0 {
		fmt.Println("Usage: node-utils mock <command>")
		fmt.Println("Commands: set-cpu <millicores>, set-memory <mib>, get")
		os.Exit(1)
	}

	// Get port from environment variable or use default (8000)
	// Note: We can't use the `port` flag variable here because flag.Parse() hasn't been called yet
	mockPort := environ.GetInt("PORT", 8000)

	baseURL := fmt.Sprintf("http://localhost:%d", mockPort)

	switch args[0] {
	case "set-cpu":
		url := fmt.Sprintf("%s/mock/cpu?millicores=%s", baseURL, args[1])
		resp, err := http.Post(url, "text/plain", bytes.NewBuffer(nil))
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			fmt.Fprintf(os.Stderr, "Error: %s\n", body)
			os.Exit(1)
		}
		fmt.Printf("CPU set to %s millicores\n", args[1])

	case "set-memory":
		url := fmt.Sprintf("%s/mock/memory?mib=%s", baseURL, args[1])
		fmt.Printf("DEBUG: POST %s\n", url)
		resp, err := http.Post(url, "text/plain", bytes.NewBuffer(nil))
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error making request to %s: %v\n", url, err)
			os.Exit(1)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		fmt.Printf("DEBUG: Response status=%d body=%s\n", resp.StatusCode, string(body))
		if resp.StatusCode != http.StatusOK {
			fmt.Fprintf(os.Stderr, "Error: %s\n", body)
			os.Exit(1)
		}
		fmt.Printf("Memory set to %s MiB\n", args[1])

	case "get":
		url := fmt.Sprintf("%s/mock/stats", baseURL)
		resp, err := http.Get(url)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: %v\n", err)
			os.Exit(1)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			fmt.Fprintf(os.Stderr, "Error: %s\n", body)
			os.Exit(1)
		}
		fmt.Println(string(body))

	default:
		fmt.Printf("Unknown mock command: %s\n", args[0])
		fmt.Println("Commands: set-cpu <millicores>, set-memory <mib>, get")
		os.Exit(1)
	}
}
