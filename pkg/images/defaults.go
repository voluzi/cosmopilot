// Package images defines the pinned defaults for operator-owned container images.
package images

const (
	DefaultNodeUtilsImage         = "ghcr.io/voluzi/node-utils:3.0.0"
	DefaultCosmoseedImage         = "ghcr.io/voluzi/cosmoseed:0.11.0"
	DefaultCosmoGuardImage        = "ghcr.io/voluzi/cosmoguard:4.0.3"
	DefaultCosmosignerImage       = "ghcr.io/voluzi/cosmosigner:0.2.1"
	DefaultUtilityImage           = "ghcr.io/voluzi/node-tools:1.4.3"
	DefaultDataExporterImage      = "ghcr.io/voluzi/dataexporter:2.0.1"
	DefaultTmKmsImage             = "ghcr.io/voluzi/tmkms:0.14.0-vault"
	DefaultVaultTokenRenewerImage = "ghcr.io/voluzi/vault-renewer:1.0.1"
)
