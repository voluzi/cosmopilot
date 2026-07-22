package chainnodeset

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

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

// TestGroupGuardScheduling verifies the group guard follows the placement of the pods it fronts: a
// regular group uses the group-level nodeSelector/affinity and the nodes' priority + the group
// Config's ServiceAccount; a validator group uses the validator sub-config's nodeSelector/affinity,
// the validators' priority, and the validator Config's ServiceAccount.
func TestGroupGuardScheduling(t *testing.T) {
	// Regular group: group-level placement, nodes priority, group Config SA.
	regularNodeSet, regularGroup := guardedNodeSet()
	regularGroup.NodeSelector = map[string]string{"pool": "nodes"}
	regularGroup.Affinity = &corev1.Affinity{}
	regularGroup.Config.ServiceAccountName = ptr.To("nodes-sa")
	regularNodeSet.Spec.Nodes = []appsv1.NodeGroupSpec{regularGroup}

	r := newValidatorTestReconciler(t, regularNodeSet)
	r.opts = &controllers.ControllerRunOptions{ReleaseName: "rel"}

	p := r.groupCosmoGuardParams(regularNodeSet, regularGroup)
	assert.Equal(t, map[string]string{"pool": "nodes"}, p.NodeSelector)
	assert.Equal(t, regularGroup.Affinity, p.Affinity)
	assert.Equal(t, "rel-nodes", p.PriorityClassName)
	assert.Equal(t, "nodes-sa", p.ServiceAccountName)

	// Validator group: the pods are rendered from group.Validator, so the guard must follow it.
	valAffinity := &corev1.Affinity{}
	valGroup := appsv1.NodeGroupSpec{
		Name: "validators",
		Validator: &appsv1.NodeSetValidatorConfig{
			NodeSelector: map[string]string{"pool": "validators"},
			Affinity:     valAffinity,
			Config: &appsv1.Config{
				ServiceAccountName: ptr.To("validators-sa"),
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
	valNodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "chain", Namespace: "ns"},
		Spec:       appsv1.ChainNodeSetSpec{Nodes: []appsv1.NodeGroupSpec{valGroup}},
	}

	p = r.groupCosmoGuardParams(valNodeSet, valGroup)
	assert.Equal(t, map[string]string{"pool": "validators"}, p.NodeSelector)
	assert.Equal(t, valAffinity, p.Affinity)
	assert.Equal(t, "rel-validators", p.PriorityClassName)
	assert.Equal(t, "validators-sa", p.ServiceAccountName)
}

// TestZeroInstanceGroupSkipsGuard verifies a group scaled to instances:0 (still CosmoGuard-enabled)
// gets no guard: it is absent from the expected set (so cleanup removes any prior guard), never marked
// ready (so its Service falls back to the empty raw selector), and is not route-guardable.
func TestZeroInstanceGroupSkipsGuard(t *testing.T) {
	nodeSet, group := guardedNodeSet()
	group.Instances = ptr.To(0)
	nodeSet.Spec.Nodes = []appsv1.NodeGroupSpec{group}
	r := newValidatorTestReconciler(t, nodeSet)

	res, err := r.ensureCosmoGuards(context.Background(), nodeSet)
	require.NoError(t, err)

	name := groupCosmoGuardName(nodeSet, group)
	assert.NotContains(t, res.expected, name, "no guard expected for a zero-instance group")
	assert.False(t, res.ready[group.Name], "zero-instance group must not flip its Service to a guard")
	assert.False(t, cosmoGuardRouteGuardable(nodeSet, []string{group.Name}), "route over a zero-instance group is not guardable")
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

// TestServiceSelectsGuard verifies the sticky-flip detector recognizes a Service already flipped to
// the guard, so a guarded Service is kept on the guard through transient rollout un-readiness.
func TestServiceSelectsGuard(t *testing.T) {
	flipped := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "chain-fullnodes", Namespace: "ns"},
		Spec:       corev1.ServiceSpec{Selector: cosmoguard.InstanceLabels("chain-fullnodes-cosmoguard")},
	}
	raw := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "chain-other", Namespace: "ns"},
		Spec: corev1.ServiceSpec{Selector: map[string]string{
			controllers.LabelChainNodeSet: "chain", controllers.LabelChainNodeSetGroup: "other",
		}},
	}
	r := newValidatorTestReconciler(t, flipped, raw)

	assert.True(t, r.serviceSelectsGuard(context.Background(), "ns", "chain-fullnodes"))
	assert.False(t, r.serviceSelectsGuard(context.Background(), "ns", "chain-other"))
	assert.False(t, r.serviceSelectsGuard(context.Background(), "ns", "missing"))
}

func TestCosmoGuardRouteGuardable(t *testing.T) {
	nodeSet, _ := guardedNodeSet()

	// A guarded .spec.nodes group -> guardable.
	assert.True(t, cosmoGuardRouteGuardable(nodeSet, []string{"fullnodes"}))
	// Validator group, unknown group, mixed, or empty -> NOT guardable (structural; not sticky).
	assert.False(t, cosmoGuardRouteGuardable(nodeSet, []string{appsv1.ReservedValidatorGroupName}))
	assert.False(t, cosmoGuardRouteGuardable(nodeSet, []string{"fullnodes", appsv1.ReservedValidatorGroupName}))
	assert.False(t, cosmoGuardRouteGuardable(nodeSet, []string{"nope"}))
	assert.False(t, cosmoGuardRouteGuardable(nodeSet, nil))
}

func TestCosmoGuardRouteReady(t *testing.T) {
	nodeSet, _ := guardedNodeSet()

	// Readiness: every group's guard must be ready.
	assert.False(t, cosmoGuardRouteReady(nodeSet, []string{"fullnodes"}, map[string]bool{"fullnodes": false}))
	assert.True(t, cosmoGuardRouteReady(nodeSet, []string{"fullnodes"}, map[string]bool{"fullnodes": true}))
	assert.False(t, cosmoGuardRouteReady(nodeSet, []string{"a", "b"}, map[string]bool{"a": true, "b": false}))
}
