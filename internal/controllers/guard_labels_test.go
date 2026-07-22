package controllers

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestGuardInheritedLabels verifies genuine user labels are propagated to the guard while every
// cosmopilot-managed selector key is stripped (so a node/group-targeting selector cannot match the
// guard pods). The managed-key list here is maintained independently of GuardInheritedLabels, so if
// the function ever stops excluding one of these keys the test catches it.
func TestGuardInheritedLabels(t *testing.T) {
	managed := []string{
		LabelNodeID,
		LabelChainID,
		LabelValidator,
		LabelChainNode,
		LabelChainNodeSet,
		LabelChainNodeSetGroup,
		LabelChainNodeSetValidator,
		LabelGlobalIngress,
		LabelScope,
		LabelApp,
		LabelSeed,
		LabelPeer,
		LabelUpgrading,
		LabelCosmosignerTarget,
		LabelWorkerName,
	}

	in := map[string]string{
		"team":                      "payments", // user label -> kept
		"app.kubernetes.io/part-of": "chain",    // not managed -> kept
	}
	for _, k := range managed {
		in[k] = "managed"
	}

	out := GuardInheritedLabels(in)

	// Genuine user labels survive.
	assert.Equal(t, "payments", out["team"])
	assert.Equal(t, "chain", out["app.kubernetes.io/part-of"])
	// Every managed selector key is stripped.
	for _, k := range managed {
		assert.NotContains(t, out, k, "managed selector key %q must be stripped", k)
	}

	// Nil in -> empty out, never panics.
	assert.Empty(t, GuardInheritedLabels(nil))
}

// TestGuardInheritedLabelsStripsGuardDomain verifies labels under CosmoGuard's own domain (its
// guard-private selector labels and per-route selector labels) are never inherited — otherwise an
// inherited route.cosmoguard.voluzi.com/<route> label would make a global-route Service select
// unrelated group guards.
func TestGuardInheritedLabelsStripsGuardDomain(t *testing.T) {
	out := GuardInheritedLabels(map[string]string{
		"team":                                 "payments",
		"route.cosmoguard.voluzi.com/my-route": "true",
		"cosmoguard.voluzi.com/managed-by":     "cosmoguard",
		"cosmoguard.voluzi.com/instance":       "chain-fullnodes-cosmoguard",
		// An unrelated user label whose DNS prefix merely ends in the domain must be preserved.
		"acme-cosmoguard.voluzi.com/tier": "frontend",
	})

	assert.Equal(t, "payments", out["team"])
	assert.Equal(t, "frontend", out["acme-cosmoguard.voluzi.com/tier"], "only the owned prefixes are stripped")
	assert.NotContains(t, out, "route.cosmoguard.voluzi.com/my-route")
	assert.NotContains(t, out, "cosmoguard.voluzi.com/managed-by")
	assert.NotContains(t, out, "cosmoguard.voluzi.com/instance")
}
