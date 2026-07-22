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
	err := collide.validateServiceNameCollisions(nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "cs-foo-cg")

	// A non-colliding second group is fine.
	ok := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec:       ChainNodeSetSpec{Nodes: []NodeGroupSpec{guarded, {Name: "bar"}}},
	}
	require.NoError(t, ok.validateServiceNameCollisions(nil))

	// The same "foo-cg" group name is fine when "foo" is not guarded (no guard Service exists).
	unguarded := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec:       ChainNodeSetSpec{Nodes: []NodeGroupSpec{{Name: "foo"}, {Name: "foo-cg"}}},
	}
	require.NoError(t, unguarded.validateServiceNameCollisions(nil))
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
	err := ingColl.validateServiceNameCollisions(nil)
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
	err = gwColl.validateServiceNameCollisions(nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "global gateway route")

	// A non-colliding route is fine. The route name must differ from the guarded group's own name
	// ("global-rpc"), otherwise the route's backing Service "cs-global-rpc" would collide with the
	// group's main Service — a real collision the broadened check now also catches.
	ok := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec: ChainNodeSetSpec{
			Nodes:     []NodeGroupSpec{guarded},
			Ingresses: []GlobalIngressConfig{{Name: "p2p"}},
		},
	}
	require.NoError(t, ok.validateServiceNameCollisions(nil))
}

// Two node groups whose derived Service names shadow each other must be rejected even without any
// CosmoGuard: a group "foo" always creates an "<nodeSet>-foo-internal" Service, and a second group
// literally named "foo-internal" derives that same name as its main Service. Regression test for the
// -internal shadowing gap.
func TestValidateServiceNameCollisionsInternalShadowing(t *testing.T) {
	collide := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec:       ChainNodeSetSpec{Nodes: []NodeGroupSpec{{Name: "foo"}, {Name: "foo-internal"}}},
	}
	err := collide.validateServiceNameCollisions(nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "cs-foo-internal")

	// Distinct base names never shadow one another.
	ok := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec:       ChainNodeSetSpec{Nodes: []NodeGroupSpec{{Name: "foo"}, {Name: "bar"}}},
	}
	require.NoError(t, ok.validateServiceNameCollisions(nil))
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
		err := cs.validateGroupChildReservedNames(nil)
		require.Errorf(t, err, "group %q must be rejected", groupName)
		require.Contains(t, err.Error(), groupName)
	}

	// Ordinary group names are fine.
	ok := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec:       ChainNodeSetSpec{Nodes: []NodeGroupSpec{{Name: "fullnodes"}, {Name: "sentries"}}},
	}
	require.NoError(t, ok.validateGroupChildReservedNames(nil))
}

// A reserved-shaped group with zero instances materializes no child ChainNodes, so it must pass at
// create; scaling it up on a later update (0 -> >0) makes the child real and must then be rejected.
// A group that was already active in the old spec is grandfathered so unrelated updates to a
// predating ChainNodeSet are never blocked.
func TestValidateGroupChildReservedNamesUpdatePath(t *testing.T) {
	zero := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec:       ChainNodeSetSpec{Nodes: []NodeGroupSpec{{Name: "foo-cg", Instances: ptr.To(0)}}},
	}
	// Zero instances at create: no child yet, so it must not be rejected.
	require.NoError(t, zero.validateGroupChildReservedNames(nil))

	scaledUp := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec:       ChainNodeSetSpec{Nodes: []NodeGroupSpec{{Name: "foo-cg", Instances: ptr.To(1)}}},
	}
	// Scaling the same group up on update makes "cs-foo-cg-0" real: it must now be rejected.
	err := scaledUp.validateGroupChildReservedNames(zero)
	require.Error(t, err)
	require.Contains(t, err.Error(), "foo-cg")

	// A group already active (>0) in the old spec is grandfathered: an update leaving it active is fine.
	require.NoError(t, scaledUp.validateGroupChildReservedNames(scaledUp))
}

