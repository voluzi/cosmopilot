package chainnode

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	k8sappsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
)

func cosmoGuardTestReconciler(t *testing.T, objs ...client.Object) *Reconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, k8sappsv1.AddToScheme(scheme))
	require.NoError(t, autoscalingv2.AddToScheme(scheme))
	require.NoError(t, networkingv1.AddToScheme(scheme))

	return &Reconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build(),
		Scheme: scheme,
		opts:   &controllers.ControllerRunOptions{CosmoGuardImage: "ghcr.io/voluzi/cosmoguard:4.0.0-rc.7"},
	}
}

func guardedChainNode(name string, child bool) *appsv1.ChainNode {
	labels := map[string]string{}
	if child {
		labels[controllers.LabelChainNodeSet] = "some-set"
	}
	return &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns", Labels: labels, UID: types.UID(name + "-uid")},
		Spec: appsv1.ChainNodeSpec{
			Config: &appsv1.Config{
				CosmoGuard: &appsv1.CosmoGuardConfig{
					Enable: true,
					Config: &corev1.ConfigMapKeySelector{
						LocalObjectReference: corev1.LocalObjectReference{Name: "rules"},
						Key:                  "cosmoguard.yaml",
					},
				},
			},
		},
	}
}

// TestStandaloneGuardCreatesStatefulSetAndService verifies a standalone ChainNode gets a clustered
// guard StatefulSet + client Service + headless peer Service + encryption Secret, pointed at its
// internal Service (static upstream).
func TestStandaloneGuardCreatesStatefulSetAndService(t *testing.T) {
	cn := guardedChainNode("node-0", false)
	r := cosmoGuardTestReconciler(t, cn)

	require.NoError(t, r.ensureCosmoGuard(context.Background(), cn))

	sts := &k8sappsv1.StatefulSet{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "node-0-cosmoguard"}, sts))

	env := sts.Spec.Template.Spec.Containers[0].Env
	found := false
	for _, e := range env {
		if e.Name == "COSMOGUARD_NODE_HOST" {
			found = true
			assert.Equal(t, cn.GetNodeFQDN(), e.Value)
		}
	}
	assert.True(t, found, "static upstream host must be injected")

	svc := &corev1.Service{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "node-0-cosmoguard"}, svc))

	// Headless peer Service + encryption Secret provisioned for the olric cluster.
	peer := &corev1.Service{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "node-0-cosmoguard-peer"}, peer))
	assert.Equal(t, corev1.ClusterIPNone, peer.Spec.ClusterIP)

	secret := &corev1.Secret{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "node-0-cosmoguard-cluster"}, secret))
	assert.NotEmpty(t, secret.Data["encryptionKey"])
}

// TestAPIServiceName verifies ingress/gateway route targets: a standalone guarded node points at its
// own guard Service, while a ChainNodeSet child points at the raw node Service (its group's guard is
// a separate Service, so "<child>-cosmoguard" would never exist).
func TestAPIServiceName(t *testing.T) {
	r := cosmoGuardTestReconciler(t)

	standalone := guardedChainNode("node-0", false)
	assert.Equal(t, "node-0-cosmoguard", r.apiServiceName(standalone))

	child := guardedChainNode("chain-fullnodes-0", true)
	assert.Equal(t, "chain-fullnodes-0", r.apiServiceName(child), "nodeset child must target the raw node service")

	unguarded := guardedChainNode("node-1", false)
	unguarded.Spec.Config.CosmoGuard.Enable = false
	assert.Equal(t, "node-1", r.apiServiceName(unguarded))
}

// TestDisableAutoscalingRemovesHPA verifies the standalone guard deletes its HPA when autoscaling is
// turned off, so it stops driving the StatefulSet's replica count.
func TestDisableAutoscalingRemovesHPA(t *testing.T) {
	cn := guardedChainNode("node-0", false)
	cn.Spec.Config.CosmoGuard.Autoscaling = &appsv1.CosmoGuardAutoscalingConfig{Enable: true, MaxReplicas: 5}
	r := cosmoGuardTestReconciler(t, cn)

	require.NoError(t, r.ensureCosmoGuard(context.Background(), cn))
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "node-0-cosmoguard"}, &autoscalingv2.HorizontalPodAutoscaler{}))

	cn.Spec.Config.CosmoGuard.Autoscaling.Enable = false
	require.NoError(t, r.ensureCosmoGuard(context.Background(), cn))
	err := r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "node-0-cosmoguard"}, &autoscalingv2.HorizontalPodAutoscaler{})
	assert.Error(t, err, "HPA should be removed when autoscaling is disabled")
}

// TestNodeSetChildSkipsStandaloneGuard verifies a ChainNodeSet child never creates its own guard
// (the group's guard, managed by the set, fronts it).
func TestNodeSetChildSkipsStandaloneGuard(t *testing.T) {
	cn := guardedChainNode("chain-fullnodes-0", true)
	r := cosmoGuardTestReconciler(t, cn)

	require.NoError(t, r.ensureCosmoGuard(context.Background(), cn))

	dep := &k8sappsv1.Deployment{}
	err := r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "chain-fullnodes-0-cosmoguard"}, dep)
	assert.Error(t, err, "no standalone guard should be created for a nodeset child")
}

// TestDisableGuardUndeploys verifies disabling CosmoGuard removes the previously-created guard.
func TestDisableGuardUndeploys(t *testing.T) {
	cn := guardedChainNode("node-0", false)
	r := cosmoGuardTestReconciler(t, cn)
	require.NoError(t, r.ensureCosmoGuard(context.Background(), cn))

	// Confirm it was created first, then disable and reconcile again.
	sts := &k8sappsv1.StatefulSet{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "node-0-cosmoguard"}, sts))

	cn.Spec.Config.CosmoGuard.Enable = false
	require.NoError(t, r.ensureCosmoGuard(context.Background(), cn))

	err := r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "node-0-cosmoguard"}, &k8sappsv1.StatefulSet{})
	assert.Error(t, err, "guard statefulset should be removed when disabled")
	err = r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "node-0-cosmoguard-peer"}, &corev1.Service{})
	assert.Error(t, err, "peer service should be removed when disabled")
	err = r.Get(context.Background(), client.ObjectKey{Namespace: "ns", Name: "node-0-cosmoguard-cluster"}, &corev1.Secret{})
	assert.Error(t, err, "encryption secret should be removed when disabled")
}
