package v1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/utils/ptr"
)

func TestDeletionPolicyDefaultsToRetain(t *testing.T) {
	tests := []struct {
		name   string
		policy *DeletionPolicy
	}{
		{name: "nil policy"},
		{name: "empty policy", policy: &DeletionPolicy{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, DeletionPolicyRetain, tt.policy.GetDataVolumes())
			assert.Equal(t, DeletionPolicyRetain, tt.policy.GetGeneratedKeys())
			assert.Equal(t, DeletionPolicyRetain, tt.policy.GetCosmosignerState())
		})
	}
}

func TestDeletionPolicyExplicitDelete(t *testing.T) {
	policy := &DeletionPolicy{
		DataVolumes:      ptr.To(DeletionPolicyDelete),
		GeneratedKeys:    ptr.To(DeletionPolicyDelete),
		CosmosignerState: ptr.To(DeletionPolicyDelete),
	}

	assert.Equal(t, DeletionPolicyDelete, policy.GetDataVolumes())
	assert.Equal(t, DeletionPolicyDelete, policy.GetGeneratedKeys())
	assert.Equal(t, DeletionPolicyDelete, policy.GetCosmosignerState())
}

func TestValidateDeletionPolicyRejectsUnknownValues(t *testing.T) {
	invalid := DeletionPolicyType("Destroy")
	policy := &DeletionPolicy{GeneratedKeys: &invalid}

	err := policy.Validate(".spec.deletionPolicy")
	require.Error(t, err)
	assert.Contains(t, err.Error(), ".spec.deletionPolicy.generatedKeys")
	assert.Contains(t, err.Error(), "Retain or Delete")
}

func TestChainNodeValidationRejectsInvalidDeletionPolicy(t *testing.T) {
	node := validChainNodeForDeletionPolicyTest()
	invalid := DeletionPolicyType("Destroy")
	node.Spec.DeletionPolicy = &DeletionPolicy{DataVolumes: &invalid}

	_, err := node.Validate(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), ".spec.deletionPolicy.dataVolumes")
}

func TestChainNodeSetValidationRejectsInvalidDeletionPolicy(t *testing.T) {
	nodeSet := validChainNodeSetForDeletionPolicyTest()
	invalid := DeletionPolicyType("Destroy")
	nodeSet.Spec.DeletionPolicy = &DeletionPolicy{CosmosignerState: &invalid}

	_, err := nodeSet.Validate(nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), ".spec.deletionPolicy.cosmosignerState")
}

func validChainNodeForDeletionPolicyTest() *ChainNode {
	return &ChainNode{Spec: ChainNodeSpec{
		Genesis: &GenesisConfig{ConfigMap: ptr.To("genesis")},
		App:     AppSpec{App: "chaind"},
	}}
}

func validChainNodeSetForDeletionPolicyTest() *ChainNodeSet {
	return &ChainNodeSet{Spec: ChainNodeSetSpec{
		Genesis: &GenesisConfig{ConfigMap: ptr.To("genesis")},
		App:     AppSpec{App: "chaind"},
		Nodes:   []NodeGroupSpec{},
	}}
}