// A global ingress/gateway route always creates a "<nodeSet>-global-<route>-internal" Service. A
// route name can be short enough that the public backing Service "<nodeSet>-global-<route>" fits in
// 63 chars while the always-created -internal variant overflows. Regression test for the -internal
// route Service length gap.
func TestValidateGeneratedNameLengthsGlobalRouteInternal(t *testing.T) {
	// "cs-global-" (10) + 50 = 60 (public fits) but + "-internal" (9) = 69 (overflows).
	route50 := strings.Repeat("r", 50)

	ing := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec:       ChainNodeSetSpec{Ingresses: []GlobalIngressConfig{{Name: route50}}},
	}
	err := ing.validateGeneratedNameLengths()
	require.Error(t, err)
	require.Contains(t, err.Error(), "global ingress Service name")

	gw := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec:       ChainNodeSetSpec{GatewayRoutes: []GlobalGatewayConfig{{Name: route50}}},
	}
	err = gw.validateGeneratedNameLengths()
	require.Error(t, err)
	require.Contains(t, err.Error(), "global gateway Service name")
}

// Cosmoseed derives a "<nodeSet>-seed-headless" Service and per-instance
// "<nodeSet>-seed-<i>-internal" Services; both are 63-bound. They must be length-validated only when
// cosmoseed is enabled.
func TestValidateGeneratedNameLengthsCosmoseed(t *testing.T) {
	// name 50 + "-seed-headless" (14) = 64: the headless Service overflows.
	headless := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("a", 50)},
		Spec:       ChainNodeSetSpec{Cosmoseed: &CosmoseedConfig{Enabled: ptr.To(true)}},
	}
	err := headless.validateGeneratedNameLengths()
	require.Error(t, err)
	require.Contains(t, err.Error(), "cosmoseed headless Service name")

	// name 48: headless (62) fits, but the instance Service "<48>-seed-0-internal" (64) overflows.
	instance := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("a", 48)},
		Spec:       ChainNodeSetSpec{Cosmoseed: &CosmoseedConfig{Enabled: ptr.To(true), Instances: ptr.To(1)}},
	}
	err = instance.validateGeneratedNameLengths()
	require.Error(t, err)
	require.Contains(t, err.Error(), "cosmoseed instance Service name")

	// Disabled cosmoseed derives no seed Services, so an over-long name is not rejected on its account.
	disabled := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: strings.Repeat("a", 60)},
		Spec:       ChainNodeSetSpec{Cosmoseed: &CosmoseedConfig{Enabled: ptr.To(false)}},
	}
	require.NoError(t, disabled.validateGeneratedNameLengths())
}

// A guarded group derives, besides the client Service "<nodeSet>-<group>-cg", an upstream Service
// "<nodeSet>-<group>-cg-upstream" and a peer Service "<nodeSet>-<group>-cg-peer". A second group
// literally named "<group>-cg-upstream"/"<group>-cg-peer" derives the same Service name and must be
// rejected.
func TestValidateGroupGuardAuxiliaryNameCollisions(t *testing.T) {
	guarded := NodeGroupSpec{Name: "foo", Config: &Config{CosmoGuard: &CosmoGuardConfig{Enable: true}}}

	for _, collider := range []string{"foo-cg-upstream", "foo-cg-peer"} {
		cs := &ChainNodeSet{
			ObjectMeta: metav1.ObjectMeta{Name: "cs"},
			Spec:       ChainNodeSetSpec{Nodes: []NodeGroupSpec{guarded, {Name: collider}}},
		}
		err := cs.validateServiceNameCollisions(nil)
		require.Errorf(t, err, "group %q must collide with the guard auxiliary Service", collider)
		require.Contains(t, err.Error(), "cs-"+collider)
	}

	// Without CosmoGuard no auxiliary Services exist, so the same names are free.
	ok := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec:       ChainNodeSetSpec{Nodes: []NodeGroupSpec{{Name: "foo"}, {Name: "foo-cg-upstream"}, {Name: "foo-cg-peer"}}},
	}
	require.NoError(t, ok.validateServiceNameCollisions(nil))
}

