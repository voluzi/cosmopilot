package chainnodeset

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	k8sappsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
	"github.com/voluzi/cosmopilot/v2/internal/cosmosigner"
	"github.com/voluzi/cosmopilot/v2/internal/resourcecleanup"
)

func TestFinalizeResourcesCoversRegularMultiValidatorAndCosmosignerState(t *testing.T) {
	for _, policy := range []appsv1.DeletionPolicyType{appsv1.DeletionPolicyRetain, appsv1.DeletionPolicyDelete} {
		t.Run(string(policy), func(t *testing.T) {
			scheme := nodeSetCleanupScheme(t)
			nodeSet := &appsv1.ChainNodeSet{
				ObjectMeta: metav1.ObjectMeta{
					Name: "set", Namespace: "default", UID: "set-uid",
					Finalizers: []string{resourcecleanup.Finalizer, cosmosigner.OwnerFinalizer},
				},
				Spec: appsv1.ChainNodeSetSpec{DeletionPolicy: &appsv1.DeletionPolicy{
					DataVolumes:      ptr.To(policy),
					GeneratedKeys:    ptr.To(policy),
					CosmosignerState: ptr.To(policy),
				}},
			}
			regular := ownedChild(t, scheme, nodeSet, "set-fullnodes-0")
			validator0 := ownedChild(t, scheme, nodeSet, "set-validators-0")
			validator1 := ownedChild(t, scheme, nodeSet, "set-validators-1")
			regularPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: regular.Name, Namespace: nodeSet.Namespace, UID: "regular-pod-uid"}}
			require.NoError(t, controllerutil.SetControllerReference(regular, regularPod, scheme))
			data := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: regular.Name, Namespace: nodeSet.Namespace, UID: "data-uid"}}
			key := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: validator0.Name + "-priv-key", Namespace: nodeSet.Namespace, UID: "key-uid"}}
			_, _, err := resourcecleanup.PrepareGeneratedResource(data, regular, scheme, resourcecleanup.ClassDataVolumes, true)
			require.NoError(t, err)
			_, _, err = resourcecleanup.PrepareGeneratedResource(key, validator0, scheme, resourcecleanup.ClassGeneratedKeys, true)
			require.NoError(t, err)

			zero := int32(0)
			signer := &k8sappsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "set-signer", Namespace: nodeSet.Namespace, UID: "signer-uid"}, Spec: k8sappsv1.StatefulSetSpec{
				Replicas: &zero,
				PersistentVolumeClaimRetentionPolicy: &k8sappsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
					WhenDeleted: k8sappsv1.RetainPersistentVolumeClaimRetentionPolicyType,
					WhenScaled:  k8sappsv1.RetainPersistentVolumeClaimRetentionPolicyType,
				},
				Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
					"app.kubernetes.io/name": "cosmosigner", "app.kubernetes.io/instance": "set-signer",
				}}},
			}}
			require.NoError(t, controllerutil.SetControllerReference(nodeSet, signer, scheme))
			seed := &k8sappsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
				Name: "set-seed", Namespace: nodeSet.Namespace, UID: "seed-uid",
				Labels: map[string]string{controllers.LabelApp: controllers.CosmoseedName, controllers.LabelChainNodeSet: nodeSet.Name},
			}, Spec: k8sappsv1.StatefulSetSpec{Replicas: &zero}}
			require.NoError(t, controllerutil.SetControllerReference(nodeSet, seed, scheme))
			seedPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Name: "set-seed-0", Namespace: nodeSet.Namespace, UID: "seed-pod-uid",
				Labels: map[string]string{controllers.LabelApp: controllers.CosmoseedName, controllers.LabelChainNodeSet: nodeSet.Name},
			}}
			require.NoError(t, controllerutil.SetControllerReference(seed, seedPod, scheme))
			signerState := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
				Name: "data-set-signer-0", Namespace: nodeSet.Namespace, UID: "state-uid",
				Labels: map[string]string{
					"app.kubernetes.io/instance":              "set-signer",
					"cosmopilot.voluzi.com/cosmosigner-owner": string(nodeSet.UID),
				},
				Finalizers: []string{cosmosigner.RetainedStateFinalizer},
			}}
			resourcecleanup.Stamp(signerState, resourcecleanup.RootOwnerFor(nodeSet), resourcecleanup.ClassCosmosignerState)
			reservation := &appsv1.ConsensusKeyReservation{ObjectMeta: metav1.ObjectMeta{Name: "reservation"}, Spec: appsv1.ConsensusKeyReservationSpec{OwnerUID: nodeSet.UID}}

			objects := []client.Object{nodeSet, regular, validator0, validator1, regularPod, data, key, signer, seed, seedPod, signerState, reservation}
			base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
			guarded := &nodeSetQuiesceGuardClient{Client: base, nodeSet: nodeSet}
			r := &Reconciler{Client: guarded, Scheme: scheme}
			current := &appsv1.ChainNodeSet{}
			require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(nodeSet), current))

			done := false
			for attempts := 0; attempts < 10 && !done; attempts++ {
				done, err = r.finalizeResources(context.Background(), current)
				require.NoError(t, err)
				if !done {
					require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(nodeSet), current))
				}
			}
			assert.True(t, done)
			for _, child := range []*appsv1.ChainNode{regular, validator0, validator1} {
				assert.True(t, apierrors.IsNotFound(base.Get(context.Background(), client.ObjectKeyFromObject(child), &appsv1.ChainNode{})))
			}
			for _, sts := range []*k8sappsv1.StatefulSet{signer, seed} {
				assert.True(t, apierrors.IsNotFound(base.Get(context.Background(), client.ObjectKeyFromObject(sts), &k8sappsv1.StatefulSet{})))
			}
			for _, pod := range []*corev1.Pod{regularPod, seedPod} {
				assert.True(t, apierrors.IsNotFound(base.Get(context.Background(), client.ObjectKeyFromObject(pod), &corev1.Pod{})))
			}
			for _, object := range []client.Object{data, key, signerState} {
				fresh := object.DeepCopyObject().(client.Object)
				getErr := base.Get(context.Background(), client.ObjectKeyFromObject(object), fresh)
				if policy == appsv1.DeletionPolicyDelete {
					assert.True(t, apierrors.IsNotFound(getErr), "%T should be deleted", object)
				} else {
					require.NoError(t, getErr)
					assert.NotContains(t, fresh.GetFinalizers(), cosmosigner.RetainedStateFinalizer)
				}
			}
			require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(reservation), &appsv1.ConsensusKeyReservation{}))
		})
	}
}

