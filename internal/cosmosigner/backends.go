package cosmosigner

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// Backend selects and configures the signing backend. Exactly one field is set.
type Backend struct {
	Software *SoftwareBackend
	Vault    *VaultBackend
	GCP      *GcpBackend
}

// SoftwareBackend holds the local software backend configuration.
type SoftwareBackend struct {
	// SecretName is the secret containing priv_validator_key.json.
	SecretName string
}

// VaultBackend holds the Vault Transit backend configuration.
type VaultBackend struct {
	Address               string
	KeyName               string
	KeyVersion            int
	Mount                 string
	Namespace             string
	TokenSecret           *corev1.SecretKeySelector
	CertificateSecret     *corev1.SecretKeySelector
	SkipCertificateVerify bool
	// BindingMount and ClaimTokenSecret configure the cosmosigner 3.x cluster binding; both are
	// optional and only reach the signer StatefulSet.
	BindingMount     string
	ClaimTokenSecret *corev1.SecretKeySelector
}

// GcpBackend holds the GCP KMS backend configuration. KeyVersion is the version the signer signs
// with; for a controller-managed import it is empty until the import resolves it.
type GcpBackend struct {
	KeyVersion        string
	Import            *GcpImport
	CredentialsSecret *corev1.SecretKeySelector
	// ClaimCredentialsSecret is the optional identity that labels the CryptoKey with its owning
	// signer cluster (cosmosigner 3.x). It only reaches the signer StatefulSet.
	ClaimCredentialsSecret *corev1.SecretKeySelector
}

// GcpImport are the Cloud KMS destination coordinates of a controller-managed BYOK import. They are
// passed as flags to the one-shot `cosmosigner import` pod ONLY and never reach the signer
// StatefulSet, whose backend is configured purely from the resolved KeyVersion — so resolving an
// import does not perturb an existing signer's pod template or lifecycle digest.
type GcpImport struct {
	Project         string
	Location        string
	KeyRing         string
	Key             string
	ImportJob       string
	ProtectionLevel string
}

// CryptoKeyName is the destination CryptoKey resource name. Every version the import pod reports
// must live under it, otherwise the controller would persist a version pointing somewhere else.
func (g *GcpImport) CryptoKeyName() string {
	if g == nil {
		return ""
	}
	return fmt.Sprintf("projects/%s/locations/%s/keyRings/%s/cryptoKeys/%s", g.Project, g.Location, g.KeyRing, g.Key)
}

const (
	vaultTokenVolume = "vault-token"
	vaultCaVolume    = "vault-ca"
	gcpCredsVolume   = "gcp-credentials"
	softwareVolume   = "software-key"

	// Credential files are exposed via DIRECTORY mounts with an items projection (never subPath):
	// kubelet refreshes directory-mounted Secret contents in place via its symlink swap, whereas a
	// subPath mount freezes the file at pod start — an in-place rotation of e.g. a short-lived Vault
	// token would then never reach the signer until a manual restart. Each credential gets its own
	// directory so the mounts cannot collide.
	vaultTokenDir  = vaultMountDir + "/token-dir"
	vaultTokenFile = vaultTokenDir + "/token"
	vaultCaDir     = vaultMountDir + "/ca-dir"
	vaultCaFile    = vaultCaDir + "/ca.crt"
	gcpCredsFile   = gcpMountDir + "/credentials.json"

	vaultClaimTokenVolume = "vault-claim-token"
	gcpClaimCredsVolume   = "gcp-claim-credentials"
	vaultClaimTokenDir    = vaultMountDir + "/claim-token-dir"
	vaultClaimTokenFile   = vaultClaimTokenDir + "/token"
	gcpClaimCredsDir      = "/gcp-claim"
	gcpClaimCredsFile     = gcpClaimCredsDir + "/credentials.json"

	// softwareBindingFile is where a software signer keeps its cluster binding marker: on the state
	// PVC, beside the Raft history it names, because the key itself is a read-only Secret mount.
	softwareBindingFile = dataMountPath + "/cluster-binding.json"
)

