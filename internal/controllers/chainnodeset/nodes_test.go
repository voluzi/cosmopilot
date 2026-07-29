package chainnodeset

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
)

// TestEnsureNodesInstanceCount verifies that a group validator does not add an extra
// status.instances count beyond the group's own instances. Only the legacy singleton
// .spec.validator adds the additional +1.
func TestEnsureNodesInstanceCount(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))

	tests := []struct {
		name          string
		validator     *appsv1.NodeSetValidatorConfig
		nodes         []appsv1.NodeGroupSpec
		wantInstances int
	}{
		{
			name: "group validator is counted once via group instances",
			nodes: []appsv1.NodeGroupSpec{
				{Name: "fullnodes", Instances: ptr.To(1)},
				{Name: "validators", Instances: ptr.To(2), Validator: &appsv1.NodeSetValidatorConfig{}},
			},
			wantInstances: 3,
		},
		{
			name:      "legacy singleton validator adds an extra instance",
			validator: &appsv1.NodeSetValidatorConfig{},
			nodes: []appsv1.NodeGroupSpec{
				{Name: "fullnodes", Instances: ptr.To(2)},
			},
			wantInstances: 3,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeSet := &appsv1.ChainNodeSet{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-nodeset",
					Namespace: "default",
					UID:       types.UID("test-uid"),
				},
				Spec: appsv1.ChainNodeSetSpec{
					Genesis:   &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
					Validator: tt.validator,
					Nodes:     tt.nodes,
				},
				Status: appsv1.ChainNodeSetStatus{ChainID: "test-chain"},
			}

			cl := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(&appsv1.ChainNodeSet{}).
				WithObjects(nodeSet).
				Build()

			r := &Reconciler{
				Client:   cl,
				Scheme:   scheme,
				recorder: record.NewFakeRecorder(100),
			}

			require.NoError(t, r.ensureNodes(context.Background(), nodeSet))
			assert.Equal(t, tt.wantInstances, nodeSet.Status.Instances)
		})
	}
}

