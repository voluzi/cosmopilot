package chainnode

import (
	"context"
	"io"
	"net/http"
	"strings"
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
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/cometbft"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
	"github.com/voluzi/cosmopilot/v2/internal/cosmosigner"
)

func TestReconcileCosmosignerMigrationWaitsForTerminatingPod(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, k8sappsv1.AddToScheme(scheme))
	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "validator", Namespace: "default", UID: "validator-uid"},
		Spec: appsv1.ChainNodeSpec{Cosmosigner: &appsv1.Cosmosigner{
			Replicas: ptr.To(int32(1)),
			Backend: appsv1.CosmosignerBackend{Software: &appsv1.CosmosignerSoftwareBackend{
				PrivateKeySecret: ptr.To("validator-key"),
			}},
		}},
	}
	params := cosmosigner.Params{
		Name: cosmosignerName(chainNode), Namespace: chainNode.Namespace, ChainID: "test-1", Image: "image", Replicas: 1,
		ExpectedPublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", StateStorageSize: "1Gi",
		Backend: cosmosigner.Backend{Software: &cosmosigner.SoftwareBackend{SecretName: "validator-key"}},
	}
	desiredDigest, err := params.LifecycleDigest(chainNode.CosmosignerSigningDigest())
	require.NoError(t, err)
	chainNode.Status.CosmosignerAppliedDigest = "old-digest"
	chainNode.Status.CosmosignerPublicKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	chainNode.Status.CosmosignerMigration = &appsv1.CosmosignerMigrationStatus{
		DesiredDigest:    desiredDigest,
		DesiredPublicKey: chainNode.Status.CosmosignerPublicKey,
		Phase:            appsv1.CosmosignerMigrationQuiescing,
	}
	zero := int32(0)
	sts := &k8sappsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: cosmosignerName(chainNode), Namespace: chainNode.Namespace, Generation: 2, UID: "signer-uid"},
		Spec:       k8sappsv1.StatefulSetSpec{Replicas: &zero},
		Status:     k8sappsv1.StatefulSetStatus{ObservedGeneration: 2},
	}
	require.NoError(t, controllerutil.SetControllerReference(chainNode, sts, scheme))
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: sts.Name + "-0", Namespace: chainNode.Namespace,
		DeletionTimestamp: &metav1.Time{Time: time.Now()},
		Finalizers:        []string{"cosmopilot.voluzi.com/test-hold"},
	}}
	require.NoError(t, controllerutil.SetControllerReference(sts, pod, scheme))
	r := &Reconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(chainNode, sts, pod).Build(), Scheme: scheme}

	pending, err := r.reconcileCosmosignerMigration(context.Background(), chainNode, params)
	require.NoError(t, err)
	require.True(t, pending)
	require.Equal(t, appsv1.CosmosignerMigrationQuiescing, chainNode.Status.CosmosignerMigration.Phase)
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: chainNode.Namespace, Name: sts.Name}, &k8sappsv1.StatefulSet{}))
}

func TestReconcileCosmosignerMigrationComparesActualPublicKeys(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	oldKey, err := cometbft.GeneratePrivKey()
	require.NoError(t, err)
	differentKey, err := cometbft.GeneratePrivKey()
	require.NoError(t, err)
	oldParsed, err := cometbft.LoadPrivKey(oldKey)
	require.NoError(t, err)

	for _, tc := range []struct {
		name       string
		desiredKey []byte
		wantReset  bool
	}{
		{name: "same public key retains state", desiredKey: oldKey, wantReset: false},
		{name: "different public key resets state", desiredKey: differentKey, wantReset: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chainNode := &appsv1.ChainNode{
				ObjectMeta: metav1.ObjectMeta{Name: "sentry", Namespace: "default", UID: "sentry-uid"},
				Spec: appsv1.ChainNodeSpec{Cosmosigner: &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{
					Software: &appsv1.CosmosignerSoftwareBackend{PrivateKeySecret: ptr.To("desired-key")},
				}}},
				Status: appsv1.ChainNodeStatus{
					CosmosignerAppliedDigest: "old-digest",
					CosmosignerPublicKey:     oldParsed.PubKey.Value,
				},
			}
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "desired-key", Namespace: "default"}, Data: map[string][]byte{
				PrivKeyFilename: tc.desiredKey,
			}}
			client := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&appsv1.ChainNode{}).WithObjects(chainNode, secret).Build()
			r := &Reconciler{Client: client, Scheme: scheme}
			params := cosmosigner.Params{
				Name: cosmosignerName(chainNode), Namespace: chainNode.Namespace, ChainID: "test-1", Image: "image", Replicas: 1,
				ExpectedPublicKey: func() string {
					parsed, parseErr := cometbft.LoadPrivKey(tc.desiredKey)
					require.NoError(t, parseErr)
					return parsed.PubKey.Value
				}(),
				StateStorageSize: "1Gi",
				Backend:          cosmosigner.Backend{Software: &cosmosigner.SoftwareBackend{SecretName: secret.Name}},
			}

			pending, err := r.reconcileCosmosignerMigration(context.Background(), chainNode, params)
			require.NoError(t, err)
			require.True(t, pending)
			require.NotNil(t, chainNode.Status.CosmosignerMigration)
			require.Equal(t, tc.wantReset, chainNode.Status.CosmosignerMigration.ResetState)
			require.Equal(t, appsv1.CosmosignerMigrationQuiescing, chainNode.Status.CosmosignerMigration.Phase)
		})
	}
}

func TestReconcileCosmosignerMigrationQuiescesRuntimeOnlyChange(t *testing.T) {
	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "sentry", Namespace: "default", UID: "sentry-uid"},
		Spec: appsv1.ChainNodeSpec{Cosmosigner: &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{
			Software: &appsv1.CosmosignerSoftwareBackend{PrivateKeySecret: ptr.To("sentry-key")},
		}}},
	}
	const publicKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	params := cosmosigner.Params{
		Name: cosmosignerName(chainNode), Namespace: chainNode.Namespace, ChainID: "test-1", Image: "new-image", Replicas: 1,
		ExpectedPublicKey: publicKey, StateStorageSize: "1Gi",
		Backend: cosmosigner.Backend{Software: &cosmosigner.SoftwareBackend{SecretName: "sentry-key"}},
	}
	oldParams := params
	oldParams.Image = "old-image"
	oldDigest, err := oldParams.LifecycleDigest(chainNode.CosmosignerSigningDigest())
	require.NoError(t, err)
	chainNode.Status.CosmosignerAppliedDigest = oldDigest
	chainNode.Status.CosmosignerPublicKey = publicKey

	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	r := &Reconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&appsv1.ChainNode{}).WithObjects(chainNode).Build(), Scheme: scheme}

	pending, err := r.reconcileCosmosignerMigration(context.Background(), chainNode, params)
	require.NoError(t, err)
	require.True(t, pending)
	require.NotNil(t, chainNode.Status.CosmosignerMigration)
	require.False(t, chainNode.Status.CosmosignerMigration.ResetState)
	require.Equal(t, appsv1.CosmosignerMigrationQuiescing, chainNode.Status.CosmosignerMigration.Phase)
}

