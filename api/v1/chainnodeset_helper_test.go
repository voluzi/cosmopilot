package v1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
)

// TestGetValidatorMinimumGasPricesSkipsZeroInstanceGroups verifies that a zero-instance validator
// group is skipped when selecting the validator to inherit the minimum gas price from, so a later
// group that actually runs validators provides the price instead of the inactive group.
func TestGetValidatorMinimumGasPricesSkipsZeroInstanceGroups(t *testing.T) {
	mkValidator := func(price string) *NodeSetValidatorConfig {
		return &NodeSetValidatorConfig{
			Config: &Config{Override: &map[string]runtime.RawExtension{
				"app.toml": {Raw: []byte(`{"minimum-gas-prices":"` + price + `"}`)},
			}},
		}
	}

	nodeSet := &ChainNodeSet{
		Spec: ChainNodeSetSpec{
			Nodes: []NodeGroupSpec{
				{Name: "inactive", Instances: ptr.To(0), Validator: mkValidator("0.1stake")},
				{Name: "active", Instances: ptr.To(1), Validator: mkValidator("0.25stake")},
			},
		},
	}

	assert.Equal(t, "0.25stake", nodeSet.GetValidatorMinimumGasPrices())
}
