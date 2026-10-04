package cosmosigner

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func TestExtensionsUnsetLifecycleDigest(t *testing.T) {
	p := testParams()
	digest, err := p.LifecycleDigest("signing-digest")
	require.NoError(t, err)
	require.Equal(t, "73c536e656812302bf05e77bc5a8f5a75663ab8baf99d92856affc096e208f95", digest)
}

func TestSignerExtensions(t *testing.T) {
	p := testParams()
	base, err := p.LifecycleDigest("signing-digest")
	require.NoError(t, err)
	p.Env = []corev1.EnvVar{{Name: "VENDOR_CONFIG", Value: "/vendor/client.conf"}}
	p.Volumes = []corev1.Volume{{Name: "vendor", VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: "vendor-config"}}}}}
	p.VolumeMounts = []corev1.VolumeMount{{Name: "vendor", MountPath: "/vendor", ReadOnly: true}}
	config, err := p.ConfigYAML()
	require.NoError(t, err)
	sts, err := p.StatefulSet(config)
	require.NoError(t, err)
	require.Contains(t, sts.Spec.Template.Spec.Containers[0].Env, p.Env[0])
	require.Contains(t, sts.Spec.Template.Spec.Volumes, p.Volumes[0])
	require.Contains(t, sts.Spec.Template.Spec.Containers[0].VolumeMounts, p.VolumeMounts[0])
	digest, err := p.LifecycleDigest("signing-digest")
	require.NoError(t, err)
	require.NotEqual(t, base, digest)
}