func TestReconcileNamespaceTerminationReleasesCleanupFinalizers(t *testing.T) {
	scheme := nodeSetCleanupScheme(t)
	now := metav1.NewTime(time.Now())
	nodeSet := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{
		Name: "set", Namespace: "terminating", UID: "set-uid", DeletionTimestamp: &now,
		Finalizers: []string{resourcecleanup.Finalizer, cosmosigner.OwnerFinalizer, podDisruptionBudgetFinalizer},
	}}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "terminating", DeletionTimestamp: &now, Finalizers: []string{"kubernetes"},
	}}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: "data-set-signer-0", Namespace: nodeSet.Namespace, UID: "state-uid",
		Labels: map[string]string{
			"app.kubernetes.io/instance":              "set-signer",
			"cosmopilot.voluzi.com/cosmosigner-owner": string(nodeSet.UID),
		},
		Finalizers: []string{cosmosigner.RetainedStateFinalizer},
	}}
	regular := ownedChild(t, scheme, nodeSet, "set-fullnodes-0")
	validator := ownedChild(t, scheme, nodeSet, "set-validators-0")
	regularPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: regular.Name, Namespace: nodeSet.Namespace, UID: "regular-pod-uid"}}
	validatorPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: validator.Name, Namespace: nodeSet.Namespace, UID: "validator-pod-uid"}}
	require.NoError(t, controllerutil.SetControllerReference(regular, regularPod, scheme))
	require.NoError(t, controllerutil.SetControllerReference(validator, validatorPod, scheme))
	zero := int32(0)
	signer := &k8sappsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "set-signer", Namespace: nodeSet.Namespace, UID: "signer-uid"}, Spec: k8sappsv1.StatefulSetSpec{
		Replicas: &zero,
		PersistentVolumeClaimRetentionPolicy: &k8sappsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
			WhenDeleted: k8sappsv1.RetainPersistentVolumeClaimRetentionPolicyType,
			WhenScaled:  k8sappsv1.RetainPersistentVolumeClaimRetentionPolicyType,
		},
		Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
			"app.kubernetes.io/name": "cosmosigner", "app.kubernetes.io/instance": "set-signer",
		}}},
	}}
	require.NoError(t, controllerutil.SetControllerReference(nodeSet, signer, scheme))
	seed := &k8sappsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
		Name: "set-seed", Namespace: nodeSet.Namespace, UID: "seed-uid",
		Labels: map[string]string{controllers.LabelApp: controllers.CosmoseedName, controllers.LabelChainNodeSet: nodeSet.Name},
	}, Spec: k8sappsv1.StatefulSetSpec{Replicas: &zero}}
	require.NoError(t, controllerutil.SetControllerReference(nodeSet, seed, scheme))
	seedPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "set-seed-0", Namespace: nodeSet.Namespace, UID: "seed-pod-uid"}}
	require.NoError(t, controllerutil.SetControllerReference(seed, seedPod, scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		nodeSet, namespace, pvc, regular, validator, regularPod, validatorPod, signer, seed, seedPod,
	).Build()
	r := &Reconciler{Client: c, Scheme: scheme}

	for attempts := 0; attempts < 8; attempts++ {
		_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(nodeSet)})
		require.NoError(t, err)
		if apierrors.IsNotFound(c.Get(context.Background(), client.ObjectKeyFromObject(nodeSet), &appsv1.ChainNodeSet{})) {
			break
		}
	}

	current := &appsv1.ChainNodeSet{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(nodeSet), current); err == nil {
		assert.NotContains(t, current.Finalizers, resourcecleanup.Finalizer)
		assert.NotContains(t, current.Finalizers, cosmosigner.OwnerFinalizer)
		assert.NotContains(t, current.Finalizers, podDisruptionBudgetFinalizer)
	} else {
		assert.True(t, apierrors.IsNotFound(err))
	}
	currentPVC := &corev1.PersistentVolumeClaim{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(pvc), currentPVC))
	assert.NotContains(t, currentPVC.Finalizers, cosmosigner.RetainedStateFinalizer)
	for _, object := range []client.Object{regular, validator, regularPod, validatorPod, signer, seed, seedPod} {
		fresh := object.DeepCopyObject().(client.Object)
		assert.True(t, apierrors.IsNotFound(c.Get(context.Background(), client.ObjectKeyFromObject(object), fresh)), "%T should be quiesced during namespace termination", object)
	}
}