func TestPreflightCosmosignerRefusesEstablishedSignerWithoutRaftState(t *testing.T) {
	key, err := cometbft.GeneratePrivKey()
	require.NoError(t, err)
	parsed, err := cometbft.LoadPrivKey(key)
	require.NoError(t, err)

	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "sentry", Namespace: "default", UID: "sentry-uid"},
		Spec: appsv1.ChainNodeSpec{Cosmosigner: &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{
			Software: &appsv1.CosmosignerSoftwareBackend{PrivateKeySecret: ptr.To("sentry-key")},
		}}},
		Status: appsv1.ChainNodeStatus{
			ChainID:                  "test-1",
			CosmosignerAppliedDigest: "established-digest",
			CosmosignerPublicKey:     parsed.PubKey.Value,
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "sentry-key", Namespace: chainNode.Namespace},
		Data:       map[string][]byte{PrivKeyFilename: key},
	}
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, k8sappsv1.AddToScheme(scheme))
	r := &Reconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build(),
		Scheme: scheme,
		opts:   &controllers.ControllerRunOptions{},
	}

	_, err = r.preflightCosmosigner(context.Background(), chainNode)
	require.ErrorContains(t, err, "retained raft-state PVC")

	chainNode.Status.CosmosignerReplicas = ptr.To(int32(2))
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: "data-" + cosmosignerName(chainNode) + "-0", Namespace: chainNode.Namespace,
		Labels: map[string]string{"cosmopilot.voluzi.com/cosmosigner-owner": string(chainNode.UID)},
	}, Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "sentry-state-0"},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound}}
	require.NoError(t, r.Create(context.Background(), pvc))
	_, err = r.preflightCosmosigner(context.Background(), chainNode)
	require.ErrorContains(t, err, "data-"+cosmosignerName(chainNode)+"-1")

	chainNode.Status.CosmosignerMigration = &appsv1.CosmosignerMigrationStatus{
		Phase:      appsv1.CosmosignerMigrationQuiescing,
		ResetState: true,
	}
	_, err = r.preflightCosmosigner(context.Background(), chainNode)
	require.ErrorContains(t, err, "retained raft-state PVC")

	chainNode.Status.CosmosignerMigration.Phase = appsv1.CosmosignerMigrationResettingState
	_, err = r.preflightCosmosigner(context.Background(), chainNode)
	require.NoError(t, err)
}

func TestPreflightCosmosignerRejectsDifferentRecordedValidatorPublicKey(t *testing.T) {
	key, err := cometbft.GeneratePrivKey()
	require.NoError(t, err)
	parsed, err := cometbft.LoadPrivKey(key)
	require.NoError(t, err)
	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "validator", Namespace: "default", UID: "validator-uid"},
		Spec: appsv1.ChainNodeSpec{
			Validator:   &appsv1.ValidatorConfig{PrivateKeySecret: ptr.To("validator-key")},
			Cosmosigner: &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{Software: &appsv1.CosmosignerSoftwareBackend{}}},
		},
		Status: appsv1.ChainNodeStatus{
			ChainID: "test-1",
			PubKey:  `{"@type":"/cosmos.crypto.ed25519.PubKey","key":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}`,
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "validator-key", Namespace: chainNode.Namespace},
		Data:       map[string][]byte{PrivKeyFilename: key},
	}
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, k8sappsv1.AddToScheme(scheme))
	r := &Reconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build(),
		Scheme: scheme, opts: &controllers.ControllerRunOptions{},
	}

	_, err = r.preflightCosmosigner(context.Background(), chainNode)
	require.ErrorContains(t, err, "on-chain validator public key")
	require.NotEqual(t, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", parsed.PubKey.Value)
	reservation := &appsv1.ConsensusKeyReservation{}
	getErr := r.Get(context.Background(), client.ObjectKey{Name: cosmosigner.ConsensusKeyReservationName("test-1", parsed.PubKey.Value)}, reservation)
	require.True(t, apierrors.IsNotFound(getErr), "a rejected signer key must not leave an immutable reservation: %v", getErr)
}

func TestReconcileCosmosignerMigrationRequeuesAfterRecoveringLiveLifecycle(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, k8sappsv1.AddToScheme(scheme))

	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "sentry", Namespace: "default", UID: "sentry-uid"},
		Spec: appsv1.ChainNodeSpec{Cosmosigner: &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{
			Software: &appsv1.CosmosignerSoftwareBackend{PrivateKeySecret: ptr.To("sentry-key")},
		}}},
	}
	params := cosmosigner.Params{
		Name: cosmosignerName(chainNode), Namespace: chainNode.Namespace, ChainID: "test-1", Image: "new-image", Replicas: 1,
		ExpectedPublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", StateStorageSize: "1Gi",
		Backend: cosmosigner.Backend{Software: &cosmosigner.SoftwareBackend{SecretName: "sentry-key"}},
	}
	oldParams := params
	oldParams.Image = "old-image"
	oldDigest, err := oldParams.LifecycleDigest(chainNode.CosmosignerSigningDigest())
	require.NoError(t, err)

	one := int32(1)
	sts := &k8sappsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: cosmosignerName(chainNode), Namespace: chainNode.Namespace, Generation: 1,
			Annotations: map[string]string{cosmosigner.LifecycleDigestAnnotation: oldDigest},
		},
		Spec: k8sappsv1.StatefulSetSpec{Replicas: &one},
		Status: k8sappsv1.StatefulSetStatus{
			ObservedGeneration: 1, UpdatedReplicas: 1, ReadyReplicas: 1,
		},
	}
	require.NoError(t, controllerutil.SetControllerReference(chainNode, sts, scheme))
	r := &Reconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&appsv1.ChainNode{}).WithObjects(chainNode, sts).Build(),
		Scheme: scheme,
	}

	pending, err := r.reconcileCosmosignerMigration(context.Background(), chainNode, params)
	require.NoError(t, err)
	require.True(t, pending, "recovery must stop this reconcile before desired services or StatefulSet fields are applied")
	require.Equal(t, oldDigest, chainNode.Status.CosmosignerAppliedDigest)
}

