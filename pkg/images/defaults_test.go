package images

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/voluzi/cosmopilot/v3/pkg/utils"
)

func TestDefaultsArePinned(t *testing.T) {
	defaults := map[string]string{
		"node-utils":          DefaultNodeUtilsImage,
		"cosmoseed":           DefaultCosmoseedImage,
		"cosmoguard":          DefaultCosmoGuardImage,
		"cosmosigner":         DefaultCosmosignerImage,
		"utility":             DefaultUtilityImage,
		"data-exporter":       DefaultDataExporterImage,
		"tmkms":               DefaultTmKmsImage,
		"vault-token-renewer": DefaultVaultTokenRenewerImage,
	}

	for name, image := range defaults {
		t.Run(name, func(t *testing.T) {
			_, reference := utils.SplitImageRef(image)
			assert.NotEmpty(t, reference)
			assert.NotEqual(t, "latest", reference)
		})
	}
}
