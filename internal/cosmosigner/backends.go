package cosmosigner

import (
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
}

// GcpBackend holds the GCP KMS backend configuration.
type GcpBackend struct {
	KeyVersion        string
	CredentialsSecret *corev1.SecretKeySelector
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
)

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