func TestReconcileCosmosignerMigrationRecoversUnreadyLiveLifecycle(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, k8sappsv1.AddToScheme(scheme))

	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "sentry", Namespace: "default", UID: "sentry-uid"},
		Spec: appsv1.ChainNodeSpec{Cosmosigner: &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{
			Software: &appsv1.CosmosignerSoftwareBackend{PrivateKeySecret: ptr.To("sentry-key")},
		}}},
	}
	params := cosmosigner.Params{
		Name: cosmosignerName(chainNode), Namespace: chainNode.Namespace, ChainID: "test-1", Image: "fixed-image", Replicas: 1,
		ExpectedPublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", StateStorageSize: "1Gi",
		Backend: cosmosigner.Backend{Software: &cosmosigner.SoftwareBackend{SecretName: "sentry-key"}},
	}
	liveParams := params
	liveParams.Image = "broken-image"
	liveDigest, err := liveParams.LifecycleDigest(chainNode.CosmosignerSigningDigest())
	require.NoError(t, err)

	one := int32(1)
	sts := &k8sappsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: cosmosignerName(chainNode), Namespace: chainNode.Namespace, Generation: 1,
			Annotations: map[string]string{cosmosigner.LifecycleDigestAnnotation: liveDigest},
		},
		Spec:   k8sappsv1.StatefulSetSpec{Replicas: &one},
		Status: k8sappsv1.StatefulSetStatus{ObservedGeneration: 1},
	}
	require.NoError(t, controllerutil.SetControllerReference(chainNode, sts, scheme))
	r := &Reconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&appsv1.ChainNode{}).WithObjects(chainNode, sts).Build(),
		Scheme: scheme,
	}

	pending, err := r.reconcileCosmosignerMigration(context.Background(), chainNode, params)
	require.NoError(t, err)
	require.True(t, pending)
	require.Equal(t, liveDigest, chainNode.Status.CosmosignerAppliedDigest)
	require.Equal(t, params.ExpectedPublicKey, chainNode.Status.CosmosignerPublicKey)
	require.Nil(t, chainNode.Status.CosmosignerMigration)
}

func TestReconcileCosmosignerMigrationRollingOutPreservesNewStatefulSet(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, k8sappsv1.AddToScheme(scheme))
	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "sentry", Namespace: "default", UID: "sentry-uid"},
		Spec: appsv1.ChainNodeSpec{Cosmosigner: &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{
			Software: &appsv1.CosmosignerSoftwareBackend{PrivateKeySecret: ptr.To("sentry-key")},
		}}},
	}
	params := cosmosigner.Params{
		Name: cosmosignerName(chainNode), Namespace: chainNode.Namespace, ChainID: "test-1", Image: "image", Replicas: 1,
		ExpectedPublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", StateStorageSize: "1Gi",
		Backend: cosmosigner.Backend{Software: &cosmosigner.SoftwareBackend{SecretName: "sentry-key"}},
	}
	desiredDigest, err := params.LifecycleDigest(chainNode.CosmosignerSigningDigest())
	require.NoError(t, err)
	chainNode.Status.CosmosignerAppliedDigest = "old-digest"
	chainNode.Status.CosmosignerPublicKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	chainNode.Status.CosmosignerMigration = &appsv1.CosmosignerMigrationStatus{
		DesiredDigest: desiredDigest, DesiredPublicKey: chainNode.Status.CosmosignerPublicKey,
		Phase: appsv1.CosmosignerMigrationRollingOut,
	}
	one := int32(1)
	sts := &k8sappsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: cosmosignerName(chainNode), Namespace: chainNode.Namespace}, Spec: k8sappsv1.StatefulSetSpec{Replicas: &one}}
	require.NoError(t, controllerutil.SetControllerReference(chainNode, sts, scheme))
	r := &Reconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(chainNode, sts).Build(), Scheme: scheme}

	pending, err := r.reconcileCosmosignerMigration(context.Background(), chainNode, params)
	require.NoError(t, err)
	require.False(t, pending)
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: sts.Namespace, Name: sts.Name}, &k8sappsv1.StatefulSet{}))
}

func TestMaybeImportCosmosignerKeyRejectsMalformedSourceBeforeScaleDown(t *testing.T) {
	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "validator", Namespace: "default", UID: "validator-uid"},
		Spec: appsv1.ChainNodeSpec{
			Validator: &appsv1.ValidatorConfig{PrivateKeySecret: ptr.To("validator-key")},
			Cosmosigner: &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{Vault: &appsv1.CosmosignerVaultBackend{
				Address: "https://vault:8200", KeyName: "validator-key", UploadGenerated: true,
			}}},
		},
	}
	source := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "validator-key", Namespace: chainNode.Namespace},
		Data:       map[string][]byte{PrivKeyFilename: []byte("malformed-key")},
	}
	sts := &k8sappsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: cosmosignerName(chainNode), Namespace: chainNode.Namespace},
		Spec:       k8sappsv1.StatefulSetSpec{Replicas: ptr.To(int32(1))},
	}
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, k8sappsv1.AddToScheme(scheme))
	require.NoError(t, controllerutil.SetControllerReference(chainNode, sts, scheme))
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(source, sts).Build()
	r := &Reconciler{Client: client, Scheme: scheme}

	_, err := r.maybeImportCosmosignerKey(context.Background(), chainNode, cosmosigner.Params{Name: sts.Name})
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid")

	fresh := &k8sappsv1.StatefulSet{}
	require.NoError(t, client.Get(context.Background(), types.NamespacedName{Namespace: sts.Namespace, Name: sts.Name}, fresh))
	require.NotNil(t, fresh.Spec.Replicas)
	require.Equal(t, int32(1), *fresh.Spec.Replicas)
}

