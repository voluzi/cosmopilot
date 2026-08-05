package chainnode

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	k8sappsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
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
	"github.com/voluzi/cosmopilot/v2/internal/chainutils"
	"github.com/voluzi/cosmopilot/v2/internal/cometbft"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
	"github.com/voluzi/cosmopilot/v2/internal/cosmosigner"
	"github.com/voluzi/cosmopilot/v2/internal/resourcecleanup"
)

func TestFinalizeResourcesQuiescesLocalSignerAndAppliesDeletionPolicy(t *testing.T) {
	for _, policy := range []appsv1.DeletionPolicyType{appsv1.DeletionPolicyRetain, appsv1.DeletionPolicyDelete} {
		t.Run(string(policy), func(t *testing.T) {
			scheme := resourceCleanupScheme(t)
			node := &appsv1.ChainNode{
				ObjectMeta: metav1.ObjectMeta{
					Name: "validator", Namespace: "default", UID: "node-uid",
					Finalizers: []string{resourcecleanup.Finalizer, cosmosigner.OwnerFinalizer},
				},
				Spec: appsv1.ChainNodeSpec{DeletionPolicy: &appsv1.DeletionPolicy{
					DataVolumes:   ptr.To(policy),
					GeneratedKeys: ptr.To(policy),
				}},
			}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: node.Name, Namespace: node.Namespace, UID: "pod-uid"}}
			require.NoError(t, controllerutil.SetControllerReference(node, pod, scheme))
			initPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: node.Name + "-init-data", Namespace: node.Namespace, UID: "init-pod-uid"}}
			require.NoError(t, controllerutil.SetControllerReference(node, initPod, scheme))
			pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: node.Name, Namespace: node.Namespace, UID: "pvc-uid"}}
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: node.Name + "-priv-key", Namespace: node.Namespace, UID: "secret-uid"}}
			for _, object := range []client.Object{pvc, secret} {
				class := resourcecleanup.ClassDataVolumes
				if _, ok := object.(*corev1.Secret); ok {
					class = resourcecleanup.ClassGeneratedKeys
				}
				_, _, err := resourcecleanup.PrepareGeneratedResource(object, node, scheme, class, true)
				require.NoError(t, err)
			}
			ambiguous := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: node.Name + "-account", Namespace: node.Namespace, UID: "ambiguous-uid"}}
			reservation := &appsv1.ConsensusKeyReservation{
				ObjectMeta: metav1.ObjectMeta{Name: "reservation"},
				Spec:       appsv1.ConsensusKeyReservationSpec{OwnerUID: node.UID, OwnerKind: "ChainNode", Namespace: node.Namespace, OwnerName: node.Name},
			}
			base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node, pod, initPod, pvc, secret, ambiguous, reservation).Build()
			guarded := &quiesceBeforeDurableClient{Client: base, podKeys: []client.ObjectKey{
				client.ObjectKeyFromObject(pod), client.ObjectKeyFromObject(initPod),
			}}
			r := &Reconciler{Client: guarded, Scheme: scheme}

			current := &appsv1.ChainNode{}
			require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(node), current))
			done := false
			for attempts := 0; attempts < 6 && !done; attempts++ {
				var err error
				done, err = r.finalizeResources(context.Background(), current)
				require.NoError(t, err)
				if !done {
					require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(node), current))
				}
			}
			assert.True(t, done)
			assert.True(t, apierrors.IsNotFound(base.Get(context.Background(), client.ObjectKeyFromObject(pod), &corev1.Pod{})))
			assert.True(t, apierrors.IsNotFound(base.Get(context.Background(), client.ObjectKeyFromObject(initPod), &corev1.Pod{})))

			for _, object := range []client.Object{pvc, secret} {
				fresh := object.DeepCopyObject().(client.Object)
				err := base.Get(context.Background(), client.ObjectKeyFromObject(object), fresh)
				if policy == appsv1.DeletionPolicyDelete {
					assert.True(t, apierrors.IsNotFound(err), "%T should be deleted", object)
				} else {
					require.NoError(t, err)
					assert.Nil(t, metav1.GetControllerOf(fresh))
				}
			}
			require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(ambiguous), &corev1.Secret{}))
			require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(reservation), &appsv1.ConsensusKeyReservation{}), "issue #86 reservations must not be released by issue #84 cleanup")
		})
	}
}