// clusterBindingEnv configures how a cosmosigner 3.x signer binds its key to its Raft history.
// These are environment variables, not config keys: cosmosigner 0.2.x rejects unknown config keys
// but ignores unknown environment, so a signer pinned to an older image still starts.
//
// The signer claims an unclaimed key itself: the controller already guarantees one owner per key
// (consensus-key reservations, and every signer is stopped before a migration recreates it), which
// is the precondition cosmosigner documents for claiming at startup. A key claimed by another
// cluster is still refused.
func (b Backend) clusterBindingEnv() []corev1.EnvVar {
	env := []corev1.EnvVar{{Name: "COSMOSIGNER_CLAIM_IF_UNCLAIMED", Value: "true"}}
	switch {
	case b.Software != nil:
		env = append(env, corev1.EnvVar{Name: "COSMOSIGNER_BINDING_FILE", Value: softwareBindingFile})
	case b.Vault != nil:
		if b.Vault.BindingMount != "" {
			env = append(env, corev1.EnvVar{Name: "COSMOSIGNER_VAULT_BINDING_MOUNT", Value: b.Vault.BindingMount})
		}
		if b.Vault.ClaimTokenSecret != nil {
			env = append(env, corev1.EnvVar{Name: "COSMOSIGNER_VAULT_CLAIM_TOKEN_FILE", Value: vaultClaimTokenFile})
		}
	case b.GCP != nil:
		if b.GCP.ClaimCredentialsSecret != nil {
			env = append(env, corev1.EnvVar{Name: "COSMOSIGNER_GCP_CLAIM_CREDENTIALS_FILE", Value: gcpClaimCredsFile})
		}
	}
	return env
}

// claimVolumes and claimVolumeMounts expose the optional claim credential to the signer only; the
// one-shot key-management pods never claim, so they do not receive it.
func (b Backend) claimVolumes() []corev1.Volume {
	var selector *corev1.SecretKeySelector
	var name, path string
	switch {
	case b.Vault != nil && b.Vault.ClaimTokenSecret != nil:
		selector, name, path = b.Vault.ClaimTokenSecret, vaultClaimTokenVolume, "token"
	case b.GCP != nil && b.GCP.ClaimCredentialsSecret != nil:
		selector, name, path = b.GCP.ClaimCredentialsSecret, gcpClaimCredsVolume, "credentials.json"
	default:
		return nil
	}
	return []corev1.Volume{{
		Name: name,
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName: selector.Name,
				Items:      []corev1.KeyToPath{{Key: selector.Key, Path: path}},
			},
		},
	}}
}

func (b Backend) claimVolumeMounts() []corev1.VolumeMount {
	switch {
	case b.Vault != nil && b.Vault.ClaimTokenSecret != nil:
		return []corev1.VolumeMount{{Name: vaultClaimTokenVolume, ReadOnly: true, MountPath: vaultClaimTokenDir}}
	case b.GCP != nil && b.GCP.ClaimCredentialsSecret != nil:
		return []corev1.VolumeMount{{Name: gcpClaimCredsVolume, ReadOnly: true, MountPath: gcpClaimCredsDir}}
	}
	return nil
}

// backendConfig returns the cosmosigner backend configuration section for this backend.
func (b Backend) backendConfig() BackendConfig {
	switch {
	case b.Software != nil:
		return BackendConfig{
			Type:    backendSoftware,
			KeyFile: softwareKeyFile,
		}
	case b.Vault != nil:
		return BackendConfig{
			Type: backendVault,
			Vault: &VaultConfig{
				Address:    b.Vault.Address,
				TokenFile:  vaultTokenFile,
				Mount:      b.Vault.Mount,
				KeyName:    b.Vault.KeyName,
				KeyVersion: b.Vault.KeyVersion,
				Namespace:  b.Vault.Namespace,
				TLSCACert:  vaultCaCert(b.Vault),
			},
		}
	case b.GCP != nil:
		return BackendConfig{
			Type: backendGcpKms,
			GCP: &GCPConfig{
				KeyVersion:      b.GCP.KeyVersion,
				CredentialsFile: gcpCredsFilePath(b.GCP),
			},
		}
	default:
		return BackendConfig{Type: backendSoftware}
	}
}