func TestQuiesceAndDeleteChildrenMarksLegacyChildTerminatingBeforePodCleanup(t *testing.T) {
	scheme := nodeSetCleanupScheme(t)
	nodeSet := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: "default", UID: "set-uid"}}
	child := ownedChild(t, scheme, nodeSet, "set-fullnodes-0")
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: child.Name, Namespace: child.Namespace, UID: "pod-uid"}}
	require.NoError(t, controllerutil.SetControllerReference(child, pod, scheme))
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nodeSet, child, pod).Build()
	c := &recordCleanupDeleteOrderClient{Client: base}
	r := &Reconciler{Client: c, Scheme: scheme}

	done, err := r.quiesceAndDeleteChildren(context.Background(), nodeSet)
	require.NoError(t, err)
	assert.False(t, done)
	assert.Equal(t, []string{"ChainNode/set-fullnodes-0"}, c.deleted)
	require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(pod), &corev1.Pod{}))
}

func TestQuiesceAndDeleteChildrenLeavesInitializationPodToTerminatingChild(t *testing.T) {
	scheme := nodeSetCleanupScheme(t)
	nodeSet := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: "default", UID: "set-uid"}}
	child := ownedChild(t, scheme, nodeSet, "set-fullnodes-0")
	initPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: child.Name + "-init-data", Namespace: child.Namespace, UID: "init-pod-uid"}}
	require.NoError(t, controllerutil.SetControllerReference(child, initPod, scheme))
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nodeSet, child, initPod).Build()
	c := &recordCleanupDeleteOrderClient{Client: base}
	r := &Reconciler{Client: c, Scheme: scheme}

	done, err := r.quiesceAndDeleteChildren(context.Background(), nodeSet)
	require.NoError(t, err)
	assert.False(t, done)
	assert.Equal(t, []string{"ChainNode/set-fullnodes-0"}, c.deleted)
	require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(initPod), &corev1.Pod{}))
}