func TestFinalizeResourcesBlocksOnOrphanedHelperPod(t *testing.T) {
	scheme := resourceCleanupScheme(t)
	node := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{
			Name: "validator", Namespace: "default", UID: "node-uid",
			Finalizers: []string{resourcecleanup.Finalizer},
		},
		Spec: appsv1.ChainNodeSpec{DeletionPolicy: &appsv1.DeletionPolicy{
			DataVolumes: ptr.To(appsv1.DeletionPolicyDelete),
		}},
	}
	helper := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: node.Name + "-write-file", Namespace: node.Namespace, UID: "helper-uid",
	}}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: node.Name, Namespace: node.Namespace, UID: "pvc-uid"}}
	_, _, err := resourcecleanup.PrepareGeneratedResource(pvc, node, scheme, resourcecleanup.ClassDataVolumes, true)
	require.NoError(t, err)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node, helper, pvc).Build()
	r := &Reconciler{Client: c, Scheme: scheme}

	done, err := r.finalizeResources(context.Background(), node)
	require.Error(t, err)
	assert.False(t, done)
	assert.Contains(t, err.Error(), helper.Name)
	assert.Contains(t, err.Error(), "not controlled by ChainNode UID")
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(helper), &corev1.Pod{}))
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(pvc), &corev1.PersistentVolumeClaim{}))
}

func TestFinalizeResourcesRetainsControlledLegacyGeneratedKeys(t *testing.T) {
	scheme := resourceCleanupScheme(t)
	node := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{
		Name: "validator", Namespace: "default", UID: "node-uid",
		Finalizers: []string{resourcecleanup.Finalizer},
	}}
	legacy := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "validator-priv-key", Namespace: node.Namespace, UID: "secret-uid"},
		Data:       map[string][]byte{PrivKeyFilename: []byte("legacy-consensus-key")},
	}
	require.NoError(t, controllerutil.SetControllerReference(node, legacy, scheme))
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node, legacy).Build()
	r := &Reconciler{Client: base, Scheme: scheme}

	done, err := r.finalizeResources(context.Background(), node)
	require.NoError(t, err)
	assert.True(t, done)
	retained := &corev1.Secret{}
	require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(legacy), retained))
	assert.Nil(t, metav1.GetControllerOf(retained))
	assert.True(t, resourcecleanup.IsAttributed(retained, resourcecleanup.RootOwnerFor(node), resourcecleanup.ClassGeneratedKeys))
}

func TestFinalizeResourcesRetainsControlledLegacyAccountAfterValidatorRemoval(t *testing.T) {
	scheme := resourceCleanupScheme(t)
	node := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{
		Name: "validator", Namespace: "default", UID: "node-uid",
		Finalizers: []string{resourcecleanup.Finalizer},
	}}
	legacy := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "validator-account", Namespace: node.Namespace, UID: "secret-uid"},
		Data:       map[string][]byte{MnemonicKey: []byte("legacy mnemonic")},
	}
	require.NoError(t, controllerutil.SetControllerReference(node, legacy, scheme))
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node, legacy).Build()
	r := &Reconciler{Client: base, Scheme: scheme}

	done, err := r.finalizeResources(context.Background(), node)
	require.NoError(t, err)
	assert.True(t, done)
	retained := &corev1.Secret{}
	require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(legacy), retained))
	assert.Nil(t, metav1.GetControllerOf(retained))
	assert.True(t, resourcecleanup.IsAttributed(retained, resourcecleanup.RootOwnerFor(node), resourcecleanup.ClassGeneratedKeys))
}

