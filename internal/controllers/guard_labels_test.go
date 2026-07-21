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
