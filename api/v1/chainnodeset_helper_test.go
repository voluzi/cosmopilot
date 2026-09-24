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

func TestGetLastUpgradeImageOnNodeSet(t *testing.T) {
	nodeSet := &ChainNodeSet{
		Spec: ChainNodeSetSpec{
			App: AppSpec{Image: "alloranetwork/allora-chain", Version: ptr.To("v0.8.2"), App: "allorad"},
		},
		Status: ChainNodeSetStatus{
			LatestHeight: 10618000,
			Upgrades: []Upgrade{
				{Height: 8824055, Image: "alloranetwork/allora-chain:v0.16.0", Status: UpgradeCompleted},
				{Height: 10511421, Image: "registry.ops.allora.run/bryn-test/allorad:986-test", Status: UpgradeCompleted},
			},
		},
	}

	assert.Equal(t, "registry.ops.allora.run/bryn-test/allorad:986-test", nodeSet.GetLastUpgradeImage())
	assert.Equal(t, "986-test", nodeSet.GetLastUpgradeVersion())
}

func TestGrpcServiceAnnotations(t *testing.T) {
	h2c := map[string]string{TraefikServersSchemeAnnotation: "h2c"}
	for class, want := range map[string]map[string]string{
		"traefik":          h2c,
		"traefik-internal": h2c,
		"nginx":            nil,
		"haproxy":          nil,
	} {
		gi := &GlobalIngressConfig{IngressClass: ptr.To(class)}
		assert.Equal(t, want, gi.GetGrpcServiceAnnotations(), class)
		cn := &ChainNode{Spec: ChainNodeSpec{Ingress: &IngressConfig{IngressClass: ptr.To(class)}}}
		assert.Equal(t, want, cn.GetGrpcServiceAnnotations(), class)
	}
	// The default class is nginx, which reads the protocol from the Ingress instead.
	assert.Nil(t, (&GlobalIngressConfig{}).GetGrpcServiceAnnotations())
	assert.Nil(t, (&ChainNode{Spec: ChainNodeSpec{Ingress: &IngressConfig{}}}).GetGrpcServiceAnnotations())
}