func TestQuiesceAndDeleteChildrenUsesRootRetentionPolicyBeforeRemovingFinalizer(t *testing.T) {
	scheme := nodeSetCleanupScheme(t)
	now := metav1.NewTime(time.Now())
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: "default", UID: "set-uid"},
		Spec: appsv1.ChainNodeSetSpec{DeletionPolicy: &appsv1.DeletionPolicy{
			DataVolumes:   ptr.To(appsv1.DeletionPolicyRetain),
			GeneratedKeys: ptr.To(appsv1.DeletionPolicyRetain),
		}},
	}
	child := ownedChild(t, scheme, nodeSet, "set-fullnodes-0")
	child.DeletionTimestamp = &now
	child.Finalizers = []string{resourcecleanup.Finalizer}
	child.Spec.DeletionPolicy = &appsv1.DeletionPolicy{
		DataVolumes:   ptr.To(appsv1.DeletionPolicyDelete),
		GeneratedKeys: ptr.To(appsv1.DeletionPolicyDelete),
	}
	child.Spec.Persistence = &appsv1.Persistence{AdditionalVolumes: []appsv1.VolumeSpec{{Name: "wasm", Size: "1Gi", Path: "/wasm"}}}
	child.Spec.Validator = &appsv1.ValidatorConfig{}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: child.Name + "-wasm", Namespace: child.Namespace, UID: "pvc-uid"}}
	key := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: child.Spec.Validator.GetPrivKeySecretName(child), Namespace: child.Namespace, UID: "key-uid"}}
	for _, object := range []client.Object{pvc, key} {
		require.NoError(t, controllerutil.SetControllerReference(child, object, scheme))
	}
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nodeSet, child, pvc, key).Build()
	r := &Reconciler{Client: base, Scheme: scheme}

	done, err := r.quiesceAndDeleteChildren(context.Background(), nodeSet)
	require.NoError(t, err)
	assert.True(t, done)
	for _, object := range []struct {
		resource client.Object
		class    resourcecleanup.ResourceClass
	}{
		{resource: pvc, class: resourcecleanup.ClassDataVolumes},
		{resource: key, class: resourcecleanup.ClassGeneratedKeys},
	} {
		retained := object.resource.DeepCopyObject().(client.Object)
		require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(object.resource), retained))
		assert.Nil(t, metav1.GetControllerOf(retained))
		assert.True(t, resourcecleanup.IsAttributed(retained, resourcecleanup.RootOwnerFor(child), object.class))
	}
}

