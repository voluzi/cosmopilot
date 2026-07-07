// Package cosmosigner builds the Kubernetes resources for a Cosmopilot-managed cosmosigner
// remote-signer deployment (github.com/voluzi/cosmosigner): a StatefulSet that dials the
// targeted nodes' priv_validator_laddr, the headless services for raft peering and node
// discovery, the rendered config.yaml, and the one-shot key-management pods.
package cosmosigner

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

const (
	// dataMountPath is where the per-replica state PVC is mounted. It holds the raft state
	// (double-sign protection) and the persisted connection key.
	dataMountPath = "/data"
	connKeyPath   = dataMountPath + "/conn_key.json"
	raftDataDir   = dataMountPath + "/raft"

	// configMountPath is where the rendered config.yaml is mounted.
	configMountPath = "/config"
	configFileName  = "config.yaml"

	// raftPort is the port used by the inter-replica raft transport.
	raftPort     = 7070
	raftPortName = "raft"

	// raftBindAddr is the address the raft transport listens on inside the pod.
	raftBindAddr = "0.0.0.0:7070"

	// Backend types (mirrors cosmosigner's backend.Type values).
	backendSoftware = "software"
	backendVault    = "vault"
	backendGcpKms   = "gcpkms"

	// softwareKeyPath is where the priv_validator_key.json is mounted for the software backend.
	softwareKeyDir  = "/keys"
	softwareKeyFile = softwareKeyDir + "/priv_validator_key.json"

	// vaultMountDir and gcpMountDir are where backend credentials are mounted.
	vaultMountDir = "/vault"
	gcpMountDir   = "/gcp"

	// raftTLSMountDir is where the raft mTLS material is mounted.
	raftTLSMountDir = "/tls/raft"
)

// Config mirrors the subset of cosmosigner's runtime configuration that Cosmopilot renders into
// a config.yaml ConfigMap. Per-pod fields (node_id, advertise) are intentionally omitted here and
// supplied via environment variables, which take precedence over the file.
type Config struct {
	ChainID     string        `yaml:"chain_id"`
	NodeService string        `yaml:"node_service,omitempty"`
	Nodes       []string      `yaml:"nodes,omitempty"`
	ConnKey     string        `yaml:"conn_key"`
	StateDir    string        `yaml:"state_dir"`
	LogLevel    string        `yaml:"log_level"`
	Backend     BackendConfig `yaml:"backend"`
	Raft        RaftConfig    `yaml:"raft"`
}

// BackendConfig mirrors cosmosigner's backend.Config.
type BackendConfig struct {
	Type    string       `yaml:"type"`
	KeyFile string       `yaml:"key_file,omitempty"`
	Vault   *VaultConfig `yaml:"vault,omitempty"`
	GCP     *GCPConfig   `yaml:"gcp,omitempty"`
}

// VaultConfig mirrors cosmosigner's backend.VaultConfig.
type VaultConfig struct {
	Address   string `yaml:"address"`
	TokenFile string `yaml:"token_file"`
	Mount     string `yaml:"mount,omitempty"`
	KeyName   string `yaml:"key_name"`
	Namespace string `yaml:"namespace,omitempty"`
	TLSCACert string `yaml:"tls_ca_cert,omitempty"`
}

// GCPConfig mirrors cosmosigner's backend.GCPKMSConfig.
type GCPConfig struct {
	KeyVersion      string `yaml:"key_version"`
	CredentialsFile string `yaml:"credentials_file,omitempty"`
}

// RaftConfig mirrors cosmosigner's config.RaftConfig. node_id and advertise are supplied per-pod
// via environment variables, so they are omitted from the rendered file.
type RaftConfig struct {
	BindAddr  string   `yaml:"bind_addr"`
	DataDir   string   `yaml:"data_dir"`
	Bootstrap bool     `yaml:"bootstrap"`
	Members   []Member `yaml:"members,omitempty"`
	TLSCert   string   `yaml:"tls_cert,omitempty"`
	TLSKey    string   `yaml:"tls_key,omitempty"`
	TLSCA     string   `yaml:"tls_ca,omitempty"`
}

// Member is a raft cluster member.
type Member struct {
	ID      string `yaml:"id"`
	Address string `yaml:"address"`
}

// Render serializes the config to YAML.
func (c *Config) Render() (string, error) {
	out, err := yaml.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("rendering cosmosigner config: %w", err)
	}
	return string(out), nil
}