func TestMaybeImportCosmosignerKeyUpgradesLegacyFingerprintWithoutScaleDown(t *testing.T) {
	vault := &appsv1.CosmosignerVaultBackend{
		Address: "https://vault:8200", KeyName: "validator-key", UploadGenerated: true,
	}
	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "validator", Namespace: "default", UID: "validator-uid"},
		Spec: appsv1.ChainNodeSpec{
			Validator:   &appsv1.ValidatorConfig{PrivateKeySecret: ptr.To("validator-key")},
			Cosmosigner: &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{Vault: vault}},
		},
	}
	key, err := cometbft.GeneratePrivKey()
	require.NoError(t, err)
	chainNode.Status.CosmosignerKeyImported = vault.LegacyImportFingerprint("validator-key", key)
	source := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "validator-key", Namespace: chainNode.Namespace},
		Data:       map[string][]byte{PrivKeyFilename: key},
	}
	sts := &k8sappsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: cosmosignerName(chainNode), Namespace: chainNode.Namespace},
		Spec:       k8sappsv1.StatefulSetSpec{Replicas: ptr.To(int32(1))},
	}
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, k8sappsv1.AddToScheme(scheme))
	require.NoError(t, controllerutil.SetControllerReference(chainNode, sts, scheme))
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&appsv1.ChainNode{}).WithObjects(chainNode, source, sts).Build()
	r := &Reconciler{Client: fakeClient, Scheme: scheme}

	pending, err := r.maybeImportCosmosignerKey(context.Background(), chainNode, cosmosigner.Params{Name: sts.Name})
	require.NoError(t, err)
	require.False(t, pending)
	require.Equal(t, vault.ImportFingerprint("validator-key", key), chainNode.Status.CosmosignerKeyImported)

	fresh := &k8sappsv1.StatefulSet{}
	require.NoError(t, fakeClient.Get(context.Background(), client.ObjectKeyFromObject(sts), fresh))
	require.Equal(t, int32(1), ptr.Deref(fresh.Spec.Replicas, 0), "format-only status upgrade must not quiesce the live signer")
}

func TestMaybeImportCosmosignerKeyRejectsInPlaceSourceRotationBeforeScaleDown(t *testing.T) {
	vault := &appsv1.CosmosignerVaultBackend{
		Address: "https://vault:8200", KeyName: "validator-key", UploadGenerated: true,
	}
	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "validator", Namespace: "default", UID: "validator-uid"},
		Spec: appsv1.ChainNodeSpec{
			Validator:   &appsv1.ValidatorConfig{PrivateKeySecret: ptr.To("validator-key")},
			Cosmosigner: &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{Vault: vault}},
		},
	}
	oldKey, err := cometbft.GeneratePrivKey()
	require.NoError(t, err)
	newKey, err := cometbft.GeneratePrivKey()
	require.NoError(t, err)
	chainNode.Status.CosmosignerKeyImported = vault.ImportFingerprint("validator-key", oldKey)
	source := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "validator-key", Namespace: chainNode.Namespace},
		Data:       map[string][]byte{PrivKeyFilename: newKey},
	}
	sts := &k8sappsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: cosmosignerName(chainNode), Namespace: chainNode.Namespace},
		Spec:       k8sappsv1.StatefulSetSpec{Replicas: ptr.To(int32(1))},
	}
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, k8sappsv1.AddToScheme(scheme))
	require.NoError(t, controllerutil.SetControllerReference(chainNode, sts, scheme))
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(source, sts).Build()
	r := &Reconciler{Client: fakeClient, Scheme: scheme}

	pending, err := r.maybeImportCosmosignerKey(context.Background(), chainNode, cosmosigner.Params{Name: sts.Name})
	require.ErrorContains(t, err, "new Vault keyName")
	require.False(t, pending)

	fresh := &k8sappsv1.StatefulSet{}
	require.NoError(t, fakeClient.Get(context.Background(), client.ObjectKeyFromObject(sts), fresh))
	require.Equal(t, int32(1), ptr.Deref(fresh.Spec.Replicas, 0), "an unsupported in-place rekey must leave the serving signer intact")
}

func TestMaybeImportCosmosignerKeyReimportsForVaultTargetMigration(t *testing.T) {
	oldVault := &appsv1.CosmosignerVaultBackend{
		Address: "https://vault:8200", KeyName: "old-key", UploadGenerated: true,
	}
	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "validator", Namespace: "default", UID: "validator-uid"},
		Spec: appsv1.ChainNodeSpec{
			Validator:   &appsv1.ValidatorConfig{PrivateKeySecret: ptr.To("validator-key")},
			Cosmosigner: &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{Vault: oldVault}},
		},
	}
	key, err := cometbft.GeneratePrivKey()
	require.NoError(t, err)
	chainNode.Status.CosmosignerSigningDigest = chainNode.CosmosignerSigningDigest()
	chainNode.Status.CosmosignerKeyImported = oldVault.ImportFingerprint("validator-key", key)
	chainNode.Spec.Cosmosigner.Backend.Vault.KeyName = "new-key"
	require.NotEqual(t, chainNode.Status.CosmosignerSigningDigest, chainNode.CosmosignerSigningDigest())

	source := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "validator-key", Namespace: chainNode.Namespace},
		Data:       map[string][]byte{PrivKeyFilename: key},
	}
	sts := &k8sappsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: cosmosignerName(chainNode), Namespace: chainNode.Namespace},
		Spec:       k8sappsv1.StatefulSetSpec{Replicas: ptr.To(int32(1))},
	}
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, k8sappsv1.AddToScheme(scheme))
	require.NoError(t, controllerutil.SetControllerReference(chainNode, sts, scheme))
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(source, sts).Build()
	r := &Reconciler{Client: fakeClient, Scheme: scheme}

	pending, err := r.maybeImportCosmosignerKey(context.Background(), chainNode, cosmosigner.Params{Name: sts.Name})
	require.NoError(t, err)
	require.True(t, pending)

	fresh := &k8sappsv1.StatefulSet{}
	require.NoError(t, fakeClient.Get(context.Background(), client.ObjectKeyFromObject(sts), fresh))
	require.NotNil(t, fresh.Spec.Replicas)
	require.Zero(t, *fresh.Spec.Replicas)
}

