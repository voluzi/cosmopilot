package chainnode

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	appsv1 "github.com/voluzi/cosmopilot/v4/api/v1"
	"github.com/voluzi/cosmopilot/v4/internal/controllers"
	"github.com/voluzi/cosmopilot/v4/internal/tmkms"
	"github.com/voluzi/cosmopilot/v4/pkg/images"
)

func TestGetTmKmsUsesDefaultAndConfiguredImages(t *testing.T) {
	tests := []struct {
		name        string
		opts        *controllers.ControllerRunOptions
		wantTmKms   string
		wantRenewer string
	}{
		{
			name:        "defaults",
			wantTmKms:   images.DefaultTmKmsImage,
			wantRenewer: images.DefaultVaultTokenRenewerImage,
		},
		{
			name: "overrides",
			opts: &controllers.ControllerRunOptions{
				TmKmsImage:             "registry.example.com/tmkms@sha256:abcdef",
				VaultTokenRenewerImage: "registry.example.com/renewer@sha256:abcdef",
			},
			wantTmKms:   "registry.example.com/tmkms@sha256:abcdef",
			wantRenewer: "registry.example.com/renewer@sha256:abcdef",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := &Reconciler{opts: tt.opts}
			provider, kms, err := r.getTmkms(testTmKmsChainNode())
			require.NoError(t, err)
			require.Equal(t, tt.wantTmKms, kms.Config.Image)

			hashicorp, ok := provider.(*tmkms.HashicorpProvider)
			require.True(t, ok)
			require.Equal(t, tt.wantRenewer, hashicorp.TokenRenewerImage)
		})
	}
}

func TestGetTmKmsThreadsDefensivelyClonedImagePullSecrets(t *testing.T) {
	chainNode := testTmKmsChainNode()
	chainNode.Spec.Config = &appsv1.Config{ImagePullSecrets: []corev1.LocalObjectReference{{Name: "registry-creds"}}}

	_, kms, err := (&Reconciler{}).getTmkms(chainNode)
	require.NoError(t, err)
	chainNode.Spec.Config.ImagePullSecrets[0].Name = "mutated-source"
	require.Equal(t, []corev1.LocalObjectReference{{Name: "registry-creds"}}, kms.Config.ImagePullSecrets)
}

func testTmKmsChainNode() *appsv1.ChainNode {
	return &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "validator", Namespace: "default"},
		Spec: appsv1.ChainNodeSpec{Validator: &appsv1.ValidatorConfig{TmKMS: &appsv1.TmKMS{
			Provider: appsv1.TmKmsProvider{Hashicorp: &appsv1.TmKmsHashicorpProvider{
				Address: "https://vault.example.com",
				Key:     "validator",
				TokenSecret: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "vault-token"},
					Key:                  "token",
				},
				AutoRenewToken: true,
			}},
		}}},
		Status: appsv1.ChainNodeStatus{ChainID: "chain-1"},
	}
}
