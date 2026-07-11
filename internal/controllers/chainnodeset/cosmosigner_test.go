package chainnodeset

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	k8sappsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/cosmosigner"
)

// TestUndeployCosmosignerClearsStatusInvariants verifies that once the signer StatefulSet and its
// PVCs are gone, teardown clears the recorded replica/digest invariants, so a later re-add (e.g. a
// sentry signer with a different replica count) is not rejected against stale state on the
// no-webhook path.
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

// TestUndeployCosmosignerKeepsStatusWhileTerminating verifies that while the signer StatefulSet is
// still present (teardown is asynchronous), the recorded invariants are preserved — clearing them
// early would let a remove-and-immediate-re-add bind the surviving PVCs and inherit stale raft
// membership.
func TestUndeployCosmosignerKeepsStatusWhileTerminating(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: "nodeset-uid"},
		Spec:       appsv1.ChainNodeSetSpec{},
		Status: appsv1.ChainNodeSetStatus{
			ChainID:                  "test-1",
			CosmosignerReplicas:      ptr.To(int32(3)),
			CosmosignerSigningDigest: "stale-digest",
		},
	}
	// A StatefulSet owned by the nodeSet with a finalizer: Undeploy issues a delete, but the fake
	// client retains it (deletionTimestamp set, object kept until finalizers clear), modelling the
	// window where teardown is still in flight.
	sts := &k8sappsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:       cosmosignerName(nodeSet),
			Namespace:  "default",
			Finalizers: []string{"cosmopilot.voluzi.com/test-hold"},
			Labels:     cosmosigner.InstanceLabels(cosmosignerName(nodeSet)),
		},
	}
	require.NoError(t, controllerutil.SetControllerReference(nodeSet, sts, testScheme(t)))

	r := newValidatorTestReconciler(t, nodeSet, sts)

	require.NoError(t, r.undeployCosmosigner(context.Background(), nodeSet))

	fresh := &appsv1.ChainNodeSet{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-nodeset"}, fresh))
	assert.Equal(t, ptr.To(int32(3)), fresh.Status.CosmosignerReplicas, "replica count must be preserved while the signer is still terminating")
	assert.Equal(t, "stale-digest", fresh.Status.CosmosignerSigningDigest, "signing digest must be preserved while the signer is still terminating")
}

// TestUndeployCosmosignerClearsStatusWithForeignSameNameSigner verifies that a same-name StatefulSet
// owned by ANOTHER CR does not permanently block clearing this nodeSet's recorded invariants:
// Undeploy skips the foreign resource, and IsTornDown treats it as unrelated, so the stale status is
// cleared and a later valid re-add is not rejected against it.
func TestUndeployCosmosignerClearsStatusWithForeignSameNameSigner(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: "nodeset-uid"},
		Spec:       appsv1.ChainNodeSetSpec{},
		Status: appsv1.ChainNodeSetStatus{
			ChainID:                  "test-1",
			CosmosignerReplicas:      ptr.To(int32(3)),
			CosmosignerSigningDigest: "stale-digest",
		},
	}
	// A same-name StatefulSet owned by a DIFFERENT ChainNodeSet (distinct UID).
	foreignOwner := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{Name: "other-nodeset", Namespace: "default", UID: "other-uid"}}
	sts := &k8sappsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cosmosignerName(nodeSet),
			Namespace: "default",
			Labels:    cosmosigner.InstanceLabels(cosmosignerName(nodeSet)),
		},
	}
	require.NoError(t, controllerutil.SetControllerReference(foreignOwner, sts, testScheme(t)))

	r := newValidatorTestReconciler(t, nodeSet, sts)

	require.NoError(t, r.undeployCosmosigner(context.Background(), nodeSet))

	fresh := &appsv1.ChainNodeSet{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-nodeset"}, fresh))
	assert.Nil(t, fresh.Status.CosmosignerReplicas, "a foreign same-name signer must not block clearing our status")
	assert.Empty(t, fresh.Status.CosmosignerSigningDigest, "a foreign same-name signer must not block clearing our status")

	// The foreign StatefulSet must be left untouched.
	remaining := &k8sappsv1.StatefulSet{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: cosmosignerName(nodeSet)}, remaining))
	assert.True(t, metav1.IsControlledBy(remaining, foreignOwner), "foreign signer must remain owned by the other CR")
}

// testScheme builds a scheme with the API + core + apps types for owner references in tests.
func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, k8sappsv1.AddToScheme(scheme))
	return scheme
}
