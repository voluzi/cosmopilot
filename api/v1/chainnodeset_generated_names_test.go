package v1

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	"github.com/stretchr/testify/require"
)

// A scaled-to-zero group has no child ChainNodes, so the highest-ordinal child check must be
// skipped: a group base whose own -internal Service fits must not be rejected for a nonexistent
// "<base>--1" child. Regression test for the "--1" false rejection.
func TestValidateGeneratedNameLengthsSkipsZeroInstanceChild(t *testing.T) {
	// base = 52 + "-g" = 54 chars: "<base>-internal" is exactly 63 (fits), but a child would push
	// "<base>-0-internal" to 65.
	base52 := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("a", 52)},
		Spec:       ChainNodeSetSpec{Nodes: []NodeGroupSpec{{Name: "g", Instances: ptr.To(0)}}},
	}
	require.NoError(t, base52.validateGeneratedNameLengths(), "zero-instance group must not be rejected for a nonexistent child")

	// Same base with one instance: the real child "<base>-0" overflows -internal and must be rejected.
	base52.Spec.Nodes[0].Instances = ptr.To(1)
	require.Error(t, base52.validateGeneratedNameLengths(), "a real highest-ordinal child that overflows must still be rejected")
}

// The legacy singleton .spec.validator is a ChainNode "<nodeSet>-validator" outside .spec.nodes; its
// derived -internal Service must be length-validated too.
func TestValidateGeneratedNameLengthsLegacyValidator(t *testing.T) {
	nodeSet := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("a", 50)},
	}
	// No validator, no nodes: nothing to reject.
	require.NoError(t, nodeSet.validateGeneratedNameLengths())

	// With the legacy validator, base "<50>-validator" (60) derives a 69-char -internal Service.
	nodeSet.Spec.Validator = &NodeSetValidatorConfig{}
	err := nodeSet.validateGeneratedNameLengths()
	require.Error(t, err)
	require.Contains(t, err.Error(), "internal Service")
}

// A long global ingress/gateway route name pushes its backing Service past 63 chars even when every
// node-group name fits; those route-derived Services must be length-validated.
func TestValidateGeneratedNameLengthsGlobalRoutes(t *testing.T) {
	longRoute := strings.Repeat("r", 60)

	ing := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec:       ChainNodeSetSpec{Ingresses: []GlobalIngressConfig{{Name: longRoute}}},
	}
	err := ing.validateGeneratedNameLengths()
	require.Error(t, err)
	require.Contains(t, err.Error(), "global ingress Service name")

	gw := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec:       ChainNodeSetSpec{GatewayRoutes: []GlobalGatewayConfig{{Name: longRoute}}},
	}
	err = gw.validateGeneratedNameLengths()
	require.Error(t, err)
	require.Contains(t, err.Error(), "global gateway Service name")

	// A short route name fits and must pass.
	ok := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec: ChainNodeSetSpec{
			Ingresses:     []GlobalIngressConfig{{Name: "rpc"}},
			GatewayRoutes: []GlobalGatewayConfig{{Name: "grpc"}},
		},
	}
	require.NoError(t, ok.validateGeneratedNameLengths())
}

// A guarded group "foo" derives guard Service "<nodeSet>-foo-cg"; a second group literally named
// "foo-cg" derives the same Service name. That collision must be rejected.
func TestValidateGroupGuardNameCollisions(t *testing.T) {
	guarded := NodeGroupSpec{Name: "foo", Config: &Config{CosmoGuard: &CosmoGuardConfig{Enable: true}}}

	collide := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec:       ChainNodeSetSpec{Nodes: []NodeGroupSpec{guarded, {Name: "foo-cg"}}},
	}
	err := collide.validateGroupGuardNameCollisions()
	require.Error(t, err)
	require.Contains(t, err.Error(), "cs-foo-cg")

	// A non-colliding second group is fine.
	ok := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec:       ChainNodeSetSpec{Nodes: []NodeGroupSpec{guarded, {Name: "bar"}}},
	}
	require.NoError(t, ok.validateGroupGuardNameCollisions())

	// The same "foo-cg" group name is fine when "foo" is not guarded (no guard Service exists).
	unguarded := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec:       ChainNodeSetSpec{Nodes: []NodeGroupSpec{{Name: "foo"}, {Name: "foo-cg"}}},
	}
	require.NoError(t, unguarded.validateGroupGuardNameCollisions())
}

// A guard Service name can also collide with a global ingress/gateway route's backing Service. A
// guarded group "global-rpc" derives guard Service "cs-global-rpc-cg"; a global route named "rpc-cg"
// derives that same Service name. Those collisions must be rejected too.
func TestValidateGroupGuardNameCollisionsWithRoutes(t *testing.T) {
	guarded := NodeGroupSpec{Name: "global-rpc", Config: &Config{CosmoGuard: &CosmoGuardConfig{Enable: true}}}

	ingColl := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec: ChainNodeSetSpec{
			Nodes:     []NodeGroupSpec{guarded},
			Ingresses: []GlobalIngressConfig{{Name: "rpc-cg"}},
		},
	}
	err := ingColl.validateGroupGuardNameCollisions()
	require.Error(t, err)
	require.Contains(t, err.Error(), "cs-global-rpc-cg")
	require.Contains(t, err.Error(), "global ingress route")

	gwColl := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec: ChainNodeSetSpec{
			Nodes:         []NodeGroupSpec{guarded},
			GatewayRoutes: []GlobalGatewayConfig{{Name: "rpc-cg"}},
		},
	}
	err = gwColl.validateGroupGuardNameCollisions()
	require.Error(t, err)
	require.Contains(t, err.Error(), "global gateway route")

	// A non-colliding route is fine.
	ok := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec: ChainNodeSetSpec{
			Nodes:     []NodeGroupSpec{guarded},
			Ingresses: []GlobalIngressConfig{{Name: "rpc"}},
		},
	}
	require.NoError(t, ok.validateGroupGuardNameCollisions())
}

// A group whose name ends in "-cg"/"-signer" makes the controller generate child ChainNodes like
// "<nodeSet>-foo-cg-0", which the reserved-StatefulSet-child check rejects at their own admission
// create — silently stranding the parent. Reject the group name up front at ChainNodeSet create.
func TestValidateGroupChildReservedNames(t *testing.T) {
	for _, groupName := range []string{"foo-cg", "foo-signer"} {
		cs := &ChainNodeSet{
			ObjectMeta: metav1.ObjectMeta{Name: "cs"},
			Spec:       ChainNodeSetSpec{Nodes: []NodeGroupSpec{{Name: groupName}}},
		}
		err := cs.validateGroupChildReservedNames()
		require.Errorf(t, err, "group %q must be rejected", groupName)
		require.Contains(t, err.Error(), groupName)
	}

	// Ordinary group names are fine.
	ok := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec:       ChainNodeSetSpec{Nodes: []NodeGroupSpec{{Name: "fullnodes"}, {Name: "sentries"}}},
	}
	require.NoError(t, ok.validateGroupChildReservedNames())
}