// Cosmoseed derives a client Service "<nodeSet>-seed" and a headless Service
// "<nodeSet>-seed-headless"; a node group named "seed"/"seed-headless" derives the same name and must
// be rejected only when cosmoseed is enabled.
func TestValidateCosmoseedServiceNameCollisions(t *testing.T) {
	for _, collider := range []string{"seed", "seed-headless"} {
		cs := &ChainNodeSet{
			ObjectMeta: metav1.ObjectMeta{Name: "cs"},
			Spec: ChainNodeSetSpec{
				Cosmoseed: &CosmoseedConfig{Enabled: ptr.To(true)},
				Nodes:     []NodeGroupSpec{{Name: collider}},
			},
		}
		err := cs.validateServiceNameCollisions(nil)
		require.Errorf(t, err, "group %q must collide with a cosmoseed Service", collider)
		require.Contains(t, err.Error(), "cs-"+collider)
	}

	// Disabled cosmoseed derives no seed Services, so the same group names are free.
	ok := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec: ChainNodeSetSpec{
			Cosmoseed: &CosmoseedConfig{Enabled: ptr.To(false)},
			Nodes:     []NodeGroupSpec{{Name: "seed"}, {Name: "seed-headless"}},
		},
	}
	require.NoError(t, ok.validateServiceNameCollisions(nil))
}

// Cosmoseed also derives, per configured instance, an internal Service "<nodeSet>-seed-<i>-internal"
// (always) and an exposure Service "<nodeSet>-seed-<i>" (only when P2P expose is enabled). A node group
// named "seed-<i>" or "seed-<i>-internal" collides with the always-created instance internal Service
// under the same owner and must be rejected — but only up to the configured instance count.
func TestValidateCosmoseedInstanceServiceNameCollisions(t *testing.T) {
	// A group named "seed-0" collides via its own -internal Service with the always-created instance-0
	// internal Service; "seed-0-internal" collides at its base with the same Service. Neither needs
	// P2P expose to be enabled.
	for _, collider := range []string{"seed-0", "seed-0-internal"} {
		cs := &ChainNodeSet{
			ObjectMeta: metav1.ObjectMeta{Name: "cs"},
			Spec: ChainNodeSetSpec{
				Cosmoseed: &CosmoseedConfig{Enabled: ptr.To(true), Instances: ptr.To(1)},
				Nodes:     []NodeGroupSpec{{Name: collider}},
			},
		}
		err := cs.validateServiceNameCollisions(nil)
		require.Errorf(t, err, "group %q must collide with a cosmoseed instance Service", collider)
		require.Contains(t, err.Error(), "cs-seed-0-internal")
	}

	// The instance loop honours GetInstances(): "seed-1" is free with a single instance but collides
	// once a second instance is configured.
	single := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec: ChainNodeSetSpec{
			Cosmoseed: &CosmoseedConfig{Enabled: ptr.To(true), Instances: ptr.To(1)},
			Nodes:     []NodeGroupSpec{{Name: "seed-1"}},
		},
	}
	require.NoError(t, single.validateServiceNameCollisions(nil))

	two := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec: ChainNodeSetSpec{
			Cosmoseed: &CosmoseedConfig{Enabled: ptr.To(true), Instances: ptr.To(2)},
			Nodes:     []NodeGroupSpec{{Name: "seed-1"}},
		},
	}
	err := two.validateServiceNameCollisions(nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "cs-seed-1-internal")

	// With P2P expose enabled the per-instance exposure Service "<nodeSet>-seed-<i>" is also registered;
	// a non-colliding set still passes (exercises the expose registration path).
	exposed := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec: ChainNodeSetSpec{
			Cosmoseed: &CosmoseedConfig{Enabled: ptr.To(true), Instances: ptr.To(2), Expose: &ExposeConfig{P2P: ptr.To(true)}},
			Nodes:     []NodeGroupSpec{{Name: "fullnodes"}},
		},
	}
	require.NoError(t, exposed.validateServiceNameCollisions(nil))
}