func TestReconcileSigningConfigsValidatesMigrationSourceBeforeQuiescing(t *testing.T) {
	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "validator", Namespace: "default", UID: "validator-uid"},
		Spec: appsv1.ChainNodeSpec{
			Validator: &appsv1.ValidatorConfig{PrivateKeySecret: ptr.To("validator-key")},
			Cosmosigner: &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{Vault: &appsv1.CosmosignerVaultBackend{
				Address: "https://vault:8200", KeyName: "old-key", UploadGenerated: true,
				TokenSecret: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "vault-token"}, Key: "token"},
			}}},
		},
		Status: appsv1.ChainNodeStatus{ChainID: "chain-1"},
	}
	oldDigest := chainNode.CosmosignerSigningDigest()
	chainNode.Spec.Cosmosigner.Backend.Vault.KeyName = "new-key"
	desiredDigest := chainNode.CosmosignerSigningDigest()
	chainNode.Status.CosmosignerSigningDigest = oldDigest
	chainNode.Status.CosmosignerAppliedDigest = oldDigest
	chainNode.Status.CosmosignerPublicKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	chainNode.Status.CosmosignerReplicas = ptr.To(int32(1))
	chainNode.Status.CosmosignerStateStorageSize = chainNode.Spec.Cosmosigner.GetStateStorageSize()
	chainNode.Status.CosmosignerValidatorTargeted = ptr.To(true)
	chainNode.Status.CosmosignerMigration = &appsv1.CosmosignerMigrationStatus{
		DesiredDigest:    desiredDigest,
		DesiredPublicKey: chainNode.Status.CosmosignerPublicKey,
		Phase:            appsv1.CosmosignerMigrationQuiescing,
	}

	token := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "vault-token", Namespace: chainNode.Namespace},
		Data:       map[string][]byte{"token": []byte("test-token")},
	}
	source := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "validator-key", Namespace: chainNode.Namespace},
		Data:       map[string][]byte{PrivKeyFilename: []byte("malformed-key")},
	}
	sts := &k8sappsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: cosmosignerName(chainNode), Namespace: chainNode.Namespace},
		Spec:       k8sappsv1.StatefulSetSpec{Replicas: ptr.To(int32(1))},
	}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: "data-" + cosmosignerName(chainNode) + "-0", Namespace: chainNode.Namespace,
		Labels: map[string]string{"cosmopilot.voluzi.com/cosmosigner-owner": string(chainNode.UID)},
	}, Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "validator-state-0"},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound}}
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, k8sappsv1.AddToScheme(scheme))
	require.NoError(t, controllerutil.SetControllerReference(chainNode, sts, scheme))
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&appsv1.ChainNode{}, &k8sappsv1.StatefulSet{}).
		WithObjects(chainNode, token, source, sts, pvc).Build()
	r := &Reconciler{Client: fakeClient, Scheme: scheme, opts: &controllers.ControllerRunOptions{}}

	pending, err := r.reconcileSigningConfigs(context.Background(), chainNode)
	require.Error(t, err)
	require.False(t, pending)
	require.Contains(t, err.Error(), "invalid")

	fresh := &k8sappsv1.StatefulSet{}
	require.NoError(t, fakeClient.Get(context.Background(), client.ObjectKeyFromObject(sts), fresh))
	require.Equal(t, int32(1), ptr.Deref(fresh.Spec.Replicas, 0))
}

func TestEnsureCosmosignerRejectsRecoveredLockMismatch(t *testing.T) {
	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "validator", Namespace: "default", UID: types.UID("validator-uid")},
		Spec: appsv1.ChainNodeSpec{
			Validator:   &appsv1.ValidatorConfig{},
			Cosmosigner: &appsv1.Cosmosigner{Replicas: ptr.To(int32(1))},
		},
		Status: appsv1.ChainNodeStatus{ChainID: "test-1"},
	}
	sts := &k8sappsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: cosmosignerName(chainNode), Namespace: chainNode.Namespace},
		Spec:       k8sappsv1.StatefulSetSpec{Replicas: ptr.To(int32(3))},
	}
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, k8sappsv1.AddToScheme(scheme))
	require.NoError(t, controllerutil.SetControllerReference(chainNode, sts, scheme))
	client := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&appsv1.ChainNode{}).
		WithObjects(chainNode, sts).
		Build()
	r := &Reconciler{Client: client, Scheme: scheme}

	wait, err := r.ensureCosmosignerWithParams(context.Background(), chainNode, cosmosigner.Params{Name: sts.Name, Replicas: 1})
	require.Error(t, err)
	require.False(t, wait)
	require.Contains(t, err.Error(), "replicas are immutable")

	fresh := &k8sappsv1.StatefulSet{}
	require.NoError(t, client.Get(context.Background(), types.NamespacedName{Namespace: sts.Namespace, Name: sts.Name}, fresh))
	require.Equal(t, int32(3), ptr.Deref(fresh.Spec.Replicas, 0))
}

func TestPreflightCosmosignerRejectsRecoveredIdentityMismatch(t *testing.T) {
	requireRecoveredStandaloneIdentityMismatchRejected(t, true, true, true)
}

func TestPreflightCosmosignerRejectsLiveIdentityMismatchWithLostStatus(t *testing.T) {
	requireRecoveredStandaloneIdentityMismatchRejected(t, true, true, false)
}

func TestPreflightCosmosignerRejectsRecoveredIdentityMismatchAfterValidatorDemotion(t *testing.T) {
	requireRecoveredStandaloneIdentityMismatchRejected(t, false, true, true)
}

func TestPreflightCosmosignerRejectsRecoveredSentryIdentityMismatch(t *testing.T) {
	requireRecoveredStandaloneIdentityMismatchRejected(t, false, false, true)
}

