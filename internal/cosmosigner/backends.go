package cosmosigner

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/voluzi/cosmopilot/v2/internal/k8s"
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
	Address           string
	KeyName           string
	Mount             string
	Namespace         string
	TokenSecret       *corev1.SecretKeySelector
	CertificateSecret *corev1.SecretKeySelector
	AutoRenewToken    bool
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

	vaultTokenFile = vaultMountDir + "/token"
	vaultCaFile    = vaultMountDir + "/ca.crt"
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
				Address:   b.Vault.Address,
				TokenFile: vaultTokenFile,
				Mount:     b.Vault.Mount,
				KeyName:   b.Vault.KeyName,
				Namespace: b.Vault.Namespace,
				TLSCACert: vaultCaCert(b.Vault),
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
		return []corev1.Volume{
			{
				Name: softwareVolume,
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{SecretName: b.Software.SecretName},
				},
			},
		}
	case b.Vault != nil:
		vols := []corev1.Volume{
			{
				Name: vaultTokenVolume,
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{SecretName: b.Vault.TokenSecret.Name},
				},
			},
		}
		if b.Vault.CertificateSecret != nil {
			vols = append(vols, corev1.Volume{
				Name: vaultCaVolume,
				VolumeSource: corev1.VolumeSource{
					Secret: &corev1.SecretVolumeSource{SecretName: b.Vault.CertificateSecret.Name},
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
						Secret: &corev1.SecretVolumeSource{SecretName: b.GCP.CredentialsSecret.Name},
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
		return []corev1.VolumeMount{
			{Name: softwareVolume, ReadOnly: true, MountPath: softwareKeyFile, SubPath: "priv_validator_key.json"},
		}
	case b.Vault != nil:
		mounts := []corev1.VolumeMount{
			{Name: vaultTokenVolume, ReadOnly: true, MountPath: vaultTokenFile, SubPath: b.Vault.TokenSecret.Key},
		}
		if b.Vault.CertificateSecret != nil {
			mounts = append(mounts, corev1.VolumeMount{
				Name: vaultCaVolume, ReadOnly: true, MountPath: vaultCaFile, SubPath: b.Vault.CertificateSecret.Key,
			})
		}
		return mounts
	case b.GCP != nil:
		if b.GCP.CredentialsSecret != nil {
			return []corev1.VolumeMount{
				{Name: gcpCredsVolume, ReadOnly: true, MountPath: gcpCredsFile, SubPath: b.GCP.CredentialsSecret.Key},
			}
		}
	}
	return nil
}

// sidecars returns extra containers required by the backend (e.g. the Vault token renewer).
func (b Backend) sidecars() []corev1.Container {
	if b.Vault == nil || !b.Vault.AutoRenewToken {
		return nil
	}
	renewerResources := corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("100m"),
		corev1.ResourceMemory: resource.MustParse("64Mi"),
	}
	c := corev1.Container{
		Name:            "vault-token-renewer",
		Image:           "ghcr.io/voluzi/vault-token-renewer",
		SecurityContext: k8s.RestrictedSecurityContext(),
		Env: []corev1.EnvVar{
			{Name: "VAULT_ADDR", Value: b.Vault.Address},
			{Name: "VAULT_TOKEN_PATH", Value: vaultTokenFile},
		},
		VolumeMounts: []corev1.VolumeMount{
			{Name: vaultTokenVolume, ReadOnly: true, MountPath: vaultTokenFile, SubPath: b.Vault.TokenSecret.Key},
		},
		Resources: corev1.ResourceRequirements{Requests: renewerResources, Limits: renewerResources},
	}
	if b.Vault.CertificateSecret != nil {
		c.Env = append(c.Env, corev1.EnvVar{Name: "VAULT_CACERT", Value: vaultCaFile})
		c.VolumeMounts = append(c.VolumeMounts, corev1.VolumeMount{
			Name: vaultCaVolume, ReadOnly: true, MountPath: vaultCaFile, SubPath: b.Vault.CertificateSecret.Key,
		})
	}
	return []corev1.Container{c}
}