func TestFinalizeResourcesRetainsControlledLegacyGenesisValidatorSecrets(t *testing.T) {
	scheme := nodeSetCleanupScheme(t)
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: "default", UID: "set-uid", Finalizers: []string{resourcecleanup.Finalizer}},
		Spec: appsv1.ChainNodeSetSpec{Nodes: []appsv1.NodeGroupSpec{{
			Name: "validators", Instances: ptr.To(2), Validator: &appsv1.NodeSetValidatorConfig{Init: &appsv1.GenesisInitConfig{}},
		}}},
	}
	account := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "set-validators-1-account", Namespace: nodeSet.Namespace, UID: "account-uid"}}
	key := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "set-validators-1-priv-key", Namespace: nodeSet.Namespace, UID: "key-uid"}}
	for _, secret := range []*corev1.Secret{account, key} {
		require.NoError(t, controllerutil.SetControllerReference(nodeSet, secret, scheme))
	}
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nodeSet, account, key).Build()
	r := &Reconciler{Client: base, Scheme: scheme}

	done, err := r.finalizeResources(context.Background(), nodeSet)
	require.NoError(t, err)
	assert.True(t, done)
	for _, secret := range []*corev1.Secret{account, key} {
		fresh := &corev1.Secret{}
		require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(secret), fresh))
		assert.Nil(t, metav1.GetControllerOf(fresh))
		assert.True(t, resourcecleanup.IsAttributed(fresh, resourcecleanup.RootOwnerFor(nodeSet), resourcecleanup.ClassGeneratedKeys))
	}
}

func TestQuiesceCosmoseedStopsPodsBeforeDeletingStatefulSet(t *testing.T) {
	scheme := nodeSetCleanupScheme(t)
	nodeSet := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: "default", UID: "set-uid"}}
	zero := int32(0)
	seed := &k8sappsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
		Name: "set-seed", Namespace: nodeSet.Namespace, UID: "seed-uid",
		Labels: map[string]string{controllers.LabelApp: controllers.CosmoseedName, controllers.LabelChainNodeSet: nodeSet.Name},
	}, Spec: k8sappsv1.StatefulSetSpec{Replicas: &zero}}
	require.NoError(t, controllerutil.SetControllerReference(nodeSet, seed, scheme))
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "set-seed-0", Namespace: nodeSet.Namespace, UID: "seed-pod-uid"}}
	require.NoError(t, controllerutil.SetControllerReference(seed, pod, scheme))
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nodeSet, seed, pod).Build()
	c := &recordCleanupDeleteOrderClient{Client: base}
	r := &Reconciler{Client: c, Scheme: scheme}

	done, err := r.quiesceCosmoseed(context.Background(), nodeSet)
	require.NoError(t, err)
	assert.True(t, done)
	assert.Equal(t, []string{"Pod/set-seed-0", "StatefulSet/set-seed"}, c.deleted)
}

func TestQuiesceCosmoseedUsesOwnedDeterministicNameWhenLabelsDrift(t *testing.T) {
	scheme := nodeSetCleanupScheme(t)
	nodeSet := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: "default", UID: "set-uid"}}
	zero := int32(0)
	seed := &k8sappsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "set-seed", Namespace: nodeSet.Namespace, UID: "seed-uid", Labels: map[string]string{"edited": "true"}}, Spec: k8sappsv1.StatefulSetSpec{Replicas: &zero}}
	require.NoError(t, controllerutil.SetControllerReference(nodeSet, seed, scheme))
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nodeSet, seed).Build()
	r := &Reconciler{Client: base, Scheme: scheme}

	done, err := r.quiesceCosmoseed(context.Background(), nodeSet)
	require.NoError(t, err)
	assert.True(t, done)
	assert.True(t, apierrors.IsNotFound(base.Get(context.Background(), client.ObjectKeyFromObject(seed), &k8sappsv1.StatefulSet{})))
}