func TestMigrateExistingAccountSecretRunsAfterAccountStatusCompleted(t *testing.T) {
	scheme := resourceCleanupScheme(t)
	node := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "validator", Namespace: "default", UID: "node-uid"},
		Spec:       appsv1.ChainNodeSpec{Validator: &appsv1.ValidatorConfig{}},
		Status:     appsv1.ChainNodeStatus{AccountAddress: "cosmos1completed"},
	}
	legacy := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: node.Spec.Validator.GetAccountSecretName(node), Namespace: node.Namespace, UID: "secret-uid"},
		Data:       map[string][]byte{MnemonicKey: []byte("legacy mnemonic")},
	}
	require.NoError(t, controllerutil.SetControllerReference(node, legacy, scheme))
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node, legacy).Build()
	r := &Reconciler{Client: base, Scheme: scheme}

	require.False(t, node.RequiresAccount())
	require.NoError(t, r.migrateExistingAccountSecret(context.Background(), node))
	fresh := &corev1.Secret{}
	require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(legacy), fresh))
	assert.Nil(t, metav1.GetControllerOf(fresh))
	assert.True(t, resourcecleanup.IsAttributed(fresh, resourcecleanup.RootOwnerFor(node), resourcecleanup.ClassGeneratedKeys))
}

func TestMigrateExistingValidatorSecretsAttributesControlledPrivKeySecret(t *testing.T) {
	scheme := resourceCleanupScheme(t)
	node := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "validator", Namespace: "default", UID: "node-uid"},
		Spec:       appsv1.ChainNodeSpec{Validator: &appsv1.ValidatorConfig{}},
	}
	legacy := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: node.Spec.Validator.GetPrivKeySecretName(node), Namespace: node.Namespace, UID: "priv-key-uid"},
		Data:       map[string][]byte{PrivKeyFilename: []byte("legacy key")},
	}
	require.NoError(t, controllerutil.SetControllerReference(node, legacy, scheme))
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node, legacy).Build()
	r := &Reconciler{Client: base, Scheme: scheme}

	require.NoError(t, r.migrateExistingValidatorSecrets(context.Background(), node))
	fresh := &corev1.Secret{}
	require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(legacy), fresh))
	assert.Nil(t, metav1.GetControllerOf(fresh))
	assert.True(t, resourcecleanup.IsAttributed(fresh, resourcecleanup.RootOwnerFor(node), resourcecleanup.ClassGeneratedKeys))
}

// Valid key material at the generated default names is exactly what a user-imported key looks like,
// so it must never be adopted on shape alone: attribution would place it under generatedKeys: Delete.
func TestMigrateExistingValidatorSecretsPreservesUnownedDefaultNamedKeys(t *testing.T) {
	scheme := resourceCleanupScheme(t)
	node := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "validator", Namespace: "default", UID: "node-uid"},
		Spec:       appsv1.ChainNodeSpec{Validator: &appsv1.ValidatorConfig{}},
	}
	account, err := chainutils.CreateAccount("cosmos", "cosmosvaloper", node.Spec.Validator.GetAccountHDPath())
	require.NoError(t, err)
	privKey, err := cometbft.GeneratePrivKey()
	require.NoError(t, err)
	imported := []*corev1.Secret{
		{
			ObjectMeta: metav1.ObjectMeta{Name: node.Spec.Validator.GetAccountSecretName(node), Namespace: node.Namespace, UID: "account-uid"},
			Data:       map[string][]byte{MnemonicKey: []byte(account.Mnemonic)},
		},
		{
			ObjectMeta: metav1.ObjectMeta{Name: node.Spec.Validator.GetPrivKeySecretName(node), Namespace: node.Namespace, UID: "priv-key-uid"},
			Data:       map[string][]byte{PrivKeyFilename: privKey},
		},
	}
	objects := []client.Object{node}
	for _, secret := range imported {
		objects = append(objects, secret)
	}
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	r := &Reconciler{Client: base, Scheme: scheme}

	require.NoError(t, r.migrateExistingValidatorSecrets(context.Background(), node))
	for _, secret := range imported {
		fresh := &corev1.Secret{}
		require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(secret), fresh))
		assert.False(t, resourcecleanup.IsAttributed(fresh, resourcecleanup.RootOwnerFor(node), resourcecleanup.ClassGeneratedKeys))
		assert.Empty(t, fresh.Annotations[resourcecleanup.AnnotationResourceOwnerUID])
	}
}