// TestEnsureNodesReadyInstances verifies that .status.readyInstances counts only children whose
// phase is Running, Syncing or Snapshotting, so a degraded nodeset is visible from the CR alone.
func TestEnsureNodesReadyInstances(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))

	mkNodeIn := func(namespace, group string, index int, phase appsv1.ChainNodePhase, validator bool) *appsv1.ChainNode {
		labels := map[string]string{
			controllers.LabelChainNodeSet:      "test-nodeset",
			controllers.LabelChainNodeSetGroup: group,
		}
		if validator {
			labels[controllers.LabelChainNodeSetValidator] = controllers.StringValueTrue
		}
		return &appsv1.ChainNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("test-nodeset-%s-%d", group, index),
				Namespace: namespace,
				Labels:    labels,
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: appsv1.GroupVersion.String(),
					Kind:       "ChainNodeSet",
					Name:       "test-nodeset",
					UID:        types.UID("u"),
					Controller: ptr.To(true),
				}},
			},
			Status: appsv1.ChainNodeStatus{Phase: phase},
		}
	}
	mkNode := func(group string, index int, phase appsv1.ChainNodePhase, validator bool) *appsv1.ChainNode {
		return mkNodeIn("default", group, index, phase, validator)
	}
	// A standalone ChainNode that merely carries the user-settable nodeset label. It has no group
	// label, so the deleted-group cleanup leaves it alone and it survives to the counting loop.
	unowned := func(node *appsv1.ChainNode) *appsv1.ChainNode {
		node.OwnerReferences = nil
		delete(node.Labels, controllers.LabelChainNodeSetGroup)
		return node
	}
	// A ChainNode needs a finalizer for the fake client to keep it around with a deletion timestamp.
	terminating := func(node *appsv1.ChainNode) *appsv1.ChainNode {
		node.Finalizers = []string{"cosmopilot.voluzi.com/test"}
		node.DeletionTimestamp = ptr.To(metav1.Now())
		return node
	}
	stopsForSnapshots := func(node *appsv1.ChainNode) *appsv1.ChainNode {
		node.Spec.Persistence = &appsv1.Persistence{
			Snapshots: &appsv1.VolumeSnapshotsConfig{StopNode: ptr.To(true)},
		}
		return node
	}

	tests := []struct {
		name      string
		nodes     []appsv1.NodeGroupSpec
		children  []*appsv1.ChainNode
		wantReady int
	}{
		{
			name:  "degraded validator group is not fully ready",
			nodes: []appsv1.NodeGroupSpec{{Name: "validators", Instances: ptr.To(3), Validator: &appsv1.NodeSetValidatorConfig{}}},
			children: []*appsv1.ChainNode{
				mkNode("validators", 0, appsv1.PhaseChainNodeRunning, true),
				mkNode("validators", 1, appsv1.PhaseChainNodeRestarting, true),
				mkNode("validators", 2, appsv1.PhaseChainNodeError, true),
			},
			wantReady: 1,
		},
		{
			name:  "syncing and snapshotting nodes count as ready",
			nodes: []appsv1.NodeGroupSpec{{Name: "fullnodes", Instances: ptr.To(3)}},
			children: []*appsv1.ChainNode{
				mkNode("fullnodes", 0, appsv1.PhaseChainNodeSyncing, false),
				mkNode("fullnodes", 1, appsv1.PhaseChainNodeSnapshotting, false),
				mkNode("fullnodes", 2, appsv1.PhaseChainNodeRunning, false),
			},
			wantReady: 3,
		},
		{
			name:  "nodes that have not reported a phase are not ready",
			nodes: []appsv1.NodeGroupSpec{{Name: "fullnodes", Instances: ptr.To(2)}},
			children: []*appsv1.ChainNode{
				mkNode("fullnodes", 0, appsv1.PhaseChainNodeRunning, false),
				mkNode("fullnodes", 1, "", false),
			},
			wantReady: 1,
		},
		{
			// The manager cache is cluster-wide, so an identically named ChainNodeSet in another
			// namespace must not contribute to this one's readiness.
			name:  "ready nodes of a same-named nodeset in another namespace are not counted",
			nodes: []appsv1.NodeGroupSpec{{Name: "fullnodes", Instances: ptr.To(1)}},
			children: []*appsv1.ChainNode{
				mkNode("fullnodes", 0, appsv1.PhaseChainNodeError, false),
				mkNodeIn("other", "fullnodes", 0, appsv1.PhaseChainNodeRunning, false),
			},
			wantReady: 0,
		},
		{
			// The nodeset label is user-settable, so a standalone ChainNode wearing it must not be
			// counted as an instance of this nodeset.
			name:  "same-named-label nodes not controlled by this nodeset are not counted",
			nodes: []appsv1.NodeGroupSpec{{Name: "fullnodes", Instances: ptr.To(1)}},
			children: []*appsv1.ChainNode{
				mkNode("fullnodes", 0, appsv1.PhaseChainNodeError, false),
				unowned(mkNode("standalone", 0, appsv1.PhaseChainNodeRunning, false)),
			},
			wantReady: 0,
		},
		{
			// A node deleted by a scale-down keeps its last phase until finalizers complete, but is
			// already excluded from .status.instances — counting it could report ready > instances.
			name:  "children being deleted are not counted",
			nodes: []appsv1.NodeGroupSpec{{Name: "fullnodes", Instances: ptr.To(1)}},
			children: []*appsv1.ChainNode{
				mkNode("fullnodes", 0, appsv1.PhaseChainNodeRunning, false),
				terminating(mkNode("fullnodes", 1, appsv1.PhaseChainNodeRunning, false)),
			},
			wantReady: 1,
		},
		{
			// With stopNode the pod is deleted for the duration of the snapshot, so the node holds
			// the Snapshotting phase while being down.
			name: "snapshotting nodes that stop for the snapshot are not ready",
			nodes: []appsv1.NodeGroupSpec{
				{Name: "stopping", Instances: ptr.To(1), Persistence: &appsv1.Persistence{
					Snapshots: &appsv1.VolumeSnapshotsConfig{StopNode: ptr.To(true)},
				}},
				{Name: "serving", Instances: ptr.To(1)},
			},
			children: []*appsv1.ChainNode{
				stopsForSnapshots(mkNode("stopping", 0, appsv1.PhaseChainNodeSnapshotting, false)),
				mkNode("serving", 0, appsv1.PhaseChainNodeSnapshotting, false),
			},
			wantReady: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeSet := &appsv1.ChainNodeSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: types.UID("u")},
				Spec: appsv1.ChainNodeSetSpec{
					Genesis: &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
					Nodes:   tt.nodes,
				},
				Status: appsv1.ChainNodeSetStatus{ChainID: "test-chain"},
			}

			builder := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(&appsv1.ChainNodeSet{}, &appsv1.ChainNode{}).
				WithObjects(nodeSet)
			for _, child := range tt.children {
				builder = builder.WithObjects(child)
			}
			r := &Reconciler{Client: builder.Build(), Scheme: scheme, recorder: record.NewFakeRecorder(100)}

			require.NoError(t, r.ensureNodes(context.Background(), nodeSet))
			assert.Equal(t, tt.wantReady, nodeSet.Status.ReadyInstances)

			// The count must also be persisted, not just set on the in-memory object.
			persisted := &appsv1.ChainNodeSet{}
			require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-nodeset"}, persisted))
			assert.Equal(t, tt.wantReady, persisted.Status.ReadyInstances)
		})
	}
}