func TestQuiesceCosmoseedScalesDownBeforeDeletingPods(t *testing.T) {
	scheme := nodeSetCleanupScheme(t)
	nodeSet := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: "default", UID: "set-uid"}}
	one := int32(1)
	seed := &k8sappsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
		Name: "set-seed", Namespace: nodeSet.Namespace, UID: "seed-uid",
		Labels: map[string]string{controllers.LabelApp: controllers.CosmoseedName, controllers.LabelChainNodeSet: nodeSet.Name},
	}, Spec: k8sappsv1.StatefulSetSpec{Replicas: &one}}
	require.NoError(t, controllerutil.SetControllerReference(nodeSet, seed, scheme))
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "set-seed-0", Namespace: nodeSet.Namespace, UID: "seed-pod-uid"}}
	require.NoError(t, controllerutil.SetControllerReference(seed, pod, scheme))
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nodeSet, seed, pod).Build()
	c := &recordCleanupDeleteOrderClient{Client: base}
	r := &Reconciler{Client: c, Scheme: scheme}

	done, err := r.quiesceCosmoseed(context.Background(), nodeSet)
	require.NoError(t, err)
	assert.False(t, done)
	assert.Empty(t, c.deleted)
	current := &k8sappsv1.StatefulSet{}
	require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(seed), current))
	require.NotNil(t, current.Spec.Replicas)
	assert.Zero(t, *current.Spec.Replicas)
}

func TestReconcileDeletionFinalizesPDBAndDurableResources(t *testing.T) {
	scheme := nodeSetCleanupScheme(t)
	now := metav1.NewTime(time.Now())
	nodeSet := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{
		Name: "set", Namespace: "default", UID: "set-uid", DeletionTimestamp: &now,
		Finalizers: []string{resourcecleanup.Finalizer, podDisruptionBudgetFinalizer},
	}}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
	pdb := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: "set-fullnodes", Namespace: "default", UID: "pdb-uid"}}
	require.NoError(t, controllerutil.SetControllerReference(nodeSet, pdb, scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nodeSet, namespace, pdb).Build()
	r := &Reconciler{Client: c, Scheme: scheme}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(nodeSet)})
	require.NoError(t, err)
	assert.True(t, apierrors.IsNotFound(c.Get(context.Background(), client.ObjectKeyFromObject(pdb), &policyv1.PodDisruptionBudget{})))
	current := &appsv1.ChainNodeSet{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(nodeSet), current); err == nil {
		assert.NotContains(t, current.Finalizers, podDisruptionBudgetFinalizer)
		assert.NotContains(t, current.Finalizers, resourcecleanup.Finalizer)
	} else {
		assert.True(t, apierrors.IsNotFound(err))
	}
}

func ownedChild(t *testing.T, scheme *runtime.Scheme, owner *appsv1.ChainNodeSet, name string) *appsv1.ChainNode {
	t.Helper()
	child := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: owner.Namespace, UID: typesUID(name + "-uid")}}
	require.NoError(t, controllerutil.SetControllerReference(owner, child, scheme))
	return child
}

type nodeSetQuiesceGuardClient struct {
	client.Client
	nodeSet *appsv1.ChainNodeSet
}

type recordCleanupDeleteOrderClient struct {
	client.Client
	deleted []string
}

func (c *recordCleanupDeleteOrderClient) Delete(ctx context.Context, object client.Object, opts ...client.DeleteOption) error {
	c.deleted = append(c.deleted, fmt.Sprintf("%T/%s", object, object.GetName()))
	switch object.(type) {
	case *corev1.Pod:
		c.deleted[len(c.deleted)-1] = "Pod/" + object.GetName()
	case *appsv1.ChainNode:
		c.deleted[len(c.deleted)-1] = "ChainNode/" + object.GetName()
	case *k8sappsv1.StatefulSet:
		c.deleted[len(c.deleted)-1] = "StatefulSet/" + object.GetName()
	}
	return c.Client.Delete(ctx, object, opts...)
}