func TestMigrateLegacyDurableResourcesAttributesAllVerifiedClasses(t *testing.T) {
	scheme := resourceCleanupScheme(t)
	node := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "validator", Namespace: "default", UID: "node-uid"},
		Spec:       appsv1.ChainNodeSpec{Validator: &appsv1.ValidatorConfig{}},
	}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: node.Name, Namespace: node.Namespace, UID: "data-uid"}}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: node.Spec.Validator.GetAccountSecretName(node), Namespace: node.Namespace, UID: "account-uid"},
		Data:       map[string][]byte{MnemonicKey: []byte("legacy mnemonic")},
	}
	for _, object := range []client.Object{pvc, secret} {
		require.NoError(t, controllerutil.SetControllerReference(node, object, scheme))
	}
	signerPVC := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: "data-validator-signer-0", Namespace: node.Namespace, UID: "signer-state-uid",
		Labels: map[string]string{
			"app.kubernetes.io/name":                  "cosmosigner",
			"app.kubernetes.io/instance":              "validator-signer",
			"cosmopilot.voluzi.com/cosmosigner-owner": string(node.UID),
		},
	}, Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "signer-volume"}, Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound}}
	require.NoError(t, controllerutil.SetControllerReference(node, signerPVC, scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node, pvc, secret, signerPVC).Build()
	r := &Reconciler{Client: c, Scheme: scheme}

	pending, err := r.MigrateLegacyDurableResources(context.Background(), node)
	require.NoError(t, err)
	assert.True(t, pending, "protecting the signer claim should request another startup pass")
	for object, class := range map[client.Object]resourcecleanup.ResourceClass{
		pvc: resourcecleanup.ClassDataVolumes, secret: resourcecleanup.ClassGeneratedKeys, signerPVC: resourcecleanup.ClassCosmosignerState,
	} {
		fresh := object.DeepCopyObject().(client.Object)
		require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(object), fresh))
		assert.Nil(t, metav1.GetControllerOf(fresh), "%s retained a cascading controller reference", object.GetName())
		assert.True(t, resourcecleanup.IsAttributed(fresh, resourcecleanup.RootOwnerFor(node), class), "%s was not attributed", object.GetName())
	}
	freshSigner := &corev1.PersistentVolumeClaim{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(signerPVC), freshSigner))
	assert.Contains(t, freshSigner.Finalizers, cosmosigner.RetainedStateFinalizer)
}

func TestMigrateLegacyDurableResourcesPreservesUnownedSameNamePVC(t *testing.T) {
	scheme := resourceCleanupScheme(t)
	node := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: "validator", Namespace: "default", UID: "node-uid"}}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: node.Name, Namespace: node.Namespace, UID: "data-uid",
		Labels: WithChainNodeLabels(node),
		Annotations: map[string]string{
			controllers.AnnotationDataInitialized: controllers.StringValueTrue,
			controllers.AnnotationDataHeight:      "123",
		},
	}}
	ambiguous := pvc.DeepCopy()
	ambiguous.Name = "user-data"
	ambiguous.UID = "ambiguous-uid"
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node, pvc, ambiguous).Build()
	r := &Reconciler{Client: c, Scheme: scheme}

	_, err := r.MigrateLegacyDurableResources(context.Background(), node)
	require.NoError(t, err)
	fresh := &corev1.PersistentVolumeClaim{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(pvc), fresh))
	assert.False(t, resourcecleanup.IsAttributed(fresh, resourcecleanup.RootOwnerFor(node), resourcecleanup.ClassDataVolumes),
		"normal existing-PVC labels and data annotations do not prove that Cosmopilot generated the claim")
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(ambiguous), fresh))
	assert.False(t, resourcecleanup.IsAttributed(fresh, resourcecleanup.RootOwnerFor(node), resourcecleanup.ClassDataVolumes))
}