// TestEnsureNodesRemovesStaleRegularNodesOnValidatorPromotion verifies that when a group is
// changed from a regular group to a validator group, the old regular ChainNodes that are no
// longer desired are removed, while the validator ChainNode (labelled validator=true, reconciled
// by ensureValidator) is kept.
func TestEnsureNodesRemovesStaleRegularNodesOnValidatorPromotion(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))

	// Index 0 has already been promoted to a validator by ensureValidator (validator=true).
	validator0 := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-nodeset-validators-0",
			Namespace: "default",
			Labels: map[string]string{
				controllers.LabelChainNodeSet:          "test-nodeset",
				controllers.LabelChainNodeSetGroup:     "validators",
				controllers.LabelChainNodeSetValidator: controllers.StringValueTrue,
			},
		},
	}
	// Indices 1 and 2 are stale regular ChainNodes left from when the group ran 3 regular instances.
	mkRegular := func(index int) *appsv1.ChainNode {
		return &appsv1.ChainNode{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("test-nodeset-validators-%d", index),
				Namespace: "default",
				Labels: map[string]string{
					controllers.LabelChainNodeSet:      "test-nodeset",
					controllers.LabelChainNodeSetGroup: "validators",
				},
			},
		}
	}
	stale1 := mkRegular(1)
	stale2 := mkRegular(2)

	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: types.UID("u")},
		Spec: appsv1.ChainNodeSetSpec{
			Genesis: &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
			Nodes: []appsv1.NodeGroupSpec{{
				Name:      "validators",
				Instances: ptr.To(1),
				Validator: &appsv1.NodeSetValidatorConfig{},
			}},
		},
		Status: appsv1.ChainNodeSetStatus{ChainID: "test-chain"},
	}

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&appsv1.ChainNodeSet{}).
		WithObjects(nodeSet, validator0, stale1, stale2).
		Build()
	r := &Reconciler{Client: cl, Scheme: scheme, recorder: record.NewFakeRecorder(100)}

	require.NoError(t, r.ensureNodes(context.Background(), nodeSet))

	// The stale regular ChainNodes must be removed.
	for _, name := range []string{"test-nodeset-validators-1", "test-nodeset-validators-2"} {
		err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, &appsv1.ChainNode{})
		assert.Truef(t, errors.IsNotFound(err), "stale regular ChainNode %s must be deleted", name)
	}

	// The validator ChainNode must be kept.
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-nodeset-validators-0"}, &appsv1.ChainNode{}),
		"desired validator ChainNode must not be deleted")
}

// TestGetNodeSpecInheritsGasPriceWithoutAppTomlOverride verifies that inheriting the validator
// minimum gas price into a group whose Config.Override only contains a non-app.toml file does not
// fail trying to unmarshal an absent app.toml entry; the app.toml entry is added instead.
func TestGetNodeSpecInheritsGasPriceWithoutAppTomlOverride(t *testing.T) {
	appToml, err := json.Marshal(map[string]string{controllers.MinimumGasPricesKey: "0.025stake"})
	require.NoError(t, err)

	// A group override that only configures config.toml (no app.toml entry).
	groupOverride := map[string]runtime.RawExtension{
		"config.toml": {Raw: []byte(`{"moniker":"x"}`)},
	}

	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: types.UID("u")},
		Spec: appsv1.ChainNodeSetSpec{
			Genesis: &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
			Validator: &appsv1.NodeSetValidatorConfig{
				Config: &appsv1.Config{Override: &map[string]runtime.RawExtension{
					controllers.AppTomlFile: {Raw: appToml},
				}},
			},
			Nodes: []appsv1.NodeGroupSpec{{
				Name:      "fullnodes",
				Instances: ptr.To(1),
				Config:    &appsv1.Config{Override: &groupOverride},
			}},
		},
		Status: appsv1.ChainNodeSetStatus{ChainID: "test-chain"},
	}
	r := newValidatorTestReconciler(t, nodeSet)

	node, err := r.getNodeSpec(nodeSet, nodeSet.Spec.Nodes[0], 0)
	require.NoError(t, err)
	require.NotNil(t, node.Spec.Config)
	require.NotNil(t, node.Spec.Config.Override)

	override := *node.Spec.Config.Override
	raw, ok := override[controllers.AppTomlFile]
	require.True(t, ok, "app.toml override must be added")
	var appCfg map[string]interface{}
	require.NoError(t, json.Unmarshal(raw.Raw, &appCfg))
	assert.Equal(t, "0.025stake", appCfg[controllers.MinimumGasPricesKey])
	// The pre-existing config.toml override must be preserved.
	assert.Contains(t, override, "config.toml")
}

