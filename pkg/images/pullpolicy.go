package images

import (
	corev1 "k8s.io/api/core/v1"

	"github.com/voluzi/cosmopilot/v3/pkg/utils"
)

// mutableTags are the tags that are moved to new builds: `latest`, and `edge`, which every push to
// cosmopilot main republishes.
var mutableTags = map[string]bool{"latest": true, "edge": true}

// PullPolicy returns the pull policy for an operator-owned image. An image without a reference or
// with a mutable tag is pulled Always, so a restarted pod picks up the newest build instead of a
// copy the node cached earlier. Any other image keeps fallback, which is the policy the container
// used before and may be empty, so pinned images render exactly as they did.
func PullPolicy(image string, fallback corev1.PullPolicy) corev1.PullPolicy {
	_, reference := utils.SplitImageRef(image)
	if reference == "" || mutableTags[reference] {
		return corev1.PullAlways
	}
	return fallback
}
