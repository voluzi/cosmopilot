// Package images defines the pinned defaults for operator-owned container images.
package images

const (
	DefaultNodeUtilsImage         = "ghcr.io/voluzi/node-utils:3.0.0"
	DefaultCosmoseedImage         = "ghcr.io/voluzi/cosmoseed:0.12.0"
	DefaultCosmoGuardImage        = "ghcr.io/voluzi/cosmoguard:5.0.0"
	DefaultCosmosignerImage       = "ghcr.io/voluzi/cosmosigner:3.0.0"
	DefaultUtilityImage           = "ghcr.io/voluzi/node-tools:1.4.3"
	DefaultDataExporterImage      = "ghcr.io/voluzi/dataexporter:2.0.1"
	DefaultTmKmsImage             = "ghcr.io/voluzi/tmkms:0.14.0-vault"
	DefaultVaultTokenRenewerImage = "ghcr.io/voluzi/vault-renewer:1.0.1"
)
