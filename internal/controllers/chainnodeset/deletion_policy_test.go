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
	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
	"github.com/voluzi/cosmopilot/v2/internal/resourcecleanup"
)

func TestGeneratedChainNodesCopyParentDeletionPolicy(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	r := &Reconciler{Scheme: scheme}
	policy := &appsv1.DeletionPolicy{
		DataVolumes:   ptr.To(appsv1.DeletionPolicyDelete),
		GeneratedKeys: ptr.To(appsv1.DeletionPolicyDelete),
	}
	nodeSet := &appsv1.ChainNodeSet{
		Spec: appsv1.ChainNodeSetSpec{
			Genesis:        &appsv1.GenesisConfig{ConfigMap: ptr.To("genesis")},
			DeletionPolicy: policy,
		},
	}

	regular, err := r.getNodeSpec(nodeSet, appsv1.NodeGroupSpec{Name: "fullnodes", Instances: ptr.To(1)}, 0)
	require.NoError(t, err)
	validator, err := r.getValidatorSpec(nodeSet, "validators", 0, &appsv1.NodeSetValidatorConfig{})
	require.NoError(t, err)

	for _, child := range []*appsv1.ChainNode{regular, validator} {
		require.NotNil(t, child.Spec.DeletionPolicy)
		assert.Equal(t, appsv1.DeletionPolicyDelete, child.Spec.DeletionPolicy.GetDataVolumes())
		assert.Equal(t, appsv1.DeletionPolicyDelete, child.Spec.DeletionPolicy.GetGeneratedKeys())
		assert.Equal(t, appsv1.DeletionPolicyRetain, child.Spec.DeletionPolicy.GetCosmosignerState())
		assert.NotSame(t, policy, child.Spec.DeletionPolicy)
	}
}

func TestGeneratedGenesisAndCosmoseedSecretsCarryStableAttribution(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: "default", UID: "set-uid"},
		Spec: appsv1.ChainNodeSetSpec{
			Cosmoseed: &appsv1.CosmoseedConfig{Enabled: ptr.To(true), Instances: ptr.To(1)},
		},
	}
	r := newValidatorTestReconciler(t, nodeSet)

	require.NoError(t, r.ensureSecret(context.Background(), nodeSet, "set-validators-1-priv-key", []string{"key"}, func() (map[string][]byte, error) {
		return map[string][]byte{"key": []byte("value")}, nil
	}))
	_, err := r.ensureCosmoseedNodeKeys(context.Background(), nodeSet)
	require.NoError(t, err)

	for _, name := range []string{"set-validators-1-priv-key", "set-cosmoseed"} {
		secret := &corev1.Secret{}
		require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: nodeSet.Namespace, Name: name}, secret))
		assert.True(t, resourcecleanup.IsAttributed(secret, resourcecleanup.RootOwnerFor(nodeSet), resourcecleanup.ClassGeneratedKeys))
		assert.Nil(t, metav1.GetControllerOf(secret))
	}
}

func TestExistingUnownedGeneratedKeyNameRemainsAmbiguous(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: "default", UID: "set-uid"}}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "set-validators-1-priv-key", Namespace: "default"},
		Data:       map[string][]byte{"key": []byte("provided")},
	}
	r := newValidatorTestReconciler(t, nodeSet, secret)

	require.NoError(t, r.ensureSecret(context.Background(), nodeSet, secret.Name, []string{"key"}, func() (map[string][]byte, error) {
		return map[string][]byte{"key": []byte("generated")}, nil
	}))
	current := &corev1.Secret{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(secret), current))
	assert.False(t, resourcecleanup.IsAttributed(current, resourcecleanup.RootOwnerFor(nodeSet), resourcecleanup.ClassGeneratedKeys))
	assert.Empty(t, current.OwnerReferences)
}

