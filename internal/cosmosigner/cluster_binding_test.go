package cosmosigner

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

func signerEnv(t *testing.T, p Params) map[string]string {
	t.Helper()
	env := map[string]string{}
	for _, e := range mustStatefulSet(t, p).Spec.Template.Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	return env
}

func secretKey(name, key string) *corev1.SecretKeySelector {
	return &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: key}
}

// cosmosigner 3.x requires the Raft transport and single-node bootstrap to be explicit.
func TestRaftSecurityIsExplicit(t *testing.T) {
	tests := []struct {
		name         string
		replicas     int32
		tlsSecret    *string
		wantInsecure bool
		wantSingle   bool
	}{
		{name: "single replica without TLS", replicas: 1, wantInsecure: true, wantSingle: true},
		{name: "single replica with TLS", replicas: 1, tlsSecret: ptr.To("raft-tls"), wantSingle: true},
		{name: "HA with TLS", replicas: 3, tlsSecret: ptr.To("raft-tls")},
		{name: "HA opted into insecure Raft", replicas: 3, wantInsecure: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := testParams()
			p.Replicas, p.RaftTLSSecret = tt.replicas, tt.tlsSecret
			env := signerEnv(t, p)
			if _, ok := env["COSMOSIGNER_RAFT_INSECURE"]; ok != tt.wantInsecure {
				t.Errorf("COSMOSIGNER_RAFT_INSECURE present = %v, want %v", ok, tt.wantInsecure)
			}
			if _, ok := env["COSMOSIGNER_RAFT_SINGLE_NODE"]; ok != tt.wantSingle {
				t.Errorf("COSMOSIGNER_RAFT_SINGLE_NODE present = %v, want %v", ok, tt.wantSingle)
			}
			for _, key := range []string{"COSMOSIGNER_RAFT_INSECURE", "COSMOSIGNER_RAFT_SINGLE_NODE"} {
				if value, ok := env[key]; ok && value != "true" {
					t.Errorf("%s = %q, want true", key, value)
				}
			}
		})
	}
}

// The new settings must stay out of config.yaml: cosmosigner 0.2.x rejects unknown config keys, so a
// signer pinned to an older image would stop starting.
func TestClusterBindingSettingsAreNotConfigKeys(t *testing.T) {
	for _, p := range []Params{testParams(), softwareParams(), gcpParams()} {
		p.Replicas = 1
		configYAML, err := p.ConfigYAML()
		if err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"insecure", "single_node", "claim", "binding"} {
			if strings.Contains(configYAML, key) {
				t.Errorf("config.yaml must not carry %q:\n%s", key, configYAML)
			}
		}
	}
}

func softwareParams() Params {
	p := testParams()
	p.Backend = Backend{Software: &SoftwareBackend{SecretName: "validator-key"}}
	return p
}

func gcpParams() Params {
	p := testParams()
	p.Backend = Backend{GCP: &GcpBackend{KeyVersion: "projects/p/locations/l/keyRings/r/cryptoKeys/k/cryptoKeyVersions/1"}}
	return p
}

func TestEverySignerClaimsAnUnclaimedKey(t *testing.T) {
	for name, p := range map[string]Params{"vault": testParams(), "software": softwareParams(), "gcp": gcpParams()} {
		if got := signerEnv(t, p)["COSMOSIGNER_CLAIM_IF_UNCLAIMED"]; got != "true" {
			t.Errorf("%s: COSMOSIGNER_CLAIM_IF_UNCLAIMED = %q, want true", name, got)
		}
	}
}

// The software key is a read-only Secret mount, so its marker lives on the state PVC.
func TestSoftwareBindingMarkerLivesOnTheStatePVC(t *testing.T) {
	sts := mustStatefulSet(t, softwareParams())
	env := signerEnv(t, softwareParams())
	marker := env["COSMOSIGNER_BINDING_FILE"]
	if !strings.HasPrefix(marker, dataMountPath+"/") {
		t.Fatalf("COSMOSIGNER_BINDING_FILE = %q, want a path under the state PVC mount %s", marker, dataMountPath)
	}
	for _, mount := range sts.Spec.Template.Spec.Containers[0].VolumeMounts {
		if mount.MountPath == dataMountPath && mount.Name != dataVolumeName {
			t.Fatalf("%s must be the state PVC, got volume %q", dataMountPath, mount.Name)
		}
		if strings.HasPrefix(marker, mount.MountPath+"/") && mount.ReadOnly {
			t.Fatalf("marker %q lands on read-only mount %q", marker, mount.Name)
		}
	}
}

