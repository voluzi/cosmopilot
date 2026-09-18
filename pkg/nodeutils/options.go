package nodeutils

import "time"

const (
	// DefaultPort is the default port for the node-utils HTTP server.
	DefaultPort = 8000

	// DefaultHost is the default host for the node-utils HTTP server.
	DefaultHost = "0.0.0.0"

	// DefaultDataPath is the default path to the node's data directory.
	DefaultDataPath = "/home/app/data"

	// DefaultUpgradesConfig is the default path to the upgrades configuration file.
	DefaultUpgradesConfig = "/config/upgrades.json"
)

func defaultOptions() *Options {
	return &Options{
		DataPath:       DefaultDataPath,
		Host:           DefaultHost,
		Port:           DefaultPort,
		BlockThreshold: 0,
		UpgradesConfig: DefaultUpgradesConfig,
		TmkmsProxy:     false,
		SignerPeerDNS:  "",
		HaltHeight:     0,
	}
}

type Options struct {
	Host                      string
	Port                      int
	DataPath                  string
	BlockThreshold            time.Duration
	UpgradesConfig            string
	TmkmsProxy                bool
	SignerPeerDNS             string
	HaltHeight                int64
	MockMode                  bool
	ShutdownToken             string
	ExpectedShutdownTokenHash string
}

type Option func(*Options)

func WithHost(s string) Option {
	return func(opts *Options) {
		opts.Host = s
	}
}

func WithPort(v int) Option {
	return func(opts *Options) {
		opts.Port = v
	}
}

func WithDataPath(path string) Option {
	return func(opts *Options) {
		opts.DataPath = path
	}
}

func WithUpgradesConfig(path string) Option {
	return func(opts *Options) {
		opts.UpgradesConfig = path
	}
}

func WithBlockThreshold(n time.Duration) Option {
	return func(opts *Options) {
		opts.BlockThreshold = n
	}
}

func WithTmkmsProxy(enable bool) Option {
	return func(opts *Options) {
		opts.TmkmsProxy = enable
	}
}

func WithSignerPeerDNS(hostname string) Option {
	return func(opts *Options) {
		opts.SignerPeerDNS = hostname
	}
}

func WithHaltHeight(height int64) Option {
	return func(opts *Options) {
		opts.HaltHeight = height
	}
}

func WithMockMode(enable bool) Option {
	return func(opts *Options) {
		opts.MockMode = enable
	}
}

// WithShutdownToken configures the credential required by the shutdown endpoint.
func WithShutdownToken(token string) Option {
	return func(opts *Options) {
		opts.ShutdownToken = token
	}
}

// WithExpectedShutdownTokenHash configures the trusted identity of the injected credential.
func WithExpectedShutdownTokenHash(hash string) Option {
	return func(opts *Options) {
		opts.ExpectedShutdownTokenHash = hash
	}
}
