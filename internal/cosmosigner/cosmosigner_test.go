package cosmosigner

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func testParams() Params {
	return Params{
		Name:             "mychain-signer",
		Namespace:        "default",
		ChainID:          "test-1",
		Image:            "ghcr.io/voluzi/cosmosigner:latest",
		Replicas:         3,
		LogLevel:         "info",
		StateStorageSize: "1Gi",
		Backend: Backend{
			Vault: &VaultBackend{
				Address:     "https://vault:8200",
				KeyName:     "myval",
				Mount:       "transit",
				TokenSecret: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "vault-token"}, Key: "token"},
			},
		},
		Labels:         map[string]string{"chain-node-set": "mychain"},
		TargetSelector: map[string]string{"chain-node-set": "mychain", "cosmosigner-target": "true"},
	}
}

func TestBuildConfigMultiReplicaMembers(t *testing.T) {
	cfg := testParams().BuildConfig()
	if len(cfg.Raft.Members) != 3 {
		t.Fatalf("expected 3 raft members, got %d", len(cfg.Raft.Members))
	}
	if !cfg.Raft.Bootstrap {
		t.Fatalf("expected bootstrap to be true")
	}
	want := "mychain-signer-0.mychain-signer.default.svc:7070"
	if cfg.Raft.Members[0].Address != want {
		t.Fatalf("member 0 address = %q, want %q", cfg.Raft.Members[0].Address, want)
	}
	if cfg.Raft.Members[0].ID != "mychain-signer-0" {
		t.Fatalf("member 0 id = %q", cfg.Raft.Members[0].ID)
	}
	if cfg.NodeService != "mychain-signer-privval.default.svc:26659" {
		t.Fatalf("unexpected node_service %q", cfg.NodeService)
	}
	if cfg.Backend.Type != "vault" || cfg.Backend.Vault == nil || cfg.Backend.Vault.KeyName != "myval" {
		t.Fatalf("unexpected backend config: %+v", cfg.Backend)
	}
	if cfg.Backend.Vault.TokenFile != "/vault/token" {
		t.Fatalf("unexpected vault token file %q", cfg.Backend.Vault.TokenFile)
	}
}

func TestBuildConfigSingleReplicaNoMembers(t *testing.T) {
	p := testParams()
	p.Replicas = 1
	cfg := p.BuildConfig()
	if len(cfg.Raft.Members) != 0 {
		t.Fatalf("single-replica signer must have no members, got %d", len(cfg.Raft.Members))
	}
	if !cfg.Raft.Bootstrap {
		t.Fatalf("expected bootstrap true for single replica")
	}
}

func TestRenderYAMLUsesSnakeCase(t *testing.T) {
	out, err := testParams().ConfigYAML()
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"chain_id:", "node_service:", "conn_key:", "state_dir:", "bind_addr:", "data_dir:", "key_name:", "token_file:"} {
		if !strings.Contains(out, key) {
			t.Fatalf("rendered config missing %q:\n%s", key, out)
		}
	}
	// node_id and advertise are supplied per-pod via env, not the file.
	if strings.Contains(out, "node_id:") || strings.Contains(out, "advertise:") {
		t.Fatalf("rendered config must not contain per-pod node_id/advertise:\n%s", out)
	}
}

func TestStatefulSetShape(t *testing.T) {
	sts, err := testParams().StatefulSet()
	if err != nil {
		t.Fatal(err)
	}
	if sts.Spec.PodManagementPolicy != "Parallel" {
		t.Fatalf("expected Parallel pod management, got %q", sts.Spec.PodManagementPolicy)
	}
	if sts.Spec.ServiceName != "mychain-signer" {
		t.Fatalf("unexpected serviceName %q", sts.Spec.ServiceName)
	}
	if *sts.Spec.Replicas != 3 {
		t.Fatalf("unexpected replicas %d", *sts.Spec.Replicas)
	}
	if len(sts.Spec.VolumeClaimTemplates) != 1 || sts.Spec.VolumeClaimTemplates[0].Name != "data" {
		t.Fatalf("expected a single data volumeClaimTemplate")
	}
	c := sts.Spec.Template.Spec.Containers[0]
	var hasNodeID, hasAdvertise, hasRollme bool
	for _, e := range c.Env {
		switch e.Name {
		case "COSMOSIGNER_RAFT_NODE_ID":
			hasNodeID = e.Value == "$(POD_NAME)"
		case "COSMOSIGNER_RAFT_ADVERTISE":
			hasAdvertise = strings.Contains(e.Value, "$(POD_NAME).mychain-signer.default.svc:7070")
		case "ROLLME":
			hasRollme = e.Value != ""
		}
	}
	if !hasNodeID || !hasAdvertise || !hasRollme {
		t.Fatalf("missing expected env vars: nodeID=%v advertise=%v rollme=%v", hasNodeID, hasAdvertise, hasRollme)
	}
	if c.LivenessProbe == nil || c.LivenessProbe.TCPSocket == nil {
		t.Fatalf("expected TCP liveness probe")
	}
}

func TestDiscoveryServiceHeadlessPublishNotReady(t *testing.T) {
	svc := testParams().DiscoveryService()
	if svc.Spec.ClusterIP != corev1.ClusterIPNone {
		t.Fatalf("discovery service must be headless")
	}
	if !svc.Spec.PublishNotReadyAddresses {
		t.Fatalf("discovery service must publish not-ready addresses")
	}
	if svc.Spec.Ports[0].Port != 26659 {
		t.Fatalf("unexpected discovery port %d", svc.Spec.Ports[0].Port)
	}
	if svc.Spec.Selector["cosmosigner-target"] != "true" {
		t.Fatalf("discovery service selector wrong: %+v", svc.Spec.Selector)
	}
}

func TestVolumeClaimTemplateLabeledForCleanup(t *testing.T) {
	sts, err := testParams().StatefulSet()
	if err != nil {
		t.Fatal(err)
	}
	pvc := sts.Spec.VolumeClaimTemplates[0]
	want := InstanceLabels("mychain-signer")
	for k, v := range want {
		if pvc.Labels[k] != v {
			t.Fatalf("PVC template missing label %s=%s (got %+v)", k, v, pvc.Labels)
		}
	}
}

func TestSoftwareBackendConfig(t *testing.T) {
	p := testParams()
	p.Backend = Backend{Software: &SoftwareBackend{SecretName: "mychain-priv-key"}}
	cfg := p.BuildConfig()
	if cfg.Backend.Type != "software" || cfg.Backend.KeyFile != "/keys/priv_validator_key.json" {
		t.Fatalf("unexpected software backend: %+v", cfg.Backend)
	}
	sts, err := p.StatefulSet()
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, v := range sts.Spec.Template.Spec.Volumes {
		if v.Secret != nil && v.Secret.SecretName == "mychain-priv-key" {
			found = true
		}
	}
	if !found {
		t.Fatalf("software key secret volume not mounted")
	}
}