func TestVaultBindingMountIsOnlyRenderedWhenSet(t *testing.T) {
	if _, ok := signerEnv(t, testParams())["COSMOSIGNER_VAULT_BINDING_MOUNT"]; ok {
		t.Fatal("an unset binding mount must keep cosmosigner's default")
	}
	p := testParams()
	p.Backend.Vault.BindingMount = "claims"
	if got := signerEnv(t, p)["COSMOSIGNER_VAULT_BINDING_MOUNT"]; got != "claims" {
		t.Fatalf("COSMOSIGNER_VAULT_BINDING_MOUNT = %q, want claims", got)
	}
}

// An optional claim credential reaches the signer as its own read-only mount and env var, and never
// the one-shot key-management pods, which do not claim.
func TestClaimCredentialsReachOnlyTheSigner(t *testing.T) {
	vault := testParams()
	vault.Backend.Vault.ClaimTokenSecret = secretKey("vault-admin", "admin-token")
	gcp := gcpParams()
	gcp.Backend.GCP.ClaimCredentialsSecret = secretKey("kms-admin", "sa.json")

	tests := []struct {
		name, env, file, secret, key string
		p                            Params
	}{
		{name: "vault", env: "COSMOSIGNER_VAULT_CLAIM_TOKEN_FILE", file: vaultClaimTokenFile, secret: "vault-admin", key: "admin-token", p: vault},
		{name: "gcp", env: "COSMOSIGNER_GCP_CLAIM_CREDENTIALS_FILE", file: gcpClaimCredsFile, secret: "kms-admin", key: "sa.json", p: gcp},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := signerEnv(t, tt.p)[tt.env]; got != tt.file {
				t.Fatalf("%s = %q, want %q", tt.env, got, tt.file)
			}
			sts := mustStatefulSet(t, tt.p)
			volumeName := ""
			for _, volume := range sts.Spec.Template.Spec.Volumes {
				if volume.Secret != nil && volume.Secret.SecretName == tt.secret {
					volumeName = volume.Name
					if len(volume.Secret.Items) != 1 || volume.Secret.Items[0].Key != tt.key {
						t.Fatalf("claim volume must project only %q, got %#v", tt.key, volume.Secret.Items)
					}
				}
			}
			if volumeName == "" {
				t.Fatalf("no volume for claim secret %q", tt.secret)
			}
			mounted := false
			for _, mount := range sts.Spec.Template.Spec.Containers[0].VolumeMounts {
				if mount.Name == volumeName {
					mounted = mount.ReadOnly && strings.HasPrefix(tt.file, mount.MountPath+"/")
				}
			}
			if !mounted {
				t.Fatalf("claim volume %q is not mounted read-only at the path in %s", volumeName, tt.env)
			}

			pod := JobRunner{Params: tt.p}.buildPod("pubkey", nil, nil, nil, 60)
			for _, volume := range pod.Spec.Volumes {
				if volume.Secret != nil && volume.Secret.SecretName == tt.secret {
					t.Fatalf("one-shot pod must not receive the claim secret")
				}
			}
			for _, env := range pod.Spec.Containers[0].Env {
				if env.Name == tt.env {
					t.Fatalf("one-shot pod must not receive %s", tt.env)
				}
			}
		})
	}

	for name, p := range map[string]Params{"vault": testParams(), "gcp": gcpParams()} {
		env := signerEnv(t, p)
		if _, ok := env["COSMOSIGNER_VAULT_CLAIM_TOKEN_FILE"]; ok {
			t.Errorf("%s: no claim token configured, but COSMOSIGNER_VAULT_CLAIM_TOKEN_FILE is set", name)
		}
		if _, ok := env["COSMOSIGNER_GCP_CLAIM_CREDENTIALS_FILE"]; ok {
			t.Errorf("%s: no claim credentials configured, but COSMOSIGNER_GCP_CLAIM_CREDENTIALS_FILE is set", name)
		}
	}
}