func (c *nodeSetQuiesceGuardClient) Update(ctx context.Context, object client.Object, opts ...client.UpdateOption) error {
	if _, durable := object.(*corev1.PersistentVolumeClaim); durable {
		controller := metav1.GetControllerOf(object)
		childOwned := controller != nil && controller.Kind == "ChainNode"
		childAttributed := object.GetAnnotations()[resourcecleanup.AnnotationResourceOwnerUID] != ""
		if !childOwned && !childAttributed {
			if err := c.requireWorkloadsGone(ctx); err != nil {
				return err
			}
		}
	}
	if _, durable := object.(*corev1.Secret); durable {
		controller := metav1.GetControllerOf(object)
		childOwned := controller != nil && controller.Kind == "ChainNode"
		childAttributed := object.GetAnnotations()[resourcecleanup.AnnotationResourceOwnerUID] != ""
		if !childOwned && !childAttributed {
			if err := c.requireWorkloadsGone(ctx); err != nil {
				return err
			}
		}
	}
	return c.Client.Update(ctx, object, opts...)
}

func (c *nodeSetQuiesceGuardClient) Delete(ctx context.Context, object client.Object, opts ...client.DeleteOption) error {
	options := (&client.DeleteOptions{}).ApplyOptions(opts)
	if options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != object.GetUID() {
		return fmt.Errorf("cleanup deletion of %T requires an exact UID precondition", object)
	}
	switch object.(type) {
	case *corev1.PersistentVolumeClaim, *corev1.Secret:
		controller := metav1.GetControllerOf(object)
		childOwned := controller != nil && controller.Kind == "ChainNode"
		childAttributed := object.GetAnnotations()[resourcecleanup.AnnotationResourceOwnerUID] != ""
		if !childOwned && !childAttributed {
			if err := c.requireWorkloadsGone(ctx); err != nil {
				return err
			}
		}
	case *corev1.Pod:
		controller := metav1.GetControllerOf(object)
		if controller == nil {
			break
		}
		switch controller.Kind {
		case "ChainNode":
			child := &appsv1.ChainNode{}
			err := c.Client.Get(ctx, client.ObjectKey{Namespace: object.GetNamespace(), Name: controller.Name}, child)
			if apierrors.IsNotFound(err) {
				return fmt.Errorf("child pod deletion started after ChainNode disappeared")
			}
			if err != nil {
				return err
			}
		case "StatefulSet":
			sts := &k8sappsv1.StatefulSet{}
			err := c.Client.Get(ctx, client.ObjectKey{Namespace: object.GetNamespace(), Name: controller.Name}, sts)
			if apierrors.IsNotFound(err) {
				return fmt.Errorf("StatefulSet pod deletion started after its owner disappeared")
			}
			if err != nil {
				return err
			}
		}
	}
	return c.Client.Delete(ctx, object, opts...)
}

func (c *nodeSetQuiesceGuardClient) requireWorkloadsGone(ctx context.Context) error {
	children := &appsv1.ChainNodeList{}
	if err := c.Client.List(ctx, children, client.InNamespace(c.nodeSet.Namespace)); err != nil {
		return err
	}
	for i := range children.Items {
		if metav1.IsControlledBy(&children.Items[i], c.nodeSet) {
			return fmt.Errorf("durable cleanup started before child ChainNodes were absent")
		}
	}
	for _, name := range []string{"set-signer", "set-seed"} {
		if err := c.Client.Get(ctx, client.ObjectKey{Namespace: c.nodeSet.Namespace, Name: name}, &k8sappsv1.StatefulSet{}); err == nil {
			return fmt.Errorf("durable cleanup started before StatefulSet %s was absent", name)
		} else if !apierrors.IsNotFound(err) {
			return err
		}
	}
	pods := &corev1.PodList{}
	if err := c.Client.List(ctx, pods, client.InNamespace(c.nodeSet.Namespace)); err != nil {
		return err
	}
	if len(pods.Items) != 0 {
		return fmt.Errorf("durable cleanup started before workload pods were absent")
	}
	return nil
}

func nodeSetCleanupScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, k8sappsv1.AddToScheme(scheme))
	require.NoError(t, policyv1.AddToScheme(scheme))
	return scheme
}

func typesUID(value string) types.UID { return types.UID(value) }
