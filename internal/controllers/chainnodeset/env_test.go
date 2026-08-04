package chainnodeset

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
)

func nodeSetAppEnv() []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "PLAIN", Value: "value"},
		{
			Name: "FROM_SECRET",
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "app-secret"},
				Key:                  "token",
			}},
		},
	}
}

func TestGeneratedChainNodeEnvIsPropagatedWithoutParentAliasing(t *testing.T) {
	t.Run("regular group", func(t *testing.T) {
		nodeSet := &appsv1.ChainNodeSet{
			ObjectMeta: metav1.ObjectMeta{Name: "nodes", Namespace: "default", UID: types.UID("nodes-uid")},
			Spec: appsv1.ChainNodeSetSpec{
				Genesis: &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
				Nodes: []appsv1.NodeGroupSpec{{
					Name:   "fullnodes",
					Config: &appsv1.Config{Env: nodeSetAppEnv()},
				}},
			},
			Status: appsv1.ChainNodeSetStatus{ChainID: "chain"},
		}
		reconciler := newValidatorTestReconciler(t)

		first, err := reconciler.getNodeSpec(nodeSet, nodeSet.Spec.Nodes[0], 0)
		require.NoError(t, err)
		require.Equal(t, nodeSetAppEnv(), first.Spec.Config.Env)
		first.Spec.Config.Env[0].Value = "mutated"
		first.Spec.Config.Env[1].ValueFrom.SecretKeyRef.Name = "mutated-secret"

		assert.Equal(t, nodeSetAppEnv(), nodeSet.Spec.Nodes[0].Config.Env)
		second, err := reconciler.getNodeSpec(nodeSet, nodeSet.Spec.Nodes[0], 0)
		require.NoError(t, err)
		assert.Equal(t, nodeSetAppEnv(), second.Spec.Config.Env)
	})

	t.Run("validator group", func(t *testing.T) {
		nodeSet := &appsv1.ChainNodeSet{
			ObjectMeta: metav1.ObjectMeta{Name: "nodes", Namespace: "default", UID: types.UID("nodes-uid")},
			Spec: appsv1.ChainNodeSetSpec{
				Genesis: &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
				Nodes: []appsv1.NodeGroupSpec{{
					Name: "validators",
					Validator: &appsv1.NodeSetValidatorConfig{
						Config: &appsv1.Config{Env: nodeSetAppEnv()},
					},
				}},
			},
			Status: appsv1.ChainNodeSetStatus{ChainID: "chain"},
		}
		reconciler := newValidatorTestReconciler(t)

		first, err := reconciler.getValidatorSpec(nodeSet, "validators", 0, nodeSet.Spec.Nodes[0].Validator)
		require.NoError(t, err)
		require.Equal(t, nodeSetAppEnv(), first.Spec.Config.Env)
		first.Spec.Config.Env[0].Value = "mutated"
		first.Spec.Config.Env[1].ValueFrom.SecretKeyRef.Name = "mutated-secret"

		assert.Equal(t, nodeSetAppEnv(), nodeSet.Spec.Nodes[0].Validator.Config.Env)
		second, err := reconciler.getValidatorSpec(nodeSet, "validators", 0, nodeSet.Spec.Nodes[0].Validator)
		require.NoError(t, err)
		assert.Equal(t, nodeSetAppEnv(), second.Spec.Config.Env)
	})
}