func requireRecoveredStandaloneIdentityMismatchRejected(t *testing.T, currentValidator, recordedValidator, recordedLocks bool) {
	t.Helper()
	tokenSelector := &corev1.SecretKeySelector{
		LocalObjectReference: corev1.LocalObjectReference{Name: "vault-token"},
		Key:                  "token",
	}
	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "validator", Namespace: "default", UID: types.UID("validator-uid")},
		Spec: appsv1.ChainNodeSpec{
			Cosmosigner: &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{Vault: &appsv1.CosmosignerVaultBackend{
				Address: "https://vault.example:8200", KeyName: "desired-key", TokenSecret: tokenSelector,
			}}},
		},
		Status: appsv1.ChainNodeStatus{
			ChainID: "test-1",
		},
	}
	if recordedLocks {
		chainNode.Status.CosmosignerReplicas = ptr.To(int32(1))
		chainNode.Status.CosmosignerStateStorageSize = appsv1.DefaultCosmosignerStateStorageSize
		chainNode.Status.CosmosignerValidatorTargeted = ptr.To(recordedValidator)
	}
	if currentValidator {
		chainNode.Spec.Validator = &appsv1.ValidatorConfig{}
	}
	liveParams := cosmosigner.Params{
		Name:              cosmosignerName(chainNode),
		Namespace:         chainNode.Namespace,
		OwnerUID:          chainNode.UID,
		ChainID:           chainNode.Status.ChainID,
		Replicas:          1,
		ExpectedPublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		StateStorageSize:  appsv1.DefaultCosmosignerStateStorageSize,
		Backend: cosmosigner.Backend{Vault: &cosmosigner.VaultBackend{
			Address: "https://vault.example:8200", KeyName: "live-key", Mount: appsv1.DefaultCosmosignerVaultMount, TokenSecret: tokenSelector,
		}},
	}
	liveConfig, err := liveParams.ConfigYAML()
	require.NoError(t, err)
	liveConfigMap, err := liveParams.ConfigMap(liveConfig)
	require.NoError(t, err)
	liveStatefulSet, err := liveParams.StatefulSet(liveConfig)
	require.NoError(t, err)
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, k8sappsv1.AddToScheme(scheme))
	require.NoError(t, controllerutil.SetControllerReference(chainNode, liveConfigMap, scheme))
	require.NoError(t, controllerutil.SetControllerReference(chainNode, liveStatefulSet, scheme))
	token := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: tokenSelector.Name, Namespace: chainNode.Namespace},
		Data:       map[string][]byte{tokenSelector.Key: []byte("token")},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(token, liveConfigMap, liveStatefulSet).Build()
	r := &Reconciler{Client: client, Scheme: scheme, opts: &controllers.ControllerRunOptions{}}

	_, err = r.preflightCosmosigner(context.Background(), chainNode)
	require.Error(t, err)
	require.Contains(t, err.Error(), "live signing identity")

	fresh := &corev1.ConfigMap{}
	require.NoError(t, client.Get(context.Background(), types.NamespacedName{Namespace: chainNode.Namespace, Name: liveParams.Name}, fresh))
	require.Equal(t, liveConfig, fresh.Data["config.yaml"])
}

func TestPreflightCosmosignerImportSourceIgnoresLegacyAnnotation(t *testing.T) {
	vault := &appsv1.CosmosignerVaultBackend{
		Address: "https://vault:8200", KeyName: "validator-key", UploadGenerated: true,
	}
	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "validator",
			Namespace: "default",
			Annotations: map[string]string{
				"cosmopilot.voluzi.com/cosmosigner-key-imported": vault.ImportFingerprint("validator-key", []byte("unknown-key")),
			},
		},
		Spec: appsv1.ChainNodeSpec{
			Validator:   &appsv1.ValidatorConfig{PrivateKeySecret: ptr.To("validator-key")},
			Cosmosigner: &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{Vault: vault}},
		},
	}
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	r := &Reconciler{Client: fake.NewClientBuilder().WithScheme(scheme).Build(), Scheme: scheme}

	err := r.preflightCosmosignerImportSource(context.Background(), chainNode)
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing")
	_, err = r.maybeImportCosmosignerKey(context.Background(), chainNode, cosmosigner.Params{Name: cosmosignerName(chainNode)})
	require.Error(t, err)
	require.Contains(t, err.Error(), "missing")
}

func TestPreflightCosmosignerImportSourceUsesRecordedStatus(t *testing.T) {
	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "validator", Namespace: "default"},
		Spec: appsv1.ChainNodeSpec{
			Validator: &appsv1.ValidatorConfig{PrivateKeySecret: ptr.To("validator-key")},
			Cosmosigner: &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{Vault: &appsv1.CosmosignerVaultBackend{
				Address: "https://vault:8200", KeyName: "validator-key", UploadGenerated: true,
			}}},
		},
	}
	want := chainNode.Spec.Cosmosigner.Backend.Vault.ImportFingerprint("validator-key", []byte("imported-key"))
	chainNode.Status.CosmosignerKeyImported = want

	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	r := &Reconciler{Client: fake.NewClientBuilder().WithScheme(scheme).Build(), Scheme: scheme}
	require.NoError(t, r.preflightCosmosignerImportSource(context.Background(), chainNode))
	pending, err := r.maybeImportCosmosignerKey(context.Background(), chainNode, cosmosigner.Params{Name: cosmosignerName(chainNode)})
	require.NoError(t, err)
	require.False(t, pending)
}

func TestEnsureCosmosignerPreflightsLocalFallbackBeforeTeardown(t *testing.T) {
	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "validator", Namespace: "default", UID: "validator-uid"},
		Spec: appsv1.ChainNodeSpec{Validator: &appsv1.ValidatorConfig{
			PrivateKeySecret: ptr.To("validator-key"),
		}},
		Status: appsv1.ChainNodeStatus{
			ChainID:                      "chain-1",
			CosmosignerReplicas:          ptr.To(int32(1)),
			CosmosignerValidatorTargeted: ptr.To(true),
		},
	}
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, k8sappsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	sts := &k8sappsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: cosmosignerName(chainNode), Namespace: chainNode.Namespace}}
	require.NoError(t, controllerutil.SetControllerReference(chainNode, sts, scheme))
	client := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&appsv1.ChainNode{}).WithObjects(chainNode, sts).Build()
	r := &Reconciler{Client: client, Scheme: scheme}

	_, err := r.ensureCosmosigner(context.Background(), chainNode)
	require.Error(t, err)
	require.Contains(t, err.Error(), "validator-key")
	require.NoError(t, client.Get(context.Background(), types.NamespacedName{Namespace: chainNode.Namespace, Name: sts.Name}, &k8sappsv1.StatefulSet{}))

	keySecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "validator-key", Namespace: chainNode.Namespace},
		Data:       map[string][]byte{PrivKeyFilename: []byte("{}")},
	}
	require.NoError(t, client.Create(context.Background(), keySecret))
	err = r.preflightCosmosignerFallback(context.Background(), chainNode)
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid")

	keySecret.Data[PrivKeyFilename] = []byte(`{
		"address":"0000000000000000000000000000000000000000",
		"pub_key":{"type":"tendermint/PubKeyEd25519","value":"eA=="},
		"priv_key":{"type":"tendermint/PrivKeyEd25519","value":"eA=="}
	}`)
	require.NoError(t, client.Update(context.Background(), keySecret))
	err = r.preflightCosmosignerFallback(context.Background(), chainNode)
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid")

	key, err := cometbft.GeneratePrivKey()
	require.NoError(t, err)
	parsed, err := cometbft.LoadPrivKey(key)
	require.NoError(t, err)
	chainNode.Status.CosmosignerPublicKey = parsed.PubKey.Value
	keySecret.Data[PrivKeyFilename] = key
	require.NoError(t, client.Update(context.Background(), keySecret))
	require.NoError(t, r.preflightCosmosignerFallback(context.Background(), chainNode))
}

