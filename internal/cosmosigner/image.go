package cosmosigner

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/version"

	"github.com/voluzi/cosmopilot/v5/pkg/images"
	"github.com/voluzi/cosmopilot/v5/pkg/utils"
)

func RequireSupportedImage(image string) error {
	_, tag := utils.SplitImageRef(strings.SplitN(image, "@", 2)[0])
	v, err := version.ParseSemantic(tag)
	if err != nil {
		// Moving tags and digest-only references cannot establish a release version locally.
		return nil
	}
	minimum := version.MustParseSemantic(images.MinimumCosmosignerVersion)
	if !v.AtLeast(minimum) {
		return fmt.Errorf("cosmosigner image %q requires version %s or newer for HTTP health endpoints and bounded redial; the running signer is left untouched", image, images.MinimumCosmosignerVersion)
	}
	return nil
}