// Each active instance of a group materializes a child ChainNode whose own main "<base>-<i>" and
// "-internal" Services share the name space. A sibling group shaped like an ordinal child ("foo-0"
// next to a scaled "foo") derives the same Service and must be rejected up front — at reconcile both
// Services share the ChainNodeSet owner, so the ownership guard cannot arbitrate.
func TestValidateChildInstanceServiceNameCollisions(t *testing.T) {
	// Group "foo" (1 instance) child Service "cs-foo-0" vs group "foo-0" main Service "cs-foo-0".
	collide := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec: ChainNodeSetSpec{Nodes: []NodeGroupSpec{
			{Name: "foo", Instances: ptr.To(1)},
			{Name: "foo-0", Instances: ptr.To(1)},
		}},
	}
	err := collide.validateServiceNameCollisions(nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "cs-foo-0")

	// The instance loop honours GetInstances(): "foo-1" is free with a single "foo" instance but
	// collides once "foo" is scaled to two.
	single := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec: ChainNodeSetSpec{Nodes: []NodeGroupSpec{
			{Name: "foo", Instances: ptr.To(1)},
			{Name: "foo-1", Instances: ptr.To(1)},
		}},
	}
	require.NoError(t, single.validateServiceNameCollisions(nil))

	two := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec: ChainNodeSetSpec{Nodes: []NodeGroupSpec{
			{Name: "foo", Instances: ptr.To(2)},
			{Name: "foo-1", Instances: ptr.To(1)},
		}},
	}
	err = two.validateServiceNameCollisions(nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "cs-foo-1")

	// A zero-instance group materializes no children, so its ordinal-shaped sibling is free.
	zero := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec: ChainNodeSetSpec{Nodes: []NodeGroupSpec{
			{Name: "foo", Instances: ptr.To(0)},
			{Name: "foo-0", Instances: ptr.To(1)},
		}},
	}
	require.NoError(t, zero.validateServiceNameCollisions(nil))
}

// A collision already present in the previous revision is grandfathered so an existing (already-broken)
// ChainNodeSet stays editable; only a collision newly introduced or newly activated on this revision is
// rejected. This validation was added after such objects could already exist, so update must not lock
// them out — the reconcilers' ownership guards remain the backstop for the grandfathered ones.
func TestValidateServiceNameCollisionsGrandfathersExisting(t *testing.T) {
	collided := func() *ChainNodeSet {
		return &ChainNodeSet{
			ObjectMeta: metav1.ObjectMeta{Name: "cs"},
			Spec: ChainNodeSetSpec{Nodes: []NodeGroupSpec{
				{Name: "foo", Instances: ptr.To(1)},
				{Name: "foo-0", Instances: ptr.To(1)},
			}},
		}
	}

	// Updating an object that already carried the "cs-foo-0" collision is allowed: it collided in old.
	require.NoError(t, collided().validateServiceNameCollisions(collided()))

	// A collision newly introduced on this revision (old had no "foo-0" sibling) is still rejected.
	oldClean := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec:       ChainNodeSetSpec{Nodes: []NodeGroupSpec{{Name: "foo", Instances: ptr.To(1)}}},
	}
	err := collided().validateServiceNameCollisions(oldClean)
	require.Error(t, err)
	require.Contains(t, err.Error(), "cs-foo-0")

	// A collision newly activated (old had the sibling but "foo" was scaled to zero, so no child existed)
	// is rejected once the group scales up and the child Service materializes.
	oldZero := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec: ChainNodeSetSpec{Nodes: []NodeGroupSpec{
			{Name: "foo", Instances: ptr.To(0)},
			{Name: "foo-0", Instances: ptr.To(1)},
		}},
	}
	err = collided().validateServiceNameCollisions(oldZero)
	require.Error(t, err)
	require.Contains(t, err.Error(), "cs-foo-0")
}