func TestPreflightCosmosignerFallbackTreatsLegacySigningDigestAsValidator(t *testing.T) {
	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "validator", Namespace: "default"},
		Spec: appsv1.ChainNodeSpec{Validator: &appsv1.ValidatorConfig{
			PrivateKeySecret: ptr.To("validator-key"),
		}},
		Status: appsv1.ChainNodeStatus{CosmosignerSigningDigest: "legacy-served-digest"},
	}
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	r := &Reconciler{Client: fake.NewClientBuilder().WithScheme(scheme).Build(), Scheme: scheme}

	err := r.preflightCosmosignerFallback(context.Background(), chainNode)
	require.Error(t, err)
	require.Contains(t, err.Error(), "validator-key")
}

func TestPreflightCosmosignerFallbackRejectsOwnedSignerWithLostStatus(t *testing.T) {
	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "validator", Namespace: "default", UID: "validator-uid"},
		Spec: appsv1.ChainNodeSpec{Validator: &appsv1.ValidatorConfig{
			PrivateKeySecret: ptr.To("validator-key"),
		}},
		Status: appsv1.ChainNodeStatus{ChainID: "chain-1"},
	}
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, k8sappsv1.AddToScheme(scheme))
	sts := &k8sappsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: cosmosignerName(chainNode), Namespace: chainNode.Namespace}}
	require.NoError(t, controllerutil.SetControllerReference(chainNode, sts, scheme))
	r := &Reconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(sts).Build(),
		Scheme: scheme,
	}

	err := r.preflightCosmosignerFallback(context.Background(), chainNode)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "status")
}

func TestPreflightCosmosignerFallbackAllowsRecordedSentryTeardown(t *testing.T) {
	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "sentry", Namespace: "default", UID: "sentry-uid"},
		Status: appsv1.ChainNodeStatus{
			ChainID:                      "chain-1",
			CosmosignerValidatorTargeted: ptr.To(false),
		},
	}
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, k8sappsv1.AddToScheme(scheme))
	sts := &k8sappsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: cosmosignerName(chainNode), Namespace: chainNode.Namespace}}
	require.NoError(t, controllerutil.SetControllerReference(chainNode, sts, scheme))
	r := &Reconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(sts).Build(),
		Scheme: scheme,
	}

	require.NoError(t, r.preflightCosmosignerFallback(context.Background(), chainNode))
}

func TestPreflightCosmosignerFallbackRequiresMatchingLocalPublicKey(t *testing.T) {
	servedKey, err := cometbft.GeneratePrivKey()
	require.NoError(t, err)
	served, err := cometbft.LoadPrivKey(servedKey)
	require.NoError(t, err)
	fallbackKey, err := cometbft.GeneratePrivKey()
	require.NoError(t, err)

	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "validator", Namespace: "default"},
		Spec: appsv1.ChainNodeSpec{Validator: &appsv1.ValidatorConfig{
			PrivateKeySecret: ptr.To("validator-key"),
		}},
		Status: appsv1.ChainNodeStatus{
			CosmosignerValidatorTargeted: ptr.To(true),
			CosmosignerPublicKey:         served.PubKey.Value,
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "validator-key", Namespace: "default"},
		Data:       map[string][]byte{PrivKeyFilename: fallbackKey},
	}
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	r := &Reconciler{Client: client, Scheme: scheme}

	err = r.preflightCosmosignerFallback(context.Background(), chainNode)
	require.Error(t, err)
	require.Contains(t, err.Error(), "does not match")

	secret.Data[PrivKeyFilename] = servedKey
	require.NoError(t, client.Update(context.Background(), secret))
	require.NoError(t, r.preflightCosmosignerFallback(context.Background(), chainNode))
}

func TestPreflightCosmosignerFallbackDoesNotTrustDifferentTmKMSTarget(t *testing.T) {
	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "validator", Namespace: "default"},
		Spec: appsv1.ChainNodeSpec{Validator: &appsv1.ValidatorConfig{TmKMS: &appsv1.TmKMS{Provider: appsv1.TmKmsProvider{
			Hashicorp: &appsv1.TmKmsHashicorpProvider{
				Address: "https://vault:8200",
				Key:     "fallback-key",
				TokenSecret: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "tmkms-token"},
					Key:                  "token",
				},
				SkipCertificateVerify: true,
			},
		}}}},
		Status: appsv1.ChainNodeStatus{
			CosmosignerServingIdentity: "vault\x00https://vault:8200\x00\x00transit\x00served-key",
			CosmosignerPublicKey:       "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		},
	}
	token := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "tmkms-token", Namespace: "default"},
		Data:       map[string][]byte{"token": []byte("vault-token")},
	}
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	var createdPod string
	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		statusCode := http.StatusNotFound
		body := `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`
		if req.Method == http.MethodPost {
			data, readErr := io.ReadAll(req.Body)
			require.NoError(t, readErr)
			createdPod = string(data)
			statusCode = http.StatusInternalServerError
			body = `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"InternalError","message":"forced pubkey failure","code":500}`
		}
		return &http.Response{
			StatusCode: statusCode,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})
	clientSet, err := kubernetes.NewForConfig(&rest.Config{
		Host: "https://kubernetes.invalid", ContentConfig: rest.ContentConfig{ContentType: "application/json"}, Transport: transport,
	})
	require.NoError(t, err)
	r := &Reconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(token).Build(),
		Scheme: scheme, ClientSet: clientSet,
	}

	err = r.preflightCosmosignerFallback(context.Background(), chainNode)
	require.Error(t, err)
	require.Contains(t, createdPod, "VAULT_SKIP_VERIFY")
	require.Contains(t, createdPod, "true")
	require.Contains(t, createdPod, `"--vault-key-version","1"`)
}

