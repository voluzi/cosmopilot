package k8s

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/voluzi/cosmopilot/v3/pkg/images"
)

func TestPvcHelperBuildWriteFilePodUsesConfiguredImageAndSecrets(t *testing.T) {
	pullSecrets := []corev1.LocalObjectReference{{Name: "registry-creds"}}
	affinity := &corev1.Affinity{}
	nodeSelector := map[string]string{"disk": "fast"}
	helper := NewPvcHelper(nil, nil, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: "data", Namespace: "default",
	}}, "registry.example.com:5000/tools:custom", pullSecrets)
	pullSecrets[0].Name = "mutated-source"

	pod := helper.buildWriteFilePod("config/genesis.json", "priority", affinity, nodeSelector)
	require.Len(t, pod.Spec.Containers, 1)
	container := pod.Spec.Containers[0]
	assert.Equal(t, "registry.example.com:5000/tools:custom", container.Image)
	assert.Equal(t, []corev1.LocalObjectReference{{Name: "registry-creds"}}, pod.Spec.ImagePullSecrets)
	assert.Equal(t, []string{"/bin/sh"}, container.Command)
	assert.Equal(t, []string{"-c", "cp /dev/stdin /pvc/config/genesis.json"}, container.Args)
	assert.True(t, container.Stdin)
	assert.True(t, container.StdinOnce)
	assert.Equal(t, ptr.To[int64](0), pod.Spec.TerminationGracePeriodSeconds)
	assert.Equal(t, ptr.To[int64](300), pod.Spec.ActiveDeadlineSeconds)
	assert.Equal(t, "priority", pod.Spec.PriorityClassName)
	assert.Same(t, affinity, pod.Spec.Affinity)
	assert.Equal(t, nodeSelector, pod.Spec.NodeSelector)
	assert.Equal(t, RestrictedPodSecurityContext(), pod.Spec.SecurityContext)
	assert.Equal(t, RestrictedSecurityContext(), container.SecurityContext)

	require.NotEmpty(t, pod.Spec.ImagePullSecrets)
	pod.Spec.ImagePullSecrets[0].Name = "mutated-pod"
	assert.Equal(t, []corev1.LocalObjectReference{{Name: "registry-creds"}}, helper.buildWriteFilePod("file", "", nil, nil).Spec.ImagePullSecrets)
}

func TestPvcHelperBuildDownloadGenesisPodUsesDefaultImageAndSecrets(t *testing.T) {
	helper := NewPvcHelper(nil, nil, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: "data", Namespace: "default",
	}}, "", []corev1.LocalObjectReference{{Name: "registry-creds"}})

	sha := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	pod := helper.buildDownloadGenesisPod("https://example.com/genesis.json.zst", "config/genesis.json", &sha, "priority", nil, nil)
	require.Len(t, pod.Spec.Containers, 1)
	container := pod.Spec.Containers[0]
	assert.Equal(t, images.DefaultUtilityImage, container.Image)
	assert.Equal(t, []corev1.LocalObjectReference{{Name: "registry-creds"}}, pod.Spec.ImagePullSecrets)
	assert.Equal(t, []string{"/bin/sh"}, container.Command)
	require.GreaterOrEqual(t, len(container.Args), 8)
	assert.Equal(t, "-c", container.Args[0])
	assert.Equal(t, "genesis-download", container.Args[2])
	assert.Equal(t, "https://example.com/genesis.json.zst", container.Args[3])
	assert.Equal(t, "/pvc/config/genesis.json", container.Args[4])
	assert.Equal(t, "zstd", container.Args[5])
	assert.Equal(t, "1", container.Args[6])
	assert.Equal(t, sha, container.Args[7])
	assert.False(t, container.Stdin)
	require.NotNil(t, pod.Spec.TerminationGracePeriodSeconds)
	assert.Positive(t, *pod.Spec.TerminationGracePeriodSeconds)
	assert.Equal(t, ptr.To[int64](4500), pod.Spec.ActiveDeadlineSeconds)
	assert.Equal(t, RestrictedPodSecurityContext(), pod.Spec.SecurityContext)
	assert.Equal(t, RestrictedSecurityContext(), container.SecurityContext)
}
