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
