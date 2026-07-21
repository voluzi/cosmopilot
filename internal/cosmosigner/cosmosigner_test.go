package cosmosigner

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func testParams() Params {
	return Params{
		Name:              "mychain-signer",
		Namespace:         "default",
		ChainID:           "test-1",
		Image:             "ghcr.io/voluzi/cosmosigner:0.2.0",
		Replicas:          3,
		LogLevel:          "info",
		ExpectedPublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		StateStorageSize:  "1Gi",
		Backend: Backend{
			Vault: &VaultBackend{
				Address:     "https://vault:8200",
				KeyName:     "myval",
				KeyVersion:  4,
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
	if cfg.Backend.Vault.TokenFile != "/vault/token-dir/token" {
		t.Fatalf("unexpected vault token file %q", cfg.Backend.Vault.TokenFile)
	}
	if cfg.Backend.Vault.KeyVersion != 4 {
		t.Fatalf("unexpected vault key version %d", cfg.Backend.Vault.KeyVersion)
	}
	if cfg.ExpectedPublicKey != "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=" {
		t.Fatalf("unexpected expected public key %q", cfg.ExpectedPublicKey)
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

func TestMultiReplicaConfigBootstrapsOnlyPodZero(t *testing.T) {
	p := testParams()
	configYAML, err := p.ConfigYAML()
	if err != nil {
		t.Fatal(err)
	}
	configMap, err := p.ConfigMap(configYAML)
	if err != nil {
		t.Fatal(err)
	}

	for i := int32(0); i < p.Replicas; i++ {
		key := fmt.Sprintf("%s-%d.yaml", p.Name, i)
		config, ok := configMap.Data[key]
		if !ok {
			t.Fatalf("ConfigMap missing per-pod config %q", key)
		}
		if got, want := strings.Contains(config, "bootstrap: true"), i == 0; got != want {
			t.Fatalf("pod %d bootstrap = %v, want %v", i, got, want)
		}
	}

	sts, err := p.StatefulSet(configYAML)
	if err != nil {
		t.Fatal(err)
	}
	var configMount corev1.VolumeMount
	for _, mount := range sts.Spec.Template.Spec.Containers[0].VolumeMounts {
		if mount.Name == "config" {
			configMount = mount
			break
		}
	}
	if configMount.SubPathExpr != "$(POD_NAME).yaml" {
		t.Fatalf("config subPathExpr = %q, want per-pod config", configMount.SubPathExpr)
	}
}

func TestRenderYAMLUsesSnakeCase(t *testing.T) {
	out, err := testParams().ConfigYAML()
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"chain_id:", "expected_public_key:", "node_service:", "conn_key:", "state_dir:", "bind_addr:", "data_dir:", "key_name:", "key_version:", "token_file:"} {
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
	sts := mustStatefulSet(t, testParams())
	if sts.Spec.PersistentVolumeClaimRetentionPolicy == nil ||
		sts.Spec.PersistentVolumeClaimRetentionPolicy.WhenDeleted != appsv1.DeletePersistentVolumeClaimRetentionPolicyType ||
		sts.Spec.PersistentVolumeClaimRetentionPolicy.WhenScaled != appsv1.RetainPersistentVolumeClaimRetentionPolicyType {
		t.Fatalf("StatefulSet must delete PVCs on ordinary deletion and retain them on scale-down, got %#v", sts.Spec.PersistentVolumeClaimRetentionPolicy)
	}
	if sts.Spec.PodManagementPolicy != "Parallel" {
		t.Fatalf("expected Parallel pod management, got %q", sts.Spec.PodManagementPolicy)
	}
	if sts.Spec.UpdateStrategy.Type != appsv1.OnDeleteStatefulSetStrategyType {
		t.Fatalf("signer StatefulSet must use OnDelete to prevent mixed-version automatic rolling updates, got %q", sts.Spec.UpdateStrategy.Type)
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
	if len(sts.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("Vault token renewal is built into cosmosigner; expected no renewer sidecar, got %d containers", len(sts.Spec.Template.Spec.Containers))
	}
	if got := strings.Join(c.Args, " "); !strings.Contains(got, "--expected-public-key "+testParams().ExpectedPublicKey) {
		t.Fatalf("statefulset must pass the expected public key as a compatibility-enforcing flag, args=%q", got)
	}
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

func TestNetworkPolicyRestrictsRaftIngressToSignerPeers(t *testing.T) {
	policy := testParams().NetworkPolicy()
	if len(policy.Spec.PolicyTypes) != 1 || policy.Spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress {
		t.Fatalf("unexpected policy types: %#v", policy.Spec.PolicyTypes)
	}
	if len(policy.Spec.Ingress) != 1 || len(policy.Spec.Ingress[0].From) != 1 {
		t.Fatalf("expected one raft ingress rule, got %#v", policy.Spec.Ingress)
	}
	peer := policy.Spec.Ingress[0].From[0].PodSelector
	if peer == nil || peer.MatchLabels["app.kubernetes.io/instance"] != testParams().Name {
		t.Fatalf("raft ingress must be limited to this signer's peers: %#v", peer)
	}
	if len(policy.Spec.Ingress[0].Ports) != 1 || policy.Spec.Ingress[0].Ports[0].Port == nil || policy.Spec.Ingress[0].Ports[0].Port.IntVal != 7070 {
		t.Fatalf("raft ingress must expose only port 7070: %#v", policy.Spec.Ingress[0].Ports)
	}
}

func TestLifecycleDigestCoversRuntimeAndExpectedIdentity(t *testing.T) {
	base := testParams()
	digest, err := base.LifecycleDigest("signing-digest")
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		mutate func(*Params)
	}{
		{name: "image", mutate: func(p *Params) { p.Image = "ghcr.io/voluzi/cosmosigner:0.3.0" }},
		{name: "expected public key", mutate: func(p *Params) { p.ExpectedPublicKey = "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB=" }},
		{name: "vault token secret", mutate: func(p *Params) { p.Backend.Vault.TokenSecret.Name = "rotated-token" }},
		{name: "resources", mutate: func(p *Params) {
			p.Resources.Requests = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")}
		}},
		{name: "service account", mutate: func(p *Params) { p.ServiceAccountName = "signer" }},
		{name: "pod labels", mutate: func(p *Params) { p.Labels["security.example/policy"] = "restricted" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			changed := testParams()
			tc.mutate(&changed)
			got, err := changed.LifecycleDigest("signing-digest")
			if err != nil {
				t.Fatal(err)
			}
			if got == digest {
				t.Fatalf("%s change must alter lifecycle digest", tc.name)
			}
		})
	}

	same, err := testParams().LifecycleDigest("signing-digest")
	if err != nil {
		t.Fatal(err)
	}
	if same != digest {
		t.Fatalf("identical runtime must have stable digest: %q != %q", same, digest)
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
	sts := mustStatefulSet(t, testParams())
	pvc := sts.Spec.VolumeClaimTemplates[0]
	want := InstanceLabels("mychain-signer")
	for k, v := range want {
		if pvc.Labels[k] != v {
			t.Fatalf("PVC template missing label %s=%s (got %+v)", k, v, pvc.Labels)
		}
	}
	if !slices.Contains(pvc.Finalizers, RetainedStateFinalizer) {
		t.Fatalf("PVC template must protect retained slash state, got finalizers %v", pvc.Finalizers)
	}
}

func TestSoftwareBackendConfig(t *testing.T) {
	p := testParams()
	p.Backend = Backend{Software: &SoftwareBackend{SecretName: "mychain-priv-key"}}
	cfg := p.BuildConfig()
	if cfg.Backend.Type != "software" || cfg.Backend.KeyFile != "/keys/priv_validator_key.json" {
		t.Fatalf("unexpected software backend: %+v", cfg.Backend)
	}
	sts := mustStatefulSet(t, p)
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

// mustStatefulSet renders the config once and builds the StatefulSet, mirroring the controllers.
// All errors are fatal, so callers get a usable StatefulSet or the test ends here.
func mustStatefulSet(t *testing.T, p Params) *appsv1.StatefulSet {
	t.Helper()
	cfg, err := p.ConfigYAML()
	if err != nil {
		t.Fatal(err)
	}
	sts, err := p.StatefulSet(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return sts
}
