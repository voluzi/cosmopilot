package controllers

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestGuardInheritedLabels verifies genuine user labels are propagated to the guard while every
// cosmopilot-managed selector key is stripped (so a node/group-targeting selector cannot match the
// guard pods).
func TestGuardInheritedLabels(t *testing.T) {
	in := map[string]string{
		"team":                      "payments", // user label -> kept
		"app.kubernetes.io/part-of": "chain",    // not managed -> kept
		LabelChainNodeSet:           "myset",    // managed selector -> dropped
		LabelChainNodeSetGroup:      "fullnodes",
		LabelValidator:              "true",
		LabelApp:                    "mychain",
		LabelScope:                  "group",
		LabelCosmosignerTarget:      "signer",
	}

	out := GuardInheritedLabels(in)

	assert.Equal(t, "payments", out["team"])
	assert.Equal(t, "chain", out["app.kubernetes.io/part-of"])
	for _, managed := range []string{
		LabelChainNodeSet, LabelChainNodeSetGroup, LabelValidator, LabelApp, LabelScope, LabelCosmosignerTarget,
	} {
		assert.NotContains(t, out, managed, "managed selector key must be stripped")
	}

	// Nil in -> empty out, never panics.
	assert.Empty(t, GuardInheritedLabels(nil))
}