func vaultCaCert(v *VaultBackend) string {
	if v.CertificateSecret != nil {
		return vaultCaFile
	}
	return ""
}

func gcpCredsFilePath(g *GcpBackend) string {
	if g.CredentialsSecret != nil {
		return gcpCredsFile
	}
	return ""
}

// volumes returns the volumes required by the backend.
func (b Backend) volumes() []corev1.Volume {
	switch {
	case b.Software != nil:
		// Project ONLY priv_validator_key.json: the referenced Secret may carry unrelated keys (e.g. a
		// validator's account mnemonic in a shared Secret) that must not be readable by the signer.
		return []corev1.Volume{
			{
				Name: softwareVolume,
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName: b.Software.SecretName,
						Items:      []corev1.KeyToPath{{Key: "priv_validator_key.json", Path: "priv_validator_key.json"}},
					},
				},
			},
		}
	case b.Vault != nil:
		// The items projection maps the (arbitrary) Secret key to a stable filename inside the
		// directory mount, so rotation propagates (see the constants above) while the config keeps
		// pointing at a fixed path.
		vols := []corev1.Volume{
			{
				Name: vaultTokenVolume,
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName: b.Vault.TokenSecret.Name,
						Items:      []corev1.KeyToPath{{Key: b.Vault.TokenSecret.Key, Path: "token"}},
					},
				},
			},
		}
		if b.Vault.CertificateSecret != nil {
			vols = append(vols, corev1.Volume{
				Name: vaultCaVolume,
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{
						SecretName: b.Vault.CertificateSecret.Name,
						Items:      []corev1.KeyToPath{{Key: b.Vault.CertificateSecret.Key, Path: "ca.crt"}},
					},
				},
			})
		}
		return vols
	case b.GCP != nil:
		if b.GCP.CredentialsSecret != nil {
			return []corev1.Volume{
				{
					Name: gcpCredsVolume,
					VolumeSource: corev1.VolumeSource{
						Secret: &corev1.SecretVolumeSource{
							SecretName: b.GCP.CredentialsSecret.Name,
							Items:      []corev1.KeyToPath{{Key: b.GCP.CredentialsSecret.Key, Path: "credentials.json"}},
						},
					},
				},
			}
		}
	}
	return nil
}

// volumeMounts returns the volume mounts required by the backend.
func (b Backend) volumeMounts() []corev1.VolumeMount {
	switch {
	case b.Software != nil:
		// Directory mount (no subPath): the secret's priv_validator_key.json lands at softwareKeyFile
		// and an in-place rotation propagates into the container.
		return []corev1.VolumeMount{
			{Name: softwareVolume, ReadOnly: true, MountPath: softwareKeyDir},
		}
	case b.Vault != nil:
		// Directory mounts (no subPath) so in-place Secret rotation propagates into the container.
		mounts := []corev1.VolumeMount{
			{Name: vaultTokenVolume, ReadOnly: true, MountPath: vaultTokenDir},
		}
		if b.Vault.CertificateSecret != nil {
			mounts = append(mounts, corev1.VolumeMount{
				Name: vaultCaVolume, ReadOnly: true, MountPath: vaultCaDir,
			})
		}
		return mounts
	case b.GCP != nil:
		if b.GCP.CredentialsSecret != nil {
			return []corev1.VolumeMount{
				{Name: gcpCredsVolume, ReadOnly: true, MountPath: gcpMountDir},
			}
		}
	}
	return nil
}