// Beyond its main "<child>" and "-internal" Services, an active child also derives a "-p2p" Service
// (P2P expose in Service mode) and its own "<child>-cg"/"<child>-cg-peer" guard Services (CosmoGuard +
// individual ingress/gateway routes). An ordinal-shaped sibling group ("foo-0-p2p", "foo-0-cg-peer")
// derives the same name under the same ChainNodeSet owner and must be rejected up front.
func TestValidateChildDerivedServiceNameCollisions(t *testing.T) {
	// P2P expose in Service mode: child "cs-foo-0" also owns Service "cs-foo-0-p2p".
	p2pCollide := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec: ChainNodeSetSpec{Nodes: []NodeGroupSpec{
			{Name: "foo", Instances: ptr.To(1), Expose: &ExposeConfig{P2P: ptr.To(true)}},
			{Name: "foo-0-p2p"},
		}},
	}
	err := p2pCollide.validateServiceNameCollisions(nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "cs-foo-0-p2p")

	// Without P2P expose no "-p2p" child Service exists, so the sibling name is free.
	p2pOff := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec: ChainNodeSetSpec{Nodes: []NodeGroupSpec{
			{Name: "foo", Instances: ptr.To(1)},
			{Name: "foo-0-p2p"},
		}},
	}
	require.NoError(t, p2pOff.validateServiceNameCollisions(nil))

	// CosmoGuard + individual ingress routes: the child runs its own guard, owning "cs-foo-0-cg" and
	// "cs-foo-0-cg-peer".
	guardChild := NodeGroupSpec{
		Name:                "foo",
		Instances:           ptr.To(1),
		Config:              &Config{CosmoGuard: &CosmoGuardConfig{Enable: true}},
		IndividualIngresses: &IngressConfig{},
	}
	guardPeerCollide := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec:       ChainNodeSetSpec{Nodes: []NodeGroupSpec{guardChild, {Name: "foo-0-cg-peer"}}},
	}
	err = guardPeerCollide.validateServiceNameCollisions(nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "cs-foo-0-cg-peer")

	guardCollide := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec:       ChainNodeSetSpec{Nodes: []NodeGroupSpec{guardChild, {Name: "foo-0-cg"}}},
	}
	err = guardCollide.validateServiceNameCollisions(nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "cs-foo-0-cg")

	// CosmoGuard without individual routes fronts the group with a single shared guard and gives the
	// children no per-node guard, so the "foo-0-cg-peer" sibling is free.
	guardNoIndividual := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec: ChainNodeSetSpec{Nodes: []NodeGroupSpec{
			{Name: "foo", Instances: ptr.To(1), Config: &Config{CosmoGuard: &CosmoGuardConfig{Enable: true}}},
			{Name: "foo-0-cg-peer"},
		}},
	}
	require.NoError(t, guardNoIndividual.validateServiceNameCollisions(nil))
}