func TestCosmosignerPublicKeyUsesVaultAfterImportedSourceRemoval(t *testing.T) {
	vault := &appsv1.CosmosignerVaultBackend{
		Address: "https://vault:8200", KeyName: "validator-key", UploadGenerated: true,
	}
	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "validator", Namespace: "default"},
		Spec: appsv1.ChainNodeSpec{
			Validator:   &appsv1.ValidatorConfig{PrivateKeySecret: ptr.To("validator-key")},
			Cosmosigner: &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{Vault: vault}},
		},
		Status: appsv1.ChainNodeStatus{
			CosmosignerKeyImported: vault.ImportFingerprint("validator-key", []byte("imported-key")),
		},
	}
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	r := &Reconciler{Client: fake.NewClientBuilder().WithScheme(scheme).Build(), Scheme: scheme}
	params := cosmosigner.Params{Backend: cosmosigner.Backend{Vault: &cosmosigner.VaultBackend{}}}

	_, err := r.cosmosignerPublicKey(context.Background(), chainNode, params)
	require.Error(t, err)
	require.Contains(t, err.Error(), "Kubernetes clientset")

	chainNode.Status.CosmosignerKeyImported = ""
	_, err = r.cosmosignerPublicKey(context.Background(), chainNode, params)
	require.Error(t, err)
	require.Contains(t, err.Error(), "validator-key")
}

func TestPreflightCosmosignerFallbackRequiresTmKMSSecrets(t *testing.T) {
	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "validator", Namespace: "default"},
		Spec: appsv1.ChainNodeSpec{Validator: &appsv1.ValidatorConfig{TmKMS: &appsv1.TmKMS{Provider: appsv1.TmKmsProvider{
			Hashicorp: &appsv1.TmKmsHashicorpProvider{
				Address: "https://vault:8200",
				Key:     "validator-key",
				TokenSecret: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "tmkms-token"},
					Key:                  "token",
				},
				CertificateSecret: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "tmkms-ca"},
					Key:                  "ca.crt",
				},
			},
		}}}},
		Status: appsv1.ChainNodeStatus{CosmosignerValidatorTargeted: ptr.To(true)},
	}
	chainNode.Status.CosmosignerServingIdentity = chainNode.EffectiveSigningIdentity()
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	client := fake.NewClientBuilder().WithScheme(scheme).Build()
	r := &Reconciler{Client: client, Scheme: scheme}

	err := r.preflightCosmosignerFallback(context.Background(), chainNode)
	require.Error(t, err)
	require.Contains(t, err.Error(), "tmKMS Vault token")
	require.NoError(t, client.Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "tmkms-token", Namespace: chainNode.Namespace},
		Data:       map[string][]byte{"token": []byte("token")},
	}))
	err = r.preflightCosmosignerFallback(context.Background(), chainNode)
	require.Error(t, err)
	require.Contains(t, err.Error(), "tmKMS Vault certificate")
	require.NoError(t, client.Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "tmkms-ca", Namespace: chainNode.Namespace},
		Data:       map[string][]byte{"ca.crt": []byte("certificate")},
	}))
	require.NoError(t, r.preflightCosmosignerFallback(context.Background(), chainNode))
}

func TestPreflightCosmosignerFallbackUsesRecordedServingIdentity(t *testing.T) {
	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "validator", Namespace: "default"},
		Spec: appsv1.ChainNodeSpec{Validator: &appsv1.ValidatorConfig{
			PrivateKeySecret: ptr.To("validator-key"),
		}},
		Status: appsv1.ChainNodeStatus{CosmosignerServingIdentity: "software\x00validator-key"},
	}
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	r := &Reconciler{Client: fake.NewClientBuilder().WithScheme(scheme).Build(), Scheme: scheme}

	err := r.preflightCosmosignerFallback(context.Background(), chainNode)
	require.Error(t, err)
	require.Contains(t, err.Error(), "validator-key")
}

func TestPreflightCosmosignerFallbackRequiresTmKMSTarget(t *testing.T) {
	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "validator", Namespace: "default"},
		Spec: appsv1.ChainNodeSpec{Validator: &appsv1.ValidatorConfig{TmKMS: &appsv1.TmKMS{Provider: appsv1.TmKmsProvider{
			Hashicorp: &appsv1.TmKmsHashicorpProvider{
				TokenSecret: &corev1.SecretKeySelector{
					LocalObjectReference: corev1.LocalObjectReference{Name: "tmkms-token"},
					Key:                  "token",
				},
			},
		}}}},
		Status: appsv1.ChainNodeStatus{CosmosignerValidatorTargeted: ptr.To(true)},
	}
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	r := &Reconciler{Client: fake.NewClientBuilder().WithScheme(scheme).Build(), Scheme: scheme}

	err := r.preflightCosmosignerFallback(context.Background(), chainNode)
	require.Error(t, err)
	require.Contains(t, err.Error(), "address and key")
}

func TestCosmosignerBackendRejectsMalformedSoftwareKey(t *testing.T) {
	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "validator", Namespace: "default"},
		Spec: appsv1.ChainNodeSpec{
			Validator: &appsv1.ValidatorConfig{
				PrivateKeySecret: ptr.To("validator-key"),
				CreateValidator:  &appsv1.CreateValidatorConfig{},
			},
			Cosmosigner: &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{Software: &appsv1.CosmosignerSoftwareBackend{}}},
		},
		Status: appsv1.ChainNodeStatus{ChainID: "test-1"},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "validator-key", Namespace: "default"},
		Data: map[string][]byte{PrivKeyFilename: []byte(`{
			"address":"0000000000000000000000000000000000000000",
			"pub_key":{"type":"tendermint/PubKeyEd25519","value":"eA=="},
			"priv_key":{"type":"tendermint/PrivKeyEd25519","value":"eA=="}
		}`)},
	}
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	r := &Reconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build(), Scheme: scheme}

	_, err := r.cosmosignerBackend(context.Background(), chainNode)
	require.Error(t, err)
	require.Contains(t, err.Error(), "invalid")
}

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
