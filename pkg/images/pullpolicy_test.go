package images

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
)

func TestPullPolicy(t *testing.T) {
	tests := []struct {
		image    string
		fallback corev1.PullPolicy
		want     corev1.PullPolicy
	}{
		{image: "ghcr.io/voluzi/node-utils:edge", fallback: corev1.PullIfNotPresent, want: corev1.PullAlways},
		{image: "ghcr.io/voluzi/node-utils:latest", fallback: corev1.PullIfNotPresent, want: corev1.PullAlways},
		{image: "ghcr.io/voluzi/node-utils", fallback: "", want: corev1.PullAlways},
		{image: "registry.voluzi.xyz:5000/node-utils", fallback: "", want: corev1.PullAlways},
		{image: "registry.voluzi.xyz:5000/node-utils:edge", fallback: "", want: corev1.PullAlways},
		// Pinned images keep the policy the container had, including none at all.
		{image: "ghcr.io/voluzi/node-utils:3.0.0", fallback: corev1.PullIfNotPresent, want: corev1.PullIfNotPresent},
		{image: "ghcr.io/voluzi/node-utils:3.0.0", fallback: "", want: ""},
		{image: "ghcr.io/voluzi/node-utils:edge-f5dfb69", fallback: "", want: ""},
		{image: "ghcr.io/voluzi/node-utils:edgy", fallback: "", want: ""},
		{image: "ghcr.io/voluzi/node-utils@sha256:0123456789abcdef", fallback: "", want: ""},
		{image: "ghcr.io/voluzi/node-utils:edge@sha256:0123456789abcdef", fallback: corev1.PullIfNotPresent, want: corev1.PullIfNotPresent},
		{image: "registry.voluzi.xyz:5000/node-utils:3.0.0", fallback: corev1.PullIfNotPresent, want: corev1.PullIfNotPresent},
	}

	for _, tt := range tests {
		t.Run(tt.image, func(t *testing.T) {
			assert.Equal(t, tt.want, PullPolicy(tt.image, tt.fallback))
		})
	}
}
