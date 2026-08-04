package chainnode

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
)

func TestNewAppPropagatesStandaloneChainNodeEnvWithoutAliasing(t *testing.T) {
	env := []corev1.EnvVar{
		{Name: "PLAIN", Value: "value"},
		{
			Name: "FROM_SECRET",
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "app-secret"},
				Key:                  "token",
			}},
		},
	}
	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "node", Namespace: "default", UID: "node-uid"},
		Spec: appsv1.ChainNodeSpec{
			App: appsv1.AppSpec{Image: "example/app", Version: ptr.To("v1"), App: "appd"},
			Config: &appsv1.Config{
				Env: env,
			},
		},
	}
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	reconciler := &Reconciler{Scheme: scheme, opts: &controllers.ControllerRunOptions{}}

	app, err := reconciler.newApp(chainNode)
	require.NoError(t, err)
	pod, err := app.BuildInitPod(&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "node"}}, nil)
	require.NoError(t, err)
	require.Equal(t, env, pod.Spec.InitContainers[0].Env)

	container := reconciler.buildAppContainer(chainNode, nil, "/ready", corev1.ResourceRequirements{}, nil)
	require.Equal(t, env, container.Env)

	pod.Spec.InitContainers[0].Env[0].Value = "mutated-pod"
	pod.Spec.InitContainers[0].Env[1].ValueFrom.SecretKeyRef.Name = "mutated-pod-secret"
	container.Env[0].Value = "mutated-container"
	container.Env[1].ValueFrom.SecretKeyRef.Name = "mutated-container-secret"

	assert.Equal(t, "value", chainNode.Spec.Config.Env[0].Value)
	assert.Equal(t, "app-secret", chainNode.Spec.Config.Env[1].ValueFrom.SecretKeyRef.Name)
}