// TestGetNodeSpecStampsValidatorFalseLabel verifies that regular group ChainNodes are stamped with
// the internal validator=false label even when the parent ChainNodeSet carries a user label
// validator=true, so the validator cleanup selector never treats them as stale validators.
func TestGetNodeSpecStampsValidatorFalseLabel(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-nodeset",
			Namespace: "default",
			UID:       types.UID("u"),
			// A user label that happens to collide with the internal validator label key.
			Labels: map[string]string{controllers.LabelChainNodeSetValidator: controllers.StringValueTrue},
		},
		Spec: appsv1.ChainNodeSetSpec{
			Nodes: []appsv1.NodeGroupSpec{{Name: "fullnodes", Instances: ptr.To(1)}},
		},
		Status: appsv1.ChainNodeSetStatus{ChainID: "test-chain"},
	}
	r := newValidatorTestReconciler(t, nodeSet)

	node, err := r.getNodeSpec(nodeSet, nodeSet.Spec.Nodes[0], 0)
	require.NoError(t, err)
	assert.Equal(t, controllers.StringValueFalse, node.Labels[controllers.LabelChainNodeSetValidator],
		"regular node must be stamped validator=false even when the parent has a user validator=true label")
}

// TestGetNodeSpecStripsDashboardExposureFromChildren verifies a generated child does not inherit the
// group's dashboard Ingress/Gateway. The group's single dashboard host is published once by the
// group guard; a child that manages its own guard (one declaring individual routes) would otherwise
// publish a competing backend for that same host, leaving route precedence to pick a winner.
// The rest of the dashboard config is inherited so the child's own dashboard still runs.
func TestGetNodeSpecStripsDashboardExposureFromChildren(t *testing.T) {
	httpsSection := "https-dashboard"
	group := appsv1.NodeGroupSpec{
		Name:      "fullnodes",
		Instances: ptr.To(2),
		Config: &appsv1.Config{
			CosmoGuard: &appsv1.CosmoGuardConfig{
				Enable: true,
				Dashboard: &appsv1.CosmoGuardDashboardConfig{
					Enable: true,
					Port:   ptr.To[int32](9100),
					Ingress: &appsv1.CosmoGuardDashboardIngress{
						Host: "guard.example.com",
					},
					Gateway: &appsv1.CosmoGuardDashboardGateway{
						Host:    "guard.example.com",
						Gateway: appsv1.GatewayRef{Name: "external", SectionName: &httpsSection},
					},
				},
			},
		},
	}
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "chain", Namespace: "default", UID: types.UID("u")},
		Spec:       appsv1.ChainNodeSetSpec{Nodes: []appsv1.NodeGroupSpec{group}},
		Status:     appsv1.ChainNodeSetStatus{ChainID: "test-chain"},
	}
	r := newValidatorTestReconciler(t, nodeSet)

	node, err := r.getNodeSpec(nodeSet, nodeSet.Spec.Nodes[0], 0)
	require.NoError(t, err)

	childDashboard := node.Spec.Config.GetCosmoGuardDashboard()
	require.NotNil(t, childDashboard, "the dashboard itself is still inherited")
	assert.Nil(t, childDashboard.Ingress, "child must not inherit the group dashboard Ingress")
	assert.Nil(t, childDashboard.Gateway, "child must not inherit the group dashboard Gateway")
	assert.True(t, childDashboard.Enable, "the dashboard still runs on the child's own guard")
	require.NotNil(t, childDashboard.Port)
	assert.Equal(t, int32(9100), *childDashboard.Port, "port is inherited unchanged")

	// The group's own spec must be untouched — the strip works on a copy, so the ChainNodeSet
	// controller still renders the group guard's dashboard exposure from it.
	assert.NotNil(t, nodeSet.Spec.Nodes[0].Config.GetCosmoGuardDashboard().Ingress,
		"stripping the child copy must not mutate the group config")
	assert.NotNil(t, nodeSet.Spec.Nodes[0].Config.GetCosmoGuardDashboard().Gateway,
		"stripping the child copy must not mutate the group config")
}