func TestFinalizeResourcesRetainsControlledLegacyDataVolumesRemovedFromSpec(t *testing.T) {
	scheme := resourceCleanupScheme(t)
	node := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "validator", Namespace: "default", UID: "node-uid", Finalizers: []string{resourcecleanup.Finalizer}},
		Spec: appsv1.ChainNodeSpec{
			Persistence: &appsv1.Persistence{AdditionalVolumes: []appsv1.VolumeSpec{{Name: "wasm", Size: "1Gi", Path: "/wasm"}}},
			Validator:   &appsv1.ValidatorConfig{},
		},
	}
	mainPVC := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: node.Name, Namespace: node.Namespace, UID: "main-pvc"}}
	additionalPVC := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: node.Name + "-wasm", Namespace: node.Namespace, UID: "additional-pvc"}}
	removedAdditionalPVC := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: node.Name + "-archive", Namespace: node.Namespace, UID: "removed-additional-pvc"}}
	nodeKey := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: node.Name, Namespace: node.Namespace, UID: "node-key"}}
	accountKey := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: node.Name + "-account", Namespace: node.Namespace, UID: "account-key"}}
	consensusKey := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: node.Name + "-priv-key", Namespace: node.Namespace, UID: "consensus-key"}}
	cosmoguardKey := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: node.Name + "-cg-cluster", Namespace: node.Namespace, UID: "guard-key"}}
	for _, object := range []client.Object{mainPVC, additionalPVC, removedAdditionalPVC, nodeKey, accountKey, consensusKey, cosmoguardKey} {
		require.NoError(t, controllerutil.SetControllerReference(node, object, scheme))
	}
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node, mainPVC, additionalPVC, removedAdditionalPVC, nodeKey, accountKey, consensusKey, cosmoguardKey).Build()
	r := &Reconciler{Client: base, Scheme: scheme}

	done, err := r.finalizeResources(context.Background(), node)
	require.NoError(t, err)
	assert.True(t, done)
	for _, object := range []client.Object{mainPVC, additionalPVC, removedAdditionalPVC, nodeKey, accountKey, consensusKey} {
		fresh := object.DeepCopyObject().(client.Object)
		require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(object), fresh))
		assert.Nil(t, metav1.GetControllerOf(fresh))
	}
	freshGuard := &corev1.Secret{}
	require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(cosmoguardKey), freshGuard))
	assert.NotNil(t, metav1.GetControllerOf(freshGuard), "operational CosmoGuard credentials must remain garbage-collectable")
	assert.False(t, resourcecleanup.IsAttributed(freshGuard, resourcecleanup.RootOwnerFor(node), resourcecleanup.ClassGeneratedKeys))
}