func TestCosmoseedStatefulSetUpdatePreservesExistingClaimTemplate(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: "default", UID: "set-uid"},
		Spec: appsv1.ChainNodeSetSpec{
			Cosmoseed: &appsv1.CosmoseedConfig{Enabled: ptr.To(true), Instances: ptr.To(1)},
		},
	}
	r := newValidatorTestReconciler(t, nodeSet)
	existing, err := r.getStatefulSet(nodeSet, "old-config", nil)
	require.NoError(t, err)
	require.Len(t, existing.Spec.VolumeClaimTemplates, 1)
	existing.Spec.VolumeClaimTemplates[0].Annotations = nil
	require.NoError(t, r.Create(context.Background(), existing))

	desired, err := r.getStatefulSet(nodeSet, "new-config", nil)
	require.NoError(t, err)
	assert.True(t, resourcecleanup.IsAttributed(
		&desired.Spec.VolumeClaimTemplates[0],
		resourcecleanup.RootOwnerFor(nodeSet),
		resourcecleanup.ClassDataVolumes,
	))
	require.NoError(t, r.ensureStatefulSet(context.Background(), desired))

	fresh := &k8sappsv1.StatefulSet{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(existing), fresh))
	require.Len(t, fresh.Spec.VolumeClaimTemplates, 1)
	assert.Empty(t, fresh.Spec.VolumeClaimTemplates[0].Annotations)
	assert.Equal(t, "new-config", fresh.Spec.Template.Annotations[controllers.AnnotationConfigHash])
}

func TestCosmoseedScaleUpRetiresLegacyClaimTemplate(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: "default", UID: "set-uid"},
		Spec: appsv1.ChainNodeSetSpec{
			Cosmoseed: &appsv1.CosmoseedConfig{Enabled: ptr.To(true), Instances: ptr.To(2)},
		},
	}
	r := newValidatorTestReconciler(t, nodeSet)
	existing, err := r.getStatefulSet(nodeSet, "old-config", nil)
	require.NoError(t, err)
	existing.Spec.Replicas = ptr.To(int32(1))
	existing.Spec.VolumeClaimTemplates[0].Annotations = nil
	require.NoError(t, r.Create(context.Background(), existing))
	desired, err := r.getStatefulSet(nodeSet, "new-config", nil)
	require.NoError(t, err)

	pending, err := r.retireLegacyCosmoseedStatefulSetBeforeScaleUp(context.Background(), nodeSet, desired)
	require.NoError(t, err)
	assert.True(t, pending)
	assert.Error(t, r.Get(context.Background(), client.ObjectKeyFromObject(existing), &k8sappsv1.StatefulSet{}))
}

func TestCosmoseedScaleUpKeepsAttributedClaimTemplateWithAPIDefaults(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: "default", UID: "set-uid"},
		Spec: appsv1.ChainNodeSetSpec{
			Cosmoseed: &appsv1.CosmoseedConfig{Enabled: ptr.To(true), Instances: ptr.To(2)},
		},
	}
	r := newValidatorTestReconciler(t, nodeSet)
	existing, err := r.getStatefulSet(nodeSet, "old-config", nil)
	require.NoError(t, err)
	existing.Spec.Replicas = ptr.To(int32(1))
	existing.Spec.VolumeClaimTemplates[0].Spec.VolumeMode = ptr.To(corev1.PersistentVolumeFilesystem)
	require.NoError(t, r.Create(context.Background(), existing))
	desired, err := r.getStatefulSet(nodeSet, "new-config", nil)
	require.NoError(t, err)

	pending, err := r.retireLegacyCosmoseedStatefulSetBeforeScaleUp(context.Background(), nodeSet, desired)
	require.NoError(t, err)
	assert.False(t, pending)
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(existing), &k8sappsv1.StatefulSet{}))
}

func TestCosmoseedMutableUpdateKeepsLegacyClaimTemplateWithoutScaleUp(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: "default", UID: "set-uid"},
		Spec: appsv1.ChainNodeSetSpec{
			Cosmoseed: &appsv1.CosmoseedConfig{Enabled: ptr.To(true), Instances: ptr.To(1)},
		},
	}
	r := newValidatorTestReconciler(t, nodeSet)
	existing, err := r.getStatefulSet(nodeSet, "old-config", nil)
	require.NoError(t, err)
	existing.Spec.VolumeClaimTemplates[0].Annotations = nil
	require.NoError(t, r.Create(context.Background(), existing))
	desired, err := r.getStatefulSet(nodeSet, "new-config", nil)
	require.NoError(t, err)

	pending, err := r.retireLegacyCosmoseedStatefulSetBeforeScaleUp(context.Background(), nodeSet, desired)
	require.NoError(t, err)
	assert.False(t, pending)
}
