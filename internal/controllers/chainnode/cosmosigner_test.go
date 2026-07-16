package chainnode

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
)

func TestBackfillCosmosignerLegacyStatusRecordsTargetKind(t *testing.T) {
	for _, tc := range []struct {
		name      string
		validator *appsv1.ValidatorConfig
		want      bool
	}{
		{name: "validator", validator: &appsv1.ValidatorConfig{}, want: true},
		{name: "sentry", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chainNode := &appsv1.ChainNode{
				ObjectMeta: metav1.ObjectMeta{Name: tc.name, Namespace: "default"},
				Spec: appsv1.ChainNodeSpec{
					Validator:   tc.validator,
					Cosmosigner: &appsv1.Cosmosigner{},
				},
				Status: appsv1.ChainNodeStatus{
					ChainID:                     "test-1",
					CosmosignerReplicas:         ptr.To(int32(1)),
					CosmosignerStateStorageSize: "1Gi",
				},
			}
			scheme := runtime.NewScheme()
			if err := appsv1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}
			cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&appsv1.ChainNode{}).WithObjects(chainNode).Build()
			r := &Reconciler{Client: cl}

			changed, err := r.backfillCosmosignerLegacyStatus(context.Background(), chainNode)
			if err != nil {
				t.Fatal(err)
			}
			if !changed {
				t.Fatal("legacy status backfill must record the signer target kind")
			}

			fresh := &appsv1.ChainNode{}
			if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: tc.name}, fresh); err != nil {
				t.Fatal(err)
			}
			if fresh.Status.CosmosignerValidatorTargeted == nil || *fresh.Status.CosmosignerValidatorTargeted != tc.want {
				t.Fatalf("CosmosignerValidatorTargeted = %v, want %v", fresh.Status.CosmosignerValidatorTargeted, tc.want)
			}
		})
	}
}

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
		ObjectMeta: metav1.ObjectMeta{
			Name:   "solo",
			Labels: map[string]string{controllers.LabelChainNodeSet: "victim-nodeset"},
		},
		Spec: appsv1.ChainNodeSpec{Cosmosigner: &appsv1.Cosmosigner{}},
	}
	v, ok := cosmosignerTargetLabelValue(cn)
	if !ok || v != "solo-signer" {
		t.Fatalf("standalone target label = %q, %v; want solo-signer, true", v, ok)
	}
	final := WithChainNodeLabels(cn, map[string]string{controllers.LabelCosmosignerTarget: v})
	if _, present := final[controllers.LabelChainNodeSet]; present {
		t.Fatalf("standalone signer target pod must not join a ChainNodeSet discovery scope: %+v", final)
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

// TestStandaloneNodeSetLabelPreservedOnOrdinaryResources verifies a standalone user label named
// "nodeset" is preserved on ordinary derived resources for backward compatibility.
func TestStandaloneNodeSetLabelPreservedOnOrdinaryResources(t *testing.T) {
	cn := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "solo",
			Labels: map[string]string{controllers.LabelChainNodeSet: "victim-nodeset"},
		},
	}
	final := WithChainNodeLabels(cn, map[string]string{})
	if final[controllers.LabelChainNodeSet] != "victim-nodeset" {
		t.Fatalf("user-set nodeset label must be preserved on ordinary resources: %+v", final)
	}
}