func TestFinalizeResourcesUsesLiveChainNodeSetPolicy(t *testing.T) {
	scheme := resourceCleanupScheme(t)
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "set", Namespace: "default", UID: "set-uid",
			Finalizers: []string{resourcecleanup.Finalizer},
		},
		Spec: appsv1.ChainNodeSetSpec{DeletionPolicy: &appsv1.DeletionPolicy{
			DataVolumes:   ptr.To(appsv1.DeletionPolicyRetain),
			GeneratedKeys: ptr.To(appsv1.DeletionPolicyRetain),
		}},
	}
	node := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "set-fullnodes-0", Namespace: nodeSet.Namespace, UID: "node-uid", Finalizers: []string{resourcecleanup.Finalizer}},
		Spec: appsv1.ChainNodeSpec{DeletionPolicy: &appsv1.DeletionPolicy{
			DataVolumes:   ptr.To(appsv1.DeletionPolicyDelete),
			GeneratedKeys: ptr.To(appsv1.DeletionPolicyDelete),
		}},
	}
	require.NoError(t, controllerutil.SetControllerReference(nodeSet, node, scheme))
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: node.Name, Namespace: node.Namespace, UID: "pvc-uid"}}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: node.Name + "-account", Namespace: node.Namespace, UID: "secret-uid"},
		Data:       map[string][]byte{MnemonicKey: []byte("legacy mnemonic")},
	}
	for _, object := range []client.Object{pvc, secret} {
		require.NoError(t, controllerutil.SetControllerReference(node, object, scheme))
	}
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(nodeSet, node, pvc, secret).Build()
	r := &Reconciler{Client: base, Scheme: scheme}

	done, err := r.finalizeResources(context.Background(), node)
	require.NoError(t, err)
	assert.True(t, done)
	for _, object := range []client.Object{pvc, secret} {
		retained := object.DeepCopyObject().(client.Object)
		require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(object), retained))
		assert.Nil(t, metav1.GetControllerOf(retained))
	}
}

func TestReconcileNamespaceTerminationReleasesCleanupFinalizers(t *testing.T) {
	scheme := resourceCleanupScheme(t)
	now := metav1.NewTime(time.Now())
	node := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{
		Name: "validator", Namespace: "terminating", UID: "node-uid", DeletionTimestamp: &now,
		Finalizers: []string{resourcecleanup.Finalizer, cosmosigner.OwnerFinalizer},
	}}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "terminating", DeletionTimestamp: &now, Finalizers: []string{"kubernetes"},
	}}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: "data-validator-signer-0", Namespace: node.Namespace, UID: "pvc-uid",
		Labels: map[string]string{
			"app.kubernetes.io/instance":              "validator-signer",
			"cosmopilot.voluzi.com/cosmosigner-owner": string(node.UID),
		},
		Finalizers: []string{cosmosigner.RetainedStateFinalizer},
	}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: node.Name, Namespace: node.Namespace, UID: "pod-uid"}}
	require.NoError(t, controllerutil.SetControllerReference(node, pod, scheme))
	zero := int32(0)
	signer := &k8sappsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "validator-signer", Namespace: node.Namespace, UID: "signer-uid"}, Spec: k8sappsv1.StatefulSetSpec{
		Replicas: &zero,
		PersistentVolumeClaimRetentionPolicy: &k8sappsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
			WhenDeleted: k8sappsv1.RetainPersistentVolumeClaimRetentionPolicyType,
			WhenScaled:  k8sappsv1.RetainPersistentVolumeClaimRetentionPolicyType,
		},
		Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
			"app.kubernetes.io/name": "cosmosigner", "app.kubernetes.io/instance": "validator-signer",
		}}},
	}}
	require.NoError(t, controllerutil.SetControllerReference(node, signer, scheme))
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node, namespace, pvc, pod, signer).Build()
	r := &Reconciler{Client: base, Scheme: scheme}

	for attempts := 0; attempts < 5; attempts++ {
		_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(node)})
		require.NoError(t, err)
		if apierrors.IsNotFound(base.Get(context.Background(), client.ObjectKeyFromObject(node), &appsv1.ChainNode{})) {
			break
		}
	}

	currentNode := &appsv1.ChainNode{}
	if err := base.Get(context.Background(), client.ObjectKeyFromObject(node), currentNode); err == nil {
		assert.NotContains(t, currentNode.Finalizers, resourcecleanup.Finalizer)
		assert.NotContains(t, currentNode.Finalizers, cosmosigner.OwnerFinalizer)
	} else {
		assert.True(t, apierrors.IsNotFound(err))
	}
	currentPVC := &corev1.PersistentVolumeClaim{}
	require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(pvc), currentPVC))
	assert.NotContains(t, currentPVC.Finalizers, cosmosigner.RetainedStateFinalizer)
	assert.True(t, apierrors.IsNotFound(base.Get(context.Background(), client.ObjectKeyFromObject(pod), &corev1.Pod{})))
	assert.True(t, apierrors.IsNotFound(base.Get(context.Background(), client.ObjectKeyFromObject(signer), &k8sappsv1.StatefulSet{})))
}

