package chainnodeset

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/chainutils"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
	"github.com/voluzi/cosmopilot/v2/internal/cosmoguard"
)

func guardedNodeSet() (*appsv1.ChainNodeSet, appsv1.NodeGroupSpec) {
	group := appsv1.NodeGroupSpec{
		Name: "fullnodes",
		Config: &appsv1.Config{
			CosmoGuard: &appsv1.CosmoGuardConfig{
				Enable: true,
				Config: &corev1.ConfigMapKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "rules"},
					Key:                  "cosmoguard.yaml",
				},
			},
		},
	}
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "chain", Namespace: "ns"},
		Spec:       appsv1.ChainNodeSetSpec{Nodes: []appsv1.NodeGroupSpec{group}},
	}
	return nodeSet, group
}

// TestGroupServiceFlipsToGuardOnlyWhenReady verifies the group Service targets the node pods on raw
// ports until the guard has rolled out, then flips its selector and target ports to the guard.
func TestGroupServiceFlipsToGuardOnlyWhenReady(t *testing.T) {
	nodeSet, group := guardedNodeSet()
	r := newValidatorTestReconciler(t, nodeSet)

	// Not ready: node selector + raw ports.
	svc, err := r.getServiceSpec(nodeSet, group, false)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		controllers.LabelChainNodeSet:      "chain",
		controllers.LabelChainNodeSetGroup: "fullnodes",
	}, svc.Spec.Selector)
	assert.Equal(t, int32(chainutils.RpcPort), svc.Spec.Ports[0].TargetPort.IntVal)

	// Ready: guard selector + guard listener target ports.
	svc, err = r.getServiceSpec(nodeSet, group, true)
	require.NoError(t, err)
	assert.Equal(t, cosmoguard.InstanceLabels(groupCosmoGuardName(nodeSet, group)), svc.Spec.Selector)
	assert.Equal(t, int32(controllers.CosmoGuardRpcPort), svc.Spec.Ports[0].TargetPort.IntVal)
	assert.Equal(t, int32(controllers.CosmoGuardLcdPort), svc.Spec.Ports[1].TargetPort.IntVal)
	assert.Equal(t, int32(controllers.CosmoGuardGrpcPort), svc.Spec.Ports[2].TargetPort.IntVal)
	// Public port numbers are preserved.
	assert.Equal(t, int32(chainutils.RpcPort), svc.Spec.Ports[0].Port)
}

// TestGuardParamsUseDiscovery verifies a group's guard is configured to discover node pods through
// the headless upstream Service.
func TestGuardParamsUseDiscovery(t *testing.T) {
	nodeSet, group := guardedNodeSet()
	r := newValidatorTestReconciler(t, nodeSet)

	p := r.groupCosmoGuardParams(nodeSet, group)
	assert.Equal(t, "chain-fullnodes-cosmoguard-upstream.ns.svc.cluster.local", p.DiscoveryHost)
	assert.Empty(t, p.UpstreamHost)
	assert.Equal(t, "rules", p.ConfigMap.Name)
}

// TestUpstreamServiceIsHeadless verifies the guard's upstream Service is headless, does NOT publish
// not-ready addresses (so only ready node pods are discoverable), and selects the group's node pods
// on raw ports.
func TestUpstreamServiceIsHeadless(t *testing.T) {
	nodeSet, group := guardedNodeSet()
	r := newValidatorTestReconciler(t, nodeSet)

	svc, err := r.buildGroupCosmoGuardUpstreamService(nodeSet, group)
	require.NoError(t, err)
	assert.Equal(t, corev1.ClusterIPNone, svc.Spec.ClusterIP)
	// Upstream discovery must only surface READY node pods, so the guard never routes client traffic
	// to syncing/upgrading/snapshotting nodes.
	assert.False(t, svc.Spec.PublishNotReadyAddresses)
	assert.Equal(t, map[string]string{
		controllers.LabelChainNodeSet:      "chain",
		controllers.LabelChainNodeSetGroup: "fullnodes",
	}, svc.Spec.Selector)
	assert.Equal(t, int32(chainutils.RpcPort), svc.Spec.Ports[0].TargetPort.IntVal)
}

func TestCosmoGuardRouteReady(t *testing.T) {
	nodeSet, _ := guardedNodeSet()

	// Single guarded group: flip only once its guard is ready.
	assert.False(t, cosmoGuardRouteReady(nodeSet, []string{"fullnodes"}, map[string]bool{"fullnodes": false}))
	assert.True(t, cosmoGuardRouteReady(nodeSet, []string{"fullnodes"}, map[string]bool{"fullnodes": true}))

	// The reserved validator group has no managed guard: a route including it must NOT flip, or its
	// endpoints would be dropped.
	assert.False(t, cosmoGuardRouteReady(nodeSet, []string{appsv1.ReservedValidatorGroupName}, map[string]bool{}))
	// Mixed route (guarded group + validator): still must not flip.
	assert.False(t, cosmoGuardRouteReady(nodeSet, []string{"fullnodes", appsv1.ReservedValidatorGroupName}, map[string]bool{"fullnodes": true}))

	// Unknown group / empty route: never flip.
	assert.False(t, cosmoGuardRouteReady(nodeSet, []string{"nope"}, map[string]bool{"nope": true}))
	assert.False(t, cosmoGuardRouteReady(nodeSet, nil, map[string]bool{}))
}
