package chainnode

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
)

// TestChainNodeSetTargetPodKeepsDiscoveryLabel is a regression test for the discovery-selector bug:
// WithChainNodeLabels strips the controller-managed cosmosigner-target label from inherited metadata,
// so a ChainNodeSet-managed target pod must have it re-added explicitly, otherwise the signer's
// discovery Service selects zero endpoints and can never dial its targets.
func TestChainNodeSetTargetPodKeepsDiscoveryLabel(t *testing.T) {
	const nodeSetName = "mychain"
	signerName := nodeSetName + "-signer"

	// A ChainNodeSet-managed target: RemoteSignerTarget with the nodeset-stamped metadata labels and
	// the controller owner reference every generated child carries, but no .spec.cosmosigner of its
	// own. The owner reference matters: WithChainNodeLabels strips the nodeset label from STANDALONE
	// nodes (where it can only be a user label spoofing a nodeset signer's discovery scope) and keeps
	// it on genuine children.
	isController := true
	child := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{
			Name: nodeSetName + "-fullnodes-0",
			Labels: map[string]string{
				controllers.LabelChainNodeSet:      nodeSetName,
				controllers.LabelCosmosignerTarget: signerName,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: appsv1.GroupVersion.String(),
				Kind:       "ChainNodeSet",
				Name:       nodeSetName,
				UID:        "nodeset-uid",
				Controller: &isController,
			}},
		},
		Spec: appsv1.ChainNodeSpec{RemoteSignerTarget: true},
	}

	// Reproduce getPodSpec's label computation.
	podLabels := map[string]string{controllers.LabelValidator: "false"}
	if v, ok := cosmosignerTargetLabelValue(child); ok {
		podLabels[controllers.LabelCosmosignerTarget] = v
	}
	final := WithChainNodeLabels(child, podLabels)

	if final[controllers.LabelCosmosignerTarget] != signerName {
		t.Fatalf("target pod missing discovery label: got %q, want %q", final[controllers.LabelCosmosignerTarget], signerName)
	}
	if final[controllers.LabelChainNodeSet] != nodeSetName {
		t.Fatalf("target pod missing nodeset label: got %q", final[controllers.LabelChainNodeSet])
	}

	// The discovery Service selects both labels (mirrors the ChainNodeSet controller's TargetSelector).
	selector := map[string]string{
		controllers.LabelChainNodeSet:      nodeSetName,
		controllers.LabelCosmosignerTarget: signerName,
	}
	for k, v := range selector {
		if final[k] != v {
			t.Fatalf("discovery selector %s=%s does not match target pod labels %+v", k, v, final)
		}
	}
}

// TestStandaloneTargetPodLabel verifies a standalone cosmosigner node still gets its own label.
func TestStandaloneTargetPodLabel(t *testing.T) {
	cn := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "solo"},
		Spec:       appsv1.ChainNodeSpec{Cosmosigner: &appsv1.Cosmosigner{}},
	}
	v, ok := cosmosignerTargetLabelValue(cn)
	if !ok || v != "solo-signer" {
		t.Fatalf("standalone target label = %q, %v; want solo-signer, true", v, ok)
	}
}

// TestNonTargetNodeHasNoDiscoveryLabel verifies a plain node never carries the label, even if a stray
// copy is present in its inherited metadata (which WithChainNodeLabels must strip).
func TestNonTargetNodeHasNoDiscoveryLabel(t *testing.T) {
	cn := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "plain",
			Labels: map[string]string{controllers.LabelCosmosignerTarget: "leaked-signer"},
		},
	}
	if _, ok := cosmosignerTargetLabelValue(cn); ok {
		t.Fatalf("non-target node must not be a signer target")
	}
	final := WithChainNodeLabels(cn, map[string]string{})
	if _, present := final[controllers.LabelCosmosignerTarget]; present {
		t.Fatalf("inherited cosmosigner-target label must be stripped from non-target pods: %+v", final)
	}
}

// TestStandaloneNodeSetLabelStripped verifies a STANDALONE node (no ChainNodeSet controller owner)
// never inherits the nodeset label onto its resources: that label is the discovery scope of every
// ChainNodeSet signer, so a user-set copy would let a same-named nodeset's signer select and dial
// this node's privval endpoint.
func TestStandaloneNodeSetLabelStripped(t *testing.T) {
	cn := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "solo",
			Labels: map[string]string{controllers.LabelChainNodeSet: "victim-nodeset"},
		},
		Spec: appsv1.ChainNodeSpec{Cosmosigner: &appsv1.Cosmosigner{}},
	}
	final := WithChainNodeLabels(cn, map[string]string{})
	if _, present := final[controllers.LabelChainNodeSet]; present {
		t.Fatalf("user-set nodeset label must be stripped from standalone node resources: %+v", final)
	}
}
