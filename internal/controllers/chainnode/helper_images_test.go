package chainnode

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	appsv1 "github.com/voluzi/cosmopilot/v3/api/v1"
	"github.com/voluzi/cosmopilot/v3/internal/controllers"
)

func TestGetPodSpecUsesUtilityImageForDataVolumeGenesisLink(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "node", Namespace: "default", UID: "node-uid"},
		Spec: appsv1.ChainNodeSpec{
			App: appsv1.AppSpec{Image: "registry.example.com/app", Version: ptr.To("v1"), App: "appd"},
			Genesis: &appsv1.GenesisConfig{
				UseDataVolume: ptr.To(true),
			},
			Config: &appsv1.Config{ImagePullSecrets: []corev1.LocalObjectReference{{Name: "registry-creds"}}},
		},
		Status: appsv1.ChainNodeStatus{ChainID: "chain", NodeID: "node-id"},
	}
	config := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: chainNode.Name, Namespace: chainNode.Namespace}}
	reconciler := &Reconciler{
		Client: fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(config).Build(),
		Scheme: scheme,
		opts: &controllers.ControllerRunOptions{
			NodeUtilsImage: "registry.example.com/node-utils:v1",
			UtilityImage:   "registry.example.com:5000/tools:custom",
		},
	}

	pod, err := reconciler.getPodSpec(context.Background(), chainNode, "config-hash", "shutdown-secret")
	require.NoError(t, err)
	link := requireNamedContainer(t, pod.Spec.InitContainers, "link-genesis")
	assert.Equal(t, "registry.example.com:5000/tools:custom", link.Image)
	assert.Equal(t, []string{"/bin/sh"}, link.Command)
	assert.Equal(t, []string{"-c", "ln -s /home/app/data/genesis.json /home/app/config/genesis.json"}, link.Args)
	assert.Equal(t, chainNode.Spec.Config.ImagePullSecrets, pod.Spec.ImagePullSecrets)
}

func requireNamedContainer(t *testing.T, containers []corev1.Container, name string) corev1.Container {
	t.Helper()
	for _, container := range containers {
		if container.Name == name {
			return container
		}
	}
	require.FailNow(t, "container not found", name)
	return corev1.Container{}
}
