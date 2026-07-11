package chainnodeset

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
)

// TestUndeployCosmosignerClearsStatusInvariants verifies that tearing down a signer clears the
// recorded replica/digest invariants, so a later re-add (e.g. a sentry signer with a different
// replica count) is not rejected against stale state on the no-webhook path.
func TestUndeployCosmosignerClearsStatusInvariants(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default"},
		Spec:       appsv1.ChainNodeSetSpec{},
		Status: appsv1.ChainNodeSetStatus{
			ChainID:                  "test-1",
			CosmosignerReplicas:      ptr.To(int32(3)),
			CosmosignerSigningDigest: "stale-digest",
		},
	}
	r := newValidatorTestReconciler(t, nodeSet)

	require.NoError(t, r.undeployCosmosigner(context.Background(), nodeSet))

	fresh := &appsv1.ChainNodeSet{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-nodeset"}, fresh))
	assert.Nil(t, fresh.Status.CosmosignerReplicas, "recorded replica count must be cleared on teardown")
	assert.Empty(t, fresh.Status.CosmosignerSigningDigest, "recorded signing digest must be cleared on teardown")
}