func TestQuiesceNodePodWaitsForSameNameReplacement(t *testing.T) {
	scheme := resourceCleanupScheme(t)
	node := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: "validator", Namespace: "default", UID: "node-uid"}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: node.Name, Namespace: node.Namespace, UID: "pod-uid"}}
	require.NoError(t, controllerutil.SetControllerReference(node, pod, scheme))
	replacement := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: pod.Name, Namespace: pod.Namespace, UID: "replacement-uid"}}
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node, pod).Build()
	c := &replacePodAfterDeleteClient{Client: base, replacement: replacement}
	r := &Reconciler{Client: c, Scheme: scheme}

	done, err := r.quiesceNodePod(context.Background(), node)
	require.NoError(t, err)
	assert.False(t, done)
	assert.Equal(t, types.UID("pod-uid"), c.deleteUID)
}

type quiesceBeforeDurableClient struct {
	client.Client
	podKeys []client.ObjectKey
}

type replacePodAfterDeleteClient struct {
	client.Client
	replacement *corev1.Pod
	deleteUID   types.UID
}

func (c *replacePodAfterDeleteClient) Delete(ctx context.Context, object client.Object, opts ...client.DeleteOption) error {
	pod, ok := object.(*corev1.Pod)
	if !ok {
		return c.Client.Delete(ctx, object, opts...)
	}
	deleteOptions := (&client.DeleteOptions{}).ApplyOptions(opts)
	if deleteOptions.Preconditions != nil && deleteOptions.Preconditions.UID != nil {
		c.deleteUID = *deleteOptions.Preconditions.UID
	}
	if err := c.Client.Delete(ctx, pod); err != nil {
		return err
	}
	return c.Client.Create(ctx, c.replacement.DeepCopy())
}

func (c *quiesceBeforeDurableClient) Update(ctx context.Context, object client.Object, opts ...client.UpdateOption) error {
	if _, durable := object.(*corev1.PersistentVolumeClaim); durable {
		if err := c.requirePodGone(ctx); err != nil {
			return err
		}
	}
	if _, durable := object.(*corev1.Secret); durable {
		if err := c.requirePodGone(ctx); err != nil {
			return err
		}
	}
	return c.Client.Update(ctx, object, opts...)
}

func (c *quiesceBeforeDurableClient) Delete(ctx context.Context, object client.Object, opts ...client.DeleteOption) error {
	options := (&client.DeleteOptions{}).ApplyOptions(opts)
	if options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != object.GetUID() {
		return fmt.Errorf("cleanup deletion of %T requires an exact UID precondition", object)
	}
	switch object.(type) {
	case *corev1.PersistentVolumeClaim, *corev1.Secret:
		if err := c.requirePodGone(ctx); err != nil {
			return err
		}
	}
	return c.Client.Delete(ctx, object, opts...)
}

func (c *quiesceBeforeDurableClient) requirePodGone(ctx context.Context) error {
	for _, key := range c.podKeys {
		err := c.Client.Get(ctx, key, &corev1.Pod{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		return fmt.Errorf("durable cleanup started before signing or initialization pod was absent")
	}
	return nil
}

func resourceCleanupScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, k8sappsv1.AddToScheme(scheme))
	return scheme
}
