package chainnode

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	appsv1 "github.com/voluzi/cosmopilot/v4/api/v1"
	"github.com/voluzi/cosmopilot/v4/internal/controllers"
)

// Pinned companion images must render exactly as before, so an operator upgrade does not change the
// node pod spec; a moving `edge` image must be re-pulled when the pod restarts.
func TestNodeUtilsContainersPullMovingImagesAlways(t *testing.T) {
	chainNode := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: "node", Namespace: "ns"}}

	tests := []struct {
		image string
		want  corev1.PullPolicy
	}{
		{image: "ghcr.io/voluzi/node-utils:3.0.0", want: corev1.PullIfNotPresent},
		{image: "ghcr.io/voluzi/node-utils:edge", want: corev1.PullAlways},
	}
	for _, tt := range tests {
		t.Run(tt.image, func(t *testing.T) {
			r := &Reconciler{opts: &controllers.ControllerRunOptions{NodeUtilsImage: tt.image}}
			assert.Equal(t, tt.want, r.buildNodeUtilsInitContainer(chainNode, "shutdown", 1000, 1000).ImagePullPolicy)
			assert.Equal(t, tt.want, r.buildCosmosignerDiscoveryInitContainer(chainNode, "signer").ImagePullPolicy)
		})
	}
}