// A CosmoGuard-guarded group with a dashboard Ingress owns Ingress "<nodeSet>-<group>-cg-dashboard".
// A global ingress route named like "<group>-cg-dashboard" renders the same Ingress name under the
// same ChainNodeSet owner, so validateIngressNameCollisions must reject it up front. gRPC route
// Ingresses ("-grpc") share the same name space.
func TestValidateIngressNameCollisions(t *testing.T) {
	guardedDashboardGroup := NodeGroupSpec{
		Name:      "global-rpc",
		Instances: ptr.To(1),
		Config: &Config{CosmoGuard: &CosmoGuardConfig{
			Enable:    true,
			Dashboard: &CosmoGuardDashboardConfig{Enable: true, Ingress: &CosmoGuardDashboardIngress{Host: "dash.example.com"}},
		}},
	}

	// Route "rpc-cg-dashboard" -> Ingress "cs-global-rpc-cg-dashboard", shadowing the group guard
	// dashboard Ingress.
	collide := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec: ChainNodeSetSpec{
			Nodes:     []NodeGroupSpec{guardedDashboardGroup},
			Ingresses: []GlobalIngressConfig{{Name: "rpc-cg-dashboard"}},
		},
	}
	err := collide.validateIngressNameCollisions(nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "cs-global-rpc-cg-dashboard")

	// A pre-existing collision is grandfathered so the ChainNodeSet stays editable.
	require.NoError(t, collide.validateIngressNameCollisions(collide))

	// A non-shadowing route name is fine.
	noCollide := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec: ChainNodeSetSpec{
			Nodes:     []NodeGroupSpec{guardedDashboardGroup},
			Ingresses: []GlobalIngressConfig{{Name: "api"}},
		},
	}
	require.NoError(t, noCollide.validateIngressNameCollisions(nil))

	// Without the dashboard Ingress the group owns no Ingress, so the same route name is free.
	dashboardOff := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec: ChainNodeSetSpec{
			Nodes:     []NodeGroupSpec{{Name: "global-rpc", Instances: ptr.To(1), Config: &Config{CosmoGuard: &CosmoGuardConfig{Enable: true}}}},
			Ingresses: []GlobalIngressConfig{{Name: "rpc-cg-dashboard"}},
		},
	}
	require.NoError(t, dashboardOff.validateIngressNameCollisions(nil))

	// A route's gRPC Ingress ("-grpc") collides with another route whose main Ingress renders the same
	// name.
	grpcCollide := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec: ChainNodeSetSpec{Ingresses: []GlobalIngressConfig{
			{Name: "rpc", EnableGRPC: true},
			{Name: "rpc-grpc"},
		}},
	}
	err = grpcCollide.validateIngressNameCollisions(nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "cs-global-rpc-grpc")
}

// The legacy singleton .spec.validator derives a Service "<nodeSet>-validator" and its -internal
// variant; a node group named "validator" derives the same name and must be rejected. When
// .spec.validator is unset no such Service exists.
func TestValidateLegacyValidatorServiceNameCollisions(t *testing.T) {
	collide := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec: ChainNodeSetSpec{
			Validator: &NodeSetValidatorConfig{},
			Nodes:     []NodeGroupSpec{{Name: "validator"}},
		},
	}
	err := collide.validateServiceNameCollisions(nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "cs-validator")

	// No legacy validator: the "validator" group name is free.
	ok := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec:       ChainNodeSetSpec{Nodes: []NodeGroupSpec{{Name: "validator"}}},
	}
	require.NoError(t, ok.validateServiceNameCollisions(nil))
}

// A global route always materializes the public "<nodeSet>-global-<route>" Service even when
// UseInternalServices flips its own backing Service to the -internal variant. A node group deriving
// that public name must still be rejected — the check must register GetName (public), not
// GetServiceName (which returns the internal name here).
func TestValidateRoutePublicServiceNameCollisionWithInternal(t *testing.T) {
	ing := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec: ChainNodeSetSpec{
			Ingresses: []GlobalIngressConfig{{Name: "rpc", UseInternalServices: ptr.To(true)}},
			Nodes:     []NodeGroupSpec{{Name: "global-rpc"}},
		},
	}
	err := ing.validateServiceNameCollisions(nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "cs-global-rpc")

	gw := &ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec: ChainNodeSetSpec{
			GatewayRoutes: []GlobalGatewayConfig{{Name: "rpc", UseInternalServices: ptr.To(true)}},
			Nodes:         []NodeGroupSpec{{Name: "global-rpc"}},
		},
	}
	err = gw.validateServiceNameCollisions(nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "cs-global-rpc")
}
