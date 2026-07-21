package chainnodeset

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
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/cometbft"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
	"github.com/voluzi/cosmopilot/v2/internal/cosmosigner"
)

func TestReconcileCosmosignerMigrationsWaitsForTerminatingPod(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: "nodeset-uid"},
		Spec: appsv1.ChainNodeSetSpec{Cosmosigner: &appsv1.Cosmosigner{
			NodeGroups: []string{"sentries"},
			Replicas:   ptr.To(int32(1)),
			Backend: appsv1.CosmosignerBackend{Software: &appsv1.CosmosignerSoftwareBackend{
				PrivateKeySecret: ptr.To("sentry-key"),
			}},
		}, Nodes: []appsv1.NodeGroupSpec{{Name: "sentries", Instances: ptr.To(1)}}},
		Status: appsv1.ChainNodeSetStatus{ChainID: "test-1"},
	}
	signer := resolveSingleSigner(t, nodeSet)
	resourceName := "stable-signer"
	nodeSet.Status.Cosmosigners = []appsv1.CosmosignerStatus{{
		Name:          signer.Name,
		ResourceName:  resourceName,
		AppliedDigest: "old-digest",
		PublicKey:     "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		Migration: &appsv1.CosmosignerMigrationStatus{
			DesiredDigest:    signer.Digest(),
			DesiredPublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
			Phase:            appsv1.CosmosignerMigrationQuiescing,
		},
	}}
	zero := int32(0)
	sts := &k8sappsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: resourceName, Namespace: nodeSet.Namespace, UID: "signer-uid", Generation: 2},
		Spec:       k8sappsv1.StatefulSetSpec{Replicas: &zero},
		Status:     k8sappsv1.StatefulSetStatus{ObservedGeneration: 2},
	}
	require.NoError(t, controllerutil.SetControllerReference(nodeSet, sts, testScheme(t)))
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: resourceName + "-0", Namespace: nodeSet.Namespace,
		DeletionTimestamp: &metav1.Time{Time: time.Now()},
		Finalizers:        []string{"cosmopilot.voluzi.com/test-hold"},
	}}
	require.NoError(t, controllerutil.SetControllerReference(sts, pod, testScheme(t)))
	r := newValidatorTestReconciler(t, nodeSet, sts, pod)

	pending, err := r.reconcileCosmosignerMigrations(context.Background(), nodeSet)
	require.NoError(t, err)
	require.True(t, pending)
	require.Equal(t, appsv1.CosmosignerMigrationQuiescing, nodeSet.Status.Cosmosigners[0].Migration.Phase)
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: nodeSet.Namespace, Name: resourceName}, &k8sappsv1.StatefulSet{}))
}

func TestReconcileCosmosignerMigrationsRetargetsBeforeRecreation(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: "nodeset-uid"},
		Spec: appsv1.ChainNodeSetSpec{
			Cosmosigner: &appsv1.Cosmosigner{
				NodeGroups: []string{"sentries"},
				Backend: appsv1.CosmosignerBackend{Software: &appsv1.CosmosignerSoftwareBackend{
					PrivateKeySecret: ptr.To("sentry-key"),
				}},
			},
			Nodes: []appsv1.NodeGroupSpec{{Name: "sentries", Instances: ptr.To(1)}},
		},
		Status: appsv1.ChainNodeSetStatus{ChainID: "test-1"},
	}
	signer := resolveSingleSigner(t, nodeSet)
	nodeSet.Status.Cosmosigners = []appsv1.CosmosignerStatus{{
		Name: signer.Name, AppliedDigest: "old-digest", PublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		Migration: &appsv1.CosmosignerMigrationStatus{
			DesiredDigest: signer.Digest(), DesiredPublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
			Phase: appsv1.CosmosignerMigrationResettingState,
		},
	}}
	r := newValidatorTestReconciler(t, nodeSet)

	pending, err := r.reconcileCosmosignerMigrations(context.Background(), nodeSet)
	require.NoError(t, err)
	require.True(t, pending)
	require.Equal(t, appsv1.CosmosignerMigrationRetargeting, nodeSet.Status.Cosmosigners[0].Migration.Phase)
}

func TestReconcileCosmosignerMigrationsRollingOutPreservesNewStatefulSet(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: "nodeset-uid"},
		Spec: appsv1.ChainNodeSetSpec{
			Cosmosigner: &appsv1.Cosmosigner{
				NodeGroups: []string{"sentries"},
				Backend: appsv1.CosmosignerBackend{Software: &appsv1.CosmosignerSoftwareBackend{
					PrivateKeySecret: ptr.To("sentry-key"),
				}},
			},
			Nodes: []appsv1.NodeGroupSpec{{Name: "sentries", Instances: ptr.To(1)}},
		},
		Status: appsv1.ChainNodeSetStatus{ChainID: "test-1"},
	}
	signer := resolveSingleSigner(t, nodeSet)
	nodeSet.Status.Cosmosigners = []appsv1.CosmosignerStatus{{
		Name: signer.Name, AppliedDigest: "old-digest", PublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		Migration: &appsv1.CosmosignerMigrationStatus{
			DesiredDigest: signer.Digest(), DesiredPublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
			Phase: appsv1.CosmosignerMigrationRollingOut,
		},
	}}
	one := int32(1)
	sts := &k8sappsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: signer.Name, Namespace: nodeSet.Namespace}, Spec: k8sappsv1.StatefulSetSpec{Replicas: &one}}
	require.NoError(t, controllerutil.SetControllerReference(nodeSet, sts, testScheme(t)))
	r := newValidatorTestReconciler(t, nodeSet, sts)

	pending, err := r.reconcileCosmosignerMigrations(context.Background(), nodeSet)
	require.NoError(t, err)
	require.False(t, pending)
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: sts.Namespace, Name: sts.Name}, &k8sappsv1.StatefulSet{}))
}

func TestReconcileCosmosignerMigrationsAdvancesRecreationToRollout(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: "nodeset-uid"},
		Spec: appsv1.ChainNodeSetSpec{
			Cosmosigner: &appsv1.Cosmosigner{
				NodeGroups: []string{"sentries"},
				Backend: appsv1.CosmosignerBackend{Software: &appsv1.CosmosignerSoftwareBackend{
					PrivateKeySecret: ptr.To("sentry-key"),
				}},
			},
			Nodes: []appsv1.NodeGroupSpec{{Name: "sentries", Instances: ptr.To(1)}},
		},
		Status: appsv1.ChainNodeSetStatus{ChainID: "test-1"},
	}
	signer := resolveSingleSigner(t, nodeSet)
	nodeSet.Status.Cosmosigners = []appsv1.CosmosignerStatus{{
		Name: signer.Name, AppliedDigest: "old-digest", PublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		Migration: &appsv1.CosmosignerMigrationStatus{
			DesiredDigest: signer.Digest(), DesiredPublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
			Phase: appsv1.CosmosignerMigrationRecreating,
		},
	}}
	r := newValidatorTestReconciler(t, nodeSet)

	pending, err := r.reconcileCosmosignerMigrations(context.Background(), nodeSet)
	require.NoError(t, err)
	require.True(t, pending)
	require.Equal(t, appsv1.CosmosignerMigrationRollingOut, nodeSet.Status.Cosmosigners[0].Migration.Phase)
}

func TestReconcileCosmosignerMigrationsCoversRuntimeAndKeyDrift(t *testing.T) {
	for _, tc := range []struct {
		name          string
		desiredKey    string
		mutateDesired func(*cosmosigner.Params)
		wantReset     bool
	}{
		{name: "runtime-only change retains raft state", desiredKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", mutateDesired: func(p *cosmosigner.Params) { p.Image = "new-image" }},
		{name: "public-key drift resets raft state", desiredKey: "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB=", mutateDesired: func(*cosmosigner.Params) {}, wantReset: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			nodeSet := &appsv1.ChainNodeSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: "nodeset-uid"},
				Spec: appsv1.ChainNodeSetSpec{
					Cosmosigner: &appsv1.Cosmosigner{NodeGroups: []string{"sentries"}, Backend: appsv1.CosmosignerBackend{
						Software: &appsv1.CosmosignerSoftwareBackend{PrivateKeySecret: ptr.To("sentry-key")},
					}},
					Nodes: []appsv1.NodeGroupSpec{{Name: "sentries", Instances: ptr.To(1)}},
				},
				Status: appsv1.ChainNodeSetStatus{ChainID: "test-1"},
			}
			signer := resolveSingleSigner(t, nodeSet)
			oldParams := cosmosigner.Params{
				Name: signer.Name, Namespace: nodeSet.Namespace, ChainID: nodeSet.Status.ChainID, Image: "old-image", Replicas: 1,
				ExpectedPublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", StateStorageSize: "1Gi",
				Backend: cosmosigner.Backend{Software: &cosmosigner.SoftwareBackend{SecretName: "sentry-key"}},
			}
			oldDigest, err := oldParams.LifecycleDigest(signer.Digest())
			require.NoError(t, err)
			nodeSet.Status.Cosmosigners = []appsv1.CosmosignerStatus{{Name: signer.Name, AppliedDigest: oldDigest, PublicKey: oldParams.ExpectedPublicKey}}
			desired := oldParams
			desired.ExpectedPublicKey = tc.desiredKey
			tc.mutateDesired(&desired)
			r := newValidatorTestReconciler(t, nodeSet)

			pending, err := r.reconcileCosmosignerMigrations(context.Background(), nodeSet, map[string]cosmosigner.Params{signer.Name: desired})
			require.NoError(t, err)
			require.True(t, pending)
			require.NotNil(t, nodeSet.Status.Cosmosigners[0].Migration)
			require.Equal(t, tc.wantReset, nodeSet.Status.Cosmosigners[0].Migration.ResetState)
			require.Equal(t, appsv1.CosmosignerMigrationQuiescing, nodeSet.Status.Cosmosigners[0].Migration.Phase)
		})
	}
}

func TestReconcileCosmosignerMigrationsRequeuesAfterRecoveringLiveLifecycle(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: "nodeset-uid"},
		Spec: appsv1.ChainNodeSetSpec{
			Cosmosigner: &appsv1.Cosmosigner{NodeGroups: []string{"sentries"}, Backend: appsv1.CosmosignerBackend{
				Software: &appsv1.CosmosignerSoftwareBackend{PrivateKeySecret: ptr.To("sentry-key")},
			}},
			Nodes: []appsv1.NodeGroupSpec{{Name: "sentries", Instances: ptr.To(1)}},
		},
		Status: appsv1.ChainNodeSetStatus{ChainID: "test-1"},
	}
	signer := resolveSingleSigner(t, nodeSet)
	nodeSet.Status.Cosmosigners = []appsv1.CosmosignerStatus{{Name: signer.Name}}
	desired := cosmosigner.Params{
		Name: signer.Name, Namespace: nodeSet.Namespace, ChainID: nodeSet.Status.ChainID, Image: "new-image", Replicas: 1,
		ExpectedPublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", StateStorageSize: "1Gi",
		Backend: cosmosigner.Backend{Software: &cosmosigner.SoftwareBackend{SecretName: "sentry-key"}},
	}
	oldParams := desired
	oldParams.Image = "old-image"
	oldDigest, err := oldParams.LifecycleDigest(signer.Digest())
	require.NoError(t, err)

	one := int32(1)
	sts := &k8sappsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: signer.Name, Namespace: nodeSet.Namespace, Generation: 1,
			Annotations: map[string]string{cosmosigner.LifecycleDigestAnnotation: oldDigest},
		},
		Spec: k8sappsv1.StatefulSetSpec{Replicas: &one},
		Status: k8sappsv1.StatefulSetStatus{
			ObservedGeneration: 1, UpdatedReplicas: 1, ReadyReplicas: 1,
		},
	}
	require.NoError(t, controllerutil.SetControllerReference(nodeSet, sts, testScheme(t)))
	r := newValidatorTestReconciler(t, nodeSet, sts)

	pending, err := r.reconcileCosmosignerMigrations(context.Background(), nodeSet, map[string]cosmosigner.Params{signer.Name: desired})
	require.NoError(t, err)
	require.True(t, pending, "recovery must stop this reconcile before a changed discovery Service can expose new targets")
	require.Equal(t, oldDigest, nodeSet.GetCosmosignerStatus(signer.Name).AppliedDigest)
}

func TestReconcileCosmosignerMigrationsRecoverUnreadyLiveLifecycle(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: "nodeset-uid"},
		Spec: appsv1.ChainNodeSetSpec{
			Cosmosigner: &appsv1.Cosmosigner{NodeGroups: []string{"sentries"}, Backend: appsv1.CosmosignerBackend{
				Software: &appsv1.CosmosignerSoftwareBackend{PrivateKeySecret: ptr.To("sentry-key")},
			}},
			Nodes: []appsv1.NodeGroupSpec{{Name: "sentries", Instances: ptr.To(1)}},
		},
		Status: appsv1.ChainNodeSetStatus{ChainID: "test-1"},
	}
	signer := resolveSingleSigner(t, nodeSet)
	nodeSet.Status.Cosmosigners = []appsv1.CosmosignerStatus{{Name: signer.Name}}
	desired := cosmosigner.Params{
		Name: signer.Name, Namespace: nodeSet.Namespace, ChainID: nodeSet.Status.ChainID, Image: "fixed-image", Replicas: 1,
		ExpectedPublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", StateStorageSize: "1Gi",
		Backend: cosmosigner.Backend{Software: &cosmosigner.SoftwareBackend{SecretName: "sentry-key"}},
	}
	liveParams := desired
	liveParams.Image = "broken-image"
	liveDigest, err := liveParams.LifecycleDigest(signer.Digest())
	require.NoError(t, err)

	one := int32(1)
	sts := &k8sappsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: signer.Name, Namespace: nodeSet.Namespace, Generation: 1,
			Annotations: map[string]string{cosmosigner.LifecycleDigestAnnotation: liveDigest},
		},
		Spec:   k8sappsv1.StatefulSetSpec{Replicas: &one},
		Status: k8sappsv1.StatefulSetStatus{ObservedGeneration: 1},
	}
	require.NoError(t, controllerutil.SetControllerReference(nodeSet, sts, testScheme(t)))
	r := newValidatorTestReconciler(t, nodeSet, sts)

	pending, err := r.reconcileCosmosignerMigrations(context.Background(), nodeSet, map[string]cosmosigner.Params{signer.Name: desired})
	require.NoError(t, err)
	require.True(t, pending)
	status := nodeSet.GetCosmosignerStatus(signer.Name)
	require.Equal(t, liveDigest, status.AppliedDigest)
	require.Equal(t, desired.ExpectedPublicKey, status.PublicKey)
	require.Nil(t, status.Migration)
}

func TestPrepareCosmosignerParamsRejectsDifferentRecordedValidatorPublicKey(t *testing.T) {
	key, err := cometbft.GeneratePrivKey()
	require.NoError(t, err)
	parsed, err := cometbft.LoadPrivKey(key)
	require.NoError(t, err)
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: "nodeset-uid"},
		Spec: appsv1.ChainNodeSetSpec{
			Genesis: &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
			Nodes: []appsv1.NodeGroupSpec{{
				Name: "validators", Instances: ptr.To(1),
				Validator: &appsv1.NodeSetValidatorConfig{PrivateKeySecret: ptr.To("validator-key")},
				Cosmosigner: &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{
					Software: &appsv1.CosmosignerSoftwareBackend{},
				}},
			}},
		},
		Status: appsv1.ChainNodeSetStatus{
			ChainID: "test-1",
			Validators: []appsv1.ChainNodeSetValidatorStatus{{
				Name: "test-nodeset-validators-0", Group: "validators",
				PubKey: `{"@type":"/cosmos.crypto.ed25519.PubKey","key":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}`,
			}},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "validator-key", Namespace: nodeSet.Namespace},
		Data:       map[string][]byte{privKeyFilename: key},
	}
	r := newValidatorTestReconciler(t, nodeSet, secret)

	_, err = r.prepareCosmosignerParams(context.Background(), nodeSet)
	require.ErrorContains(t, err, "on-chain validator public key")
	reservation := &appsv1.ConsensusKeyReservation{}
	getErr := r.Get(context.Background(), client.ObjectKey{Name: cosmosigner.ConsensusKeyReservationName("test-1", parsed.PubKey.Value)}, reservation)
	require.True(t, apierrors.IsNotFound(getErr), "a rejected signer key must not leave an immutable reservation: %v", getErr)
}

func TestPrepareCosmosignerParamsAllowsMultiInstanceValidatorEndpointsWithSharedKey(t *testing.T) {
	key, err := cometbft.GeneratePrivKey()
	require.NoError(t, err)
	parsed, err := cometbft.LoadPrivKey(key)
	require.NoError(t, err)
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: "nodeset-uid"},
		Spec: appsv1.ChainNodeSetSpec{
			Genesis: &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
			Nodes: []appsv1.NodeGroupSpec{{
				Name: "validators", Instances: ptr.To(2),
				Validator: &appsv1.NodeSetValidatorConfig{PrivateKeySecret: ptr.To("validator-key")},
				Cosmosigner: &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{
					Software: &appsv1.CosmosignerSoftwareBackend{},
				}},
			}},
		},
		Status: appsv1.ChainNodeSetStatus{ChainID: "test-1"},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "validator-key", Namespace: nodeSet.Namespace},
		Data:       map[string][]byte{privKeyFilename: key},
	}
	children := make([]client.Object, 0, 2)
	for ordinal := 0; ordinal < 2; ordinal++ {
		children = append(children, &appsv1.ChainNode{
			ObjectMeta: metav1.ObjectMeta{
				Name: validatorNodeName(nodeSet, "validators", ordinal), Namespace: nodeSet.Namespace,
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNodeSet", Name: nodeSet.Name,
					UID: nodeSet.UID, Controller: ptr.To(true),
				}},
			},
			Status: appsv1.ChainNodeStatus{
				ChainID: "test-1",
				PubKey:  `{"key":"` + parsed.PubKey.Value + `"}`,
			},
		})
	}
	r := newValidatorTestReconciler(t, append([]client.Object{nodeSet, secret}, children...)...)

	_, err = r.prepareCosmosignerParams(context.Background(), nodeSet)
	require.NoError(t, err, "redundant endpoints served by one signer must share its reservation")
}

func TestPrepareCosmosignerParamsRejectsDuplicateAndReservedKeys(t *testing.T) {
	key, err := cometbft.GeneratePrivKey()
	require.NoError(t, err)
	parsed, err := cometbft.LoadPrivKey(key)
	require.NoError(t, err)
	sharedSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "shared-key", Namespace: "default"}, Data: map[string][]byte{privKeyFilename: key}}

	t.Run("duplicate within one ChainNodeSet", func(t *testing.T) {
		nodeSet := &appsv1.ChainNodeSet{
			ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: "nodeset-uid"},
			Spec: appsv1.ChainNodeSetSpec{Nodes: []appsv1.NodeGroupSpec{
				{Name: "a", Cosmosigner: &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{Software: &appsv1.CosmosignerSoftwareBackend{PrivateKeySecret: ptr.To("shared-key")}}}},
				{Name: "b", Cosmosigner: &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{Software: &appsv1.CosmosignerSoftwareBackend{PrivateKeySecret: ptr.To("shared-key")}}}},
			}},
			Status: appsv1.ChainNodeSetStatus{ChainID: "test-1"},
		}
		r := newValidatorTestReconciler(t, nodeSet, sharedSecret.DeepCopy())
		_, err := r.prepareCosmosignerParams(context.Background(), nodeSet)
		require.Error(t, err)
		require.Contains(t, err.Error(), "same consensus public key")
	})

	t.Run("reservation held by another root", func(t *testing.T) {
		nodeSet := &appsv1.ChainNodeSet{
			ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: "nodeset-uid"},
			Spec: appsv1.ChainNodeSetSpec{Nodes: []appsv1.NodeGroupSpec{{Name: "a", Cosmosigner: &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{
				Software: &appsv1.CosmosignerSoftwareBackend{PrivateKeySecret: ptr.To("shared-key")},
			}}}}},
			Status: appsv1.ChainNodeSetStatus{ChainID: "test-1"},
		}
		reservation := &appsv1.ConsensusKeyReservation{
			ObjectMeta: metav1.ObjectMeta{Name: cosmosigner.ConsensusKeyReservationName("test-1", parsed.PubKey.Value)},
			Spec:       appsv1.ConsensusKeyReservationSpec{ChainID: "test-1", PublicKey: parsed.PubKey.Value, OwnerUID: "other-uid", OwnerKind: "ChainNode", Namespace: "other", OwnerName: "validator"},
		}
		r := newValidatorTestReconciler(t, nodeSet, sharedSecret.DeepCopy(), reservation)
		_, err := r.prepareCosmosignerParams(context.Background(), nodeSet)
		require.Error(t, err)
		require.Contains(t, err.Error(), "already reserved")
	})
}

func TestReconcileCosmosignerRetargetingSkipsOnlyBlockedSigner(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: "nodeset-uid"},
		Spec: appsv1.ChainNodeSetSpec{
			App:     appsv1.AppSpec{Image: "image", App: "appd", Version: ptr.To("1.0.0")},
			Genesis: &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
			Nodes: []appsv1.NodeGroupSpec{
				{
					Name: "moving", Instances: ptr.To(1),
					Cosmosigner: &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{
						Software: &appsv1.CosmosignerSoftwareBackend{PrivateKeySecret: ptr.To("moving-key")},
					}},
				},
				{
					Name: "blocked", Instances: ptr.To(1),
					Cosmosigner: &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{
						Software: &appsv1.CosmosignerSoftwareBackend{PrivateKeySecret: ptr.To("blocked-key")},
					}},
				},
			},
		},
		Status: appsv1.ChainNodeSetStatus{ChainID: "test-1"},
	}
	var moving, blocked appsv1.ResolvedSigner
	for _, signer := range nodeSet.ResolveCosmosigners() {
		switch signer.TargetGroups[0] {
		case "moving":
			moving = signer
		case "blocked":
			blocked = signer
		}
	}
	require.NotEmpty(t, moving.Name)
	require.NotEmpty(t, blocked.Name)
	nodeSet.EnsureCosmosignerStatus(moving.Name).Migration = &appsv1.CosmosignerMigrationStatus{
		DesiredDigest: moving.Digest(), Phase: appsv1.CosmosignerMigrationRetargeting,
	}
	discovery := &corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name: nodeSet.CosmosignerResourceName(moving) + "-privval", Namespace: nodeSet.Namespace,
	}}
	require.NoError(t, controllerutil.SetControllerReference(nodeSet, discovery, testScheme(t)))
	r := newValidatorTestReconciler(t, nodeSet, discovery)

	ready, err := r.reconcileCosmosignerRetargeting(context.Background(), nodeSet, blockedSignerTargets{blocked.Name: {}})
	require.NoError(t, err)
	require.False(t, ready)
	require.Equal(t, appsv1.CosmosignerMigrationRecreating, nodeSet.GetCosmosignerStatus(moving.Name).Migration.Phase)
	require.Error(t, r.Get(context.Background(), types.NamespacedName{Namespace: discovery.Namespace, Name: discovery.Name}, &corev1.Service{}))

	movingNode := &appsv1.ChainNode{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-nodeset-moving-0"}, movingNode))
	require.True(t, movingNode.Spec.RemoteSignerTarget)
	blockedNode := &appsv1.ChainNode{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-nodeset-blocked-0"}, blockedNode))
	require.False(t, blockedNode.Spec.RemoteSignerTarget)
}

// resolveSingleSigner returns the sole resolved signer of a ChainNodeSet, failing if there is not
// exactly one.
func resolveSingleSigner(t *testing.T, nodeSet *appsv1.ChainNodeSet) appsv1.ResolvedSigner {
	t.Helper()
	signers := nodeSet.ResolveCosmosigners()
	require.Len(t, signers, 1, "expected exactly one resolved signer")
	return signers[0]
}

// TestReconcileSignerTeardownDropsStatusEntry verifies that once a removed signer's StatefulSet and
// its PVCs are gone, teardown drops its per-signer status entry, so a later re-add (e.g. a sentry
// signer with a different replica count) is not rejected against stale state on the no-webhook path.
func TestReconcileSignerTeardownDropsStatusEntry(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default"},
		Spec:       appsv1.ChainNodeSetSpec{},
		Status: appsv1.ChainNodeSetStatus{
			ChainID: "test-1",
			Cosmosigners: []appsv1.CosmosignerStatus{{
				Name:          "test-nodeset-signer",
				Replicas:      ptr.To(int32(3)),
				SigningDigest: "stale-digest",
			}},
		},
	}
	r := newValidatorTestReconciler(t, nodeSet)

	done, err := r.reconcileSignerTeardown(context.Background(), nodeSet)
	require.NoError(t, err)
	assert.True(t, done, "teardown of an absent signer is complete")

	fresh := &appsv1.ChainNodeSet{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-nodeset"}, fresh))
	assert.Empty(t, fresh.Status.Cosmosigners, "the removed signer's status entry must be dropped on teardown")
}

func TestReconcileSignerTeardownWaitsForBreakBeforeMakeMigration(t *testing.T) {
	const oldSignerName = "test-nodeset-signer"
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: types.UID("nodeset-uid")},
		Spec: appsv1.ChainNodeSetSpec{Nodes: []appsv1.NodeGroupSpec{{
			Name:      "validators",
			Instances: ptr.To(1),
			Validator: &appsv1.NodeSetValidatorConfig{PrivateKeySecret: ptr.To("validator-key")},
			Cosmosigner: &appsv1.Cosmosigner{
				Backend: appsv1.CosmosignerBackend{Software: &appsv1.CosmosignerSoftwareBackend{}},
			},
		}}},
		Status: appsv1.ChainNodeSetStatus{ChainID: "test-1"},
	}
	replacement := resolveSingleSigner(t, nodeSet)
	nodeSet.Status.Cosmosigners = []appsv1.CosmosignerStatus{
		{
			Name:             oldSignerName,
			ServingGroup:     replacement.ValidatorGroup,
			ServingIdentity:  replacement.ValidatorTargetedIdentity(),
			Replicas:         ptr.To(int32(1)),
			StateStorageSize: "1Gi",
		},
		{
			Name:             replacement.Name,
			ResourceName:     oldSignerName,
			ServingGroup:     replacement.ValidatorGroup,
			Replicas:         ptr.To(int32(1)),
			StateStorageSize: "1Gi",
			Migration: &appsv1.CosmosignerMigrationStatus{
				DesiredDigest:    replacement.Digest(),
				DesiredPublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
				Phase:            appsv1.CosmosignerMigrationQuiescing,
			},
		},
	}
	oldSigner := &k8sappsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: oldSignerName, Namespace: nodeSet.Namespace}}
	require.NoError(t, controllerutil.SetControllerReference(nodeSet, oldSigner, testScheme(t)))
	r := newValidatorTestReconciler(t, nodeSet, oldSigner)

	done, err := r.reconcileSignerTeardown(context.Background(), nodeSet)
	require.NoError(t, err)
	require.False(t, done, "replacement rollout must stay blocked until old signer teardown is verified")
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: nodeSet.Namespace, Name: oldSignerName}, &k8sappsv1.StatefulSet{}))
	require.Error(t, r.Get(context.Background(), types.NamespacedName{Namespace: nodeSet.Namespace, Name: replacement.Name}, &k8sappsv1.StatefulSet{}),
		"replacement StatefulSet must not exist before the old signer is gone")

	require.NoError(t, r.Delete(context.Background(), oldSigner))
	nodeSet.GetCosmosignerStatus(replacement.Name).Migration.Phase = appsv1.CosmosignerMigrationRecreating

	done, err = r.reconcileSignerTeardown(context.Background(), nodeSet)
	require.NoError(t, err)
	require.True(t, done)
	require.Error(t, r.Get(context.Background(), types.NamespacedName{Namespace: nodeSet.Namespace, Name: oldSignerName}, &k8sappsv1.StatefulSet{}))
	fresh := &appsv1.ChainNodeSet{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: nodeSet.Namespace, Name: nodeSet.Name}, fresh))
	require.Nil(t, fresh.GetCosmosignerStatus(oldSignerName))
	require.NotNil(t, fresh.GetCosmosignerStatus(replacement.Name))
}

func TestReconcileSignerTeardownPreservesLegacyPerInstanceSigners(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default"},
		Spec: appsv1.ChainNodeSetSpec{Nodes: []appsv1.NodeGroupSpec{{
			Name:      "validators",
			Instances: ptr.To(2),
			Validator: &appsv1.NodeSetValidatorConfig{},
			Cosmosigner: &appsv1.Cosmosigner{
				Backend: appsv1.CosmosignerBackend{Software: &appsv1.CosmosignerSoftwareBackend{}},
			},
		}}},
		Status: appsv1.ChainNodeSetStatus{
			ChainID: "test-1",
			Cosmosigners: []appsv1.CosmosignerStatus{
				{Name: "test-nodeset-validators-0-signer"},
				{Name: "test-nodeset-validators-1-signer"},
			},
		},
	}
	r := newValidatorTestReconciler(t, nodeSet)

	done, err := r.reconcileSignerTeardown(context.Background(), nodeSet)
	require.Error(t, err)
	assert.False(t, done)
	assert.Contains(t, err.Error(), "legacy per-instance cosmosigners")

	fresh := &appsv1.ChainNodeSet{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-nodeset"}, fresh))
	require.Len(t, fresh.Status.Cosmosigners, 2)
}

func TestReconcileSignerTeardownDetectsLegacyPerInstanceSignersAfterSpecRemoval(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default"},
		Spec: appsv1.ChainNodeSetSpec{Nodes: []appsv1.NodeGroupSpec{{
			Name: "validators", Instances: ptr.To(2), Validator: &appsv1.NodeSetValidatorConfig{},
		}}},
		Status: appsv1.ChainNodeSetStatus{
			ChainID: "test-1",
			Cosmosigners: []appsv1.CosmosignerStatus{
				{Name: "test-nodeset-validators-0-signer"},
				{Name: "test-nodeset-validators-1-signer"},
			},
		},
	}
	r := newValidatorTestReconciler(t, nodeSet)

	done, err := r.reconcileSignerTeardown(context.Background(), nodeSet)
	require.Error(t, err)
	require.False(t, done)
	require.Contains(t, err.Error(), "legacy per-instance cosmosigners")

	fresh := &appsv1.ChainNodeSet{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: nodeSet.Namespace, Name: nodeSet.Name}, fresh))
	require.Len(t, fresh.Status.Cosmosigners, 2)
}

func TestPreflightCosmosignersRejectsRecoveredIdentityMismatch(t *testing.T) {
	requireRecoveredNodeSetIdentityMismatchRejected(t, false)
}

func TestPreflightCosmosignersRejectsRecoveredIdentityMismatchAfterValidatorDemotion(t *testing.T) {
	requireRecoveredNodeSetIdentityMismatchRejected(t, true)
}

func TestPreflightCosmosignersRejectsRecoveredGenesisSentryIdentityMismatch(t *testing.T) {
	const (
		genesisKey = "genesis-sentry-key"
		desiredKey = "rotated-sentry-key"
	)
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: types.UID("nodeset-uid")},
		Spec: appsv1.ChainNodeSetSpec{
			Validator: &appsv1.NodeSetValidatorConfig{Init: &appsv1.GenesisInitConfig{
				GenesisValidators: []appsv1.GenesisValidator{{PrivKeySecret: genesisKey}},
			}},
			Nodes: []appsv1.NodeGroupSpec{{
				Name: "sentries",
				Cosmosigner: &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{
					Software: &appsv1.CosmosignerSoftwareBackend{PrivateKeySecret: ptr.To(desiredKey)},
				}},
			}},
		},
		Status: appsv1.ChainNodeSetStatus{ChainID: "test-1"},
	}
	signer := resolveSingleSigner(t, nodeSet)

	key, err := cometbft.GeneratePrivKey()
	require.NoError(t, err)
	desiredSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: desiredKey, Namespace: nodeSet.Namespace},
		Data:       map[string][]byte{privKeyFilename: key},
	}
	r := newValidatorTestReconciler(t, nodeSet, desiredSecret)
	desiredParams, err := r.cosmosignerParams(context.Background(), nodeSet, signer)
	require.NoError(t, err)
	liveParams := desiredParams
	liveParams.ExpectedPublicKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	liveParams.Backend.Software = &cosmosigner.SoftwareBackend{SecretName: genesisKey}
	liveConfig, err := liveParams.ConfigYAML()
	require.NoError(t, err)
	liveConfigMap, err := liveParams.ConfigMap(liveConfig)
	require.NoError(t, err)
	liveStatefulSet, err := liveParams.StatefulSet(liveConfig)
	require.NoError(t, err)
	require.NoError(t, controllerutil.SetControllerReference(nodeSet, liveConfigMap, r.Scheme))
	require.NoError(t, controllerutil.SetControllerReference(nodeSet, liveStatefulSet, r.Scheme))
	require.NoError(t, r.Create(context.Background(), liveConfigMap))
	require.NoError(t, r.Create(context.Background(), liveStatefulSet))
	recorded, err := r.initCosmosignerLocks(context.Background(), nodeSet)
	require.NoError(t, err)
	require.True(t, recorded)
	status := nodeSet.GetCosmosignerStatus(signer.Name)
	require.NotNil(t, status)
	require.NotNil(t, status.AtEstablishment)
	require.Empty(t, *status.AtEstablishment, "the rotated desired key cannot be backfilled as the genesis identity")

	err = r.preflightCosmosigners(context.Background(), nodeSet)
	require.Error(t, err)
	require.Contains(t, err.Error(), "live signing identity")
}

func TestPreflightCosmosignersRejectsRecoveredSentryIdentityMismatch(t *testing.T) {
	backend := cosmosignerVaultBackend()
	backend.Vault.KeyName = "desired-key"
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: types.UID("nodeset-uid")},
		Spec: appsv1.ChainNodeSetSpec{
			Cosmosigner: &appsv1.Cosmosigner{NodeGroups: []string{"sentries"}, Backend: backend},
			Nodes:       []appsv1.NodeGroupSpec{{Name: "sentries", Instances: ptr.To(1)}},
		},
		Status: appsv1.ChainNodeSetStatus{ChainID: "test-1"},
	}
	signer := resolveSingleSigner(t, nodeSet)
	status := nodeSet.EnsureCosmosignerStatus(signer.Name)
	status.AtEstablishment = ptr.To("")
	status.Replicas = ptr.To(signer.Spec.GetReplicas())
	status.StateStorageSize = signer.Spec.GetStateStorageSize()
	token := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: backend.Vault.TokenSecret.Name, Namespace: nodeSet.Namespace},
		Data:       map[string][]byte{backend.Vault.TokenSecret.Key: []byte("token")},
	}
	r := newValidatorTestReconciler(t, nodeSet, token)
	desiredParams, err := r.cosmosignerParams(context.Background(), nodeSet, signer)
	require.NoError(t, err)
	liveParams := desiredParams
	liveParams.ExpectedPublicKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	liveVault := *desiredParams.Backend.Vault
	liveVault.KeyName = "live-key"
	liveParams.Backend.Vault = &liveVault
	liveConfig, err := liveParams.ConfigYAML()
	require.NoError(t, err)
	liveConfigMap, err := liveParams.ConfigMap(liveConfig)
	require.NoError(t, err)
	liveStatefulSet, err := liveParams.StatefulSet(liveConfig)
	require.NoError(t, err)
	require.NoError(t, controllerutil.SetControllerReference(nodeSet, liveConfigMap, r.Scheme))
	require.NoError(t, controllerutil.SetControllerReference(nodeSet, liveStatefulSet, r.Scheme))
	require.NoError(t, r.Create(context.Background(), liveConfigMap))
	require.NoError(t, r.Create(context.Background(), liveStatefulSet))

	err = r.preflightCosmosigners(context.Background(), nodeSet)
	require.Error(t, err)
	require.Contains(t, err.Error(), "live signing identity")
}

func TestPreflightCosmosignersRejectsLiveIdentityMismatchWithLostStatus(t *testing.T) {
	backend := cosmosignerVaultBackend()
	backend.Vault.KeyName = "desired-key"
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: types.UID("nodeset-uid")},
		Spec: appsv1.ChainNodeSetSpec{
			Cosmosigner: &appsv1.Cosmosigner{NodeGroups: []string{"sentries"}, Backend: backend},
			Nodes:       []appsv1.NodeGroupSpec{{Name: "sentries", Instances: ptr.To(1)}},
		},
		Status: appsv1.ChainNodeSetStatus{ChainID: "test-1"},
	}
	signer := resolveSingleSigner(t, nodeSet)
	token := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: backend.Vault.TokenSecret.Name, Namespace: nodeSet.Namespace},
		Data:       map[string][]byte{backend.Vault.TokenSecret.Key: []byte("token")},
	}
	r := newValidatorTestReconciler(t, nodeSet, token)
	desiredParams, err := r.cosmosignerParams(context.Background(), nodeSet, signer)
	require.NoError(t, err)
	liveParams := desiredParams
	liveParams.ExpectedPublicKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	liveVault := *desiredParams.Backend.Vault
	liveVault.KeyName = "live-key"
	liveParams.Backend.Vault = &liveVault
	liveConfig, err := liveParams.ConfigYAML()
	require.NoError(t, err)
	liveConfigMap, err := liveParams.ConfigMap(liveConfig)
	require.NoError(t, err)
	liveStatefulSet, err := liveParams.StatefulSet(liveConfig)
	require.NoError(t, err)
	require.NoError(t, controllerutil.SetControllerReference(nodeSet, liveConfigMap, r.Scheme))
	require.NoError(t, controllerutil.SetControllerReference(nodeSet, liveStatefulSet, r.Scheme))
	require.NoError(t, r.Create(context.Background(), liveConfigMap))
	require.NoError(t, r.Create(context.Background(), liveStatefulSet))

	err = r.preflightCosmosigners(context.Background(), nodeSet)
	require.Error(t, err)
	require.Contains(t, err.Error(), "live signing identity")
}

func TestPreflightCosmosignersRejectsUntrackedLiveSigner(t *testing.T) {
	key, err := cometbft.GeneratePrivKey()
	require.NoError(t, err)
	parsed, err := cometbft.LoadPrivKey(key)
	require.NoError(t, err)
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: types.UID("nodeset-uid")},
		Spec: appsv1.ChainNodeSetSpec{
			Cosmosigner: &appsv1.Cosmosigner{
				NodeGroups: []string{"sentries"},
				Backend: appsv1.CosmosignerBackend{Software: &appsv1.CosmosignerSoftwareBackend{
					PrivateKeySecret: ptr.To("desired-key"),
				}},
			},
			Nodes: []appsv1.NodeGroupSpec{{Name: "sentries", Instances: ptr.To(1)}},
		},
		Status: appsv1.ChainNodeSetStatus{ChainID: "test-1"},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "desired-key", Namespace: nodeSet.Namespace},
		Data:       map[string][]byte{privKeyFilename: key},
	}
	r := newValidatorTestReconciler(t, nodeSet, secret)
	staleParams := cosmosigner.Params{
		Name: "lost-signer", Namespace: nodeSet.Namespace, Replicas: 1,
		ExpectedPublicKey: parsed.PubKey.Value, StateStorageSize: "1Gi",
		Backend: cosmosigner.Backend{Software: &cosmosigner.SoftwareBackend{SecretName: secret.Name}},
	}
	staleConfig, err := staleParams.ConfigYAML()
	require.NoError(t, err)
	stale, err := staleParams.StatefulSet(staleConfig)
	require.NoError(t, err)
	require.NoError(t, controllerutil.SetControllerReference(nodeSet, stale, r.Scheme))
	require.NoError(t, r.Create(context.Background(), stale))

	err = r.preflightCosmosigners(context.Background(), nodeSet)
	require.Error(t, err)
	require.Contains(t, err.Error(), "no status entry")
}

func requireRecoveredNodeSetIdentityMismatchRejected(t *testing.T, demoted bool) {
	t.Helper()
	backend := cosmosignerVaultBackend()
	backend.Vault.KeyName = "desired-key"
	nodeSet := cosmosignerValidatorNodeSet(backend)
	nodeSet.UID = types.UID("nodeset-uid")
	establishedSigner := resolveSingleSigner(t, nodeSet)
	status := nodeSet.EnsureCosmosignerStatus(establishedSigner.Name)
	status.Replicas = ptr.To(establishedSigner.Spec.GetReplicas())
	status.StateStorageSize = establishedSigner.Spec.GetStateStorageSize()
	if demoted {
		status.ServingGroup = establishedSigner.ValidatorGroup
		nodeSet.Spec.Nodes[0].Validator = nil
	}
	signer := resolveSingleSigner(t, nodeSet)
	token := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: backend.Vault.TokenSecret.Name, Namespace: nodeSet.Namespace},
		Data:       map[string][]byte{backend.Vault.TokenSecret.Key: []byte("token")},
	}
	r := newValidatorTestReconciler(t, nodeSet, token)
	desiredParams, err := r.cosmosignerParams(context.Background(), nodeSet, signer)
	require.NoError(t, err)
	liveParams := desiredParams
	liveParams.ExpectedPublicKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	liveVault := *desiredParams.Backend.Vault
	liveVault.KeyName = "live-key"
	liveParams.Backend.Vault = &liveVault
	liveConfig, err := liveParams.ConfigYAML()
	require.NoError(t, err)
	liveConfigMap, err := liveParams.ConfigMap(liveConfig)
	require.NoError(t, err)
	liveStatefulSet, err := liveParams.StatefulSet(liveConfig)
	require.NoError(t, err)
	require.NoError(t, controllerutil.SetControllerReference(nodeSet, liveConfigMap, r.Scheme))
	require.NoError(t, controllerutil.SetControllerReference(nodeSet, liveStatefulSet, r.Scheme))
	require.NoError(t, r.Create(context.Background(), liveConfigMap))
	require.NoError(t, r.Create(context.Background(), liveStatefulSet))

	err = r.preflightCosmosigners(context.Background(), nodeSet)
	require.Error(t, err)
	require.Contains(t, err.Error(), "live signing identity")

	fresh := &corev1.ConfigMap{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: nodeSet.Namespace, Name: signer.Name}, fresh))
	require.Equal(t, liveConfig, fresh.Data["config.yaml"])
}

func TestReconcilePreflightsReplacementBeforeSignerTeardown(t *testing.T) {
	const staleSigner = "test-nodeset-signer"
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: "nodeset-uid"},
		Spec: appsv1.ChainNodeSetSpec{
			App:     appsv1.AppSpec{Image: "image", App: "appd", Version: ptr.To("1.0.0")},
			Genesis: &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
			Nodes: []appsv1.NodeGroupSpec{{
				Name:      "validators",
				Instances: ptr.To(1),
				Validator: &appsv1.NodeSetValidatorConfig{PrivateKeySecret: ptr.To("missing-key")},
				Cosmosigner: &appsv1.Cosmosigner{
					Backend: appsv1.CosmosignerBackend{Software: &appsv1.CosmosignerSoftwareBackend{}},
				},
			}},
		},
		Status: appsv1.ChainNodeSetStatus{
			ChainID: "test-1",
			Cosmosigners: []appsv1.CosmosignerStatus{{
				Name:             staleSigner,
				Replicas:         ptr.To(int32(1)),
				StateStorageSize: "1Gi",
			}},
		},
	}
	stale := &k8sappsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
		Name: staleSigner, Namespace: "default", Labels: cosmosigner.InstanceLabels(staleSigner),
	}}
	require.NoError(t, controllerutil.SetControllerReference(nodeSet, stale, testScheme(t)))
	r := newValidatorTestReconciler(t, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}, nodeSet, stale)

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "test-nodeset"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing-key")

	remaining := &k8sappsv1.StatefulSet{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: staleSigner}, remaining))
}

func TestInitCosmosignerLocksRecordsPreRolloutTargetKind(t *testing.T) {
	t.Run("validator target", func(t *testing.T) {
		nodeSet := &appsv1.ChainNodeSet{
			ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default"},
			Spec: appsv1.ChainNodeSetSpec{Nodes: []appsv1.NodeGroupSpec{{
				Name:      "validators",
				Instances: ptr.To(1),
				Validator: &appsv1.NodeSetValidatorConfig{},
				Cosmosigner: &appsv1.Cosmosigner{
					Backend: appsv1.CosmosignerBackend{Software: &appsv1.CosmosignerSoftwareBackend{}},
				},
			}}},
			Status: appsv1.ChainNodeSetStatus{ChainID: "test-1"},
		}
		r := newValidatorTestReconciler(t, nodeSet)
		changed, err := r.initCosmosignerLocks(context.Background(), nodeSet)
		require.NoError(t, err)
		assert.True(t, changed)

		fresh := &appsv1.ChainNodeSet{}
		require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-nodeset"}, fresh))
		require.Len(t, fresh.Status.Cosmosigners, 1)
		assert.Equal(t, "validators", fresh.Status.Cosmosigners[0].ServingGroup)
		require.NotNil(t, fresh.Status.Cosmosigners[0].LocalKeyEverServed)
		assert.True(t, *fresh.Status.Cosmosigners[0].LocalKeyEverServed)
	})

	t.Run("migration to a local key records monotonic history before rollout", func(t *testing.T) {
		nodeSet := cosmosignerValidatorNodeSet(cosmosignerVaultBackend())
		require.NotNil(t, nodeSet.Status.Cosmosigners[0].LocalKeyEverServed)
		require.False(t, *nodeSet.Status.Cosmosigners[0].LocalKeyEverServed)
		nodeSet.Spec.Cosmosigner.Backend = appsv1.CosmosignerBackend{Software: &appsv1.CosmosignerSoftwareBackend{}}

		r := newValidatorTestReconciler(t, nodeSet)
		changed, err := r.initCosmosignerLocks(context.Background(), nodeSet)
		require.NoError(t, err)
		assert.True(t, changed)

		fresh := &appsv1.ChainNodeSet{}
		require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-nodeset"}, fresh))
		require.NotNil(t, fresh.Status.Cosmosigners[0].LocalKeyEverServed)
		assert.True(t, *fresh.Status.Cosmosigners[0].LocalKeyEverServed)
	})

	t.Run("sentry target", func(t *testing.T) {
		nodeSet := &appsv1.ChainNodeSet{
			ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default"},
			Spec: appsv1.ChainNodeSetSpec{Nodes: []appsv1.NodeGroupSpec{{
				Name:      "sentries",
				Instances: ptr.To(1),
				Cosmosigner: &appsv1.Cosmosigner{
					Backend: appsv1.CosmosignerBackend{Software: &appsv1.CosmosignerSoftwareBackend{PrivateKeySecret: ptr.To("sentry-key")}},
				},
			}}},
			Status: appsv1.ChainNodeSetStatus{ChainID: "test-1"},
		}
		r := newValidatorTestReconciler(t, nodeSet)
		changed, err := r.initCosmosignerLocks(context.Background(), nodeSet)
		require.NoError(t, err)
		assert.True(t, changed)

		fresh := &appsv1.ChainNodeSet{}
		require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-nodeset"}, fresh))
		require.Len(t, fresh.Status.Cosmosigners, 1)
		require.NotNil(t, fresh.Status.Cosmosigners[0].AtEstablishment)
		assert.Empty(t, *fresh.Status.Cosmosigners[0].AtEstablishment)
	})
}

func TestPreflightCosmosignersRequiresGenesisSentrySecrets(t *testing.T) {
	const (
		privSecret    = "genesis-sentry-key"
		accountSecret = "genesis-sentry-account"
	)
	newNodeSet := func() *appsv1.ChainNodeSet {
		return &appsv1.ChainNodeSet{
			ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: "nodeset-uid"},
			Spec: appsv1.ChainNodeSetSpec{
				Validator: &appsv1.NodeSetValidatorConfig{Init: &appsv1.GenesisInitConfig{
					ChainID:     "test-1",
					Assets:      []string{"1stake"},
					StakeAmount: "1stake",
					GenesisValidators: []appsv1.GenesisValidator{{
						PrivKeySecret:         privSecret,
						AccountMnemonicSecret: accountSecret,
						Moniker:               "sentry",
						Assets:                []string{"1stake"},
						StakeAmount:           "1stake",
					}},
				}},
				Nodes: []appsv1.NodeGroupSpec{{
					Name:      "sentries",
					Instances: ptr.To(1),
					Cosmosigner: &appsv1.Cosmosigner{
						Backend: appsv1.CosmosignerBackend{Software: &appsv1.CosmosignerSoftwareBackend{PrivateKeySecret: ptr.To(privSecret)}},
					},
				}},
			},
		}
	}

	t.Run("missing private key", func(t *testing.T) {
		nodeSet := newNodeSet()
		r := newValidatorTestReconciler(t, nodeSet)
		err := r.preflightCosmosigners(context.Background(), nodeSet)
		require.Error(t, err)
		assert.Contains(t, err.Error(), privSecret)
	})

	t.Run("missing account mnemonic", func(t *testing.T) {
		nodeSet := newNodeSet()
		key := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: privSecret, Namespace: "default"},
			Data:       map[string][]byte{privKeyFilename: []byte("key")},
		}
		r := newValidatorTestReconciler(t, nodeSet, key)
		err := r.preflightCosmosigners(context.Background(), nodeSet)
		require.Error(t, err)
		assert.Contains(t, err.Error(), accountSecret)
	})
}

func TestCosmosignerBackendRejectsMalformedSoftwareKey(t *testing.T) {
	nodeSet := cosmosignerValidatorNodeSet(appsv1.CosmosignerBackend{Software: &appsv1.CosmosignerSoftwareBackend{}})
	nodeSet.UID = "nodeset-uid"
	nodeSet.Spec.Nodes[0].Validator.CreateValidator = &appsv1.CreateValidatorConfig{}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "val-priv-key", Namespace: "default"},
		Data: map[string][]byte{privKeyFilename: []byte(`{
			"address":"0000000000000000000000000000000000000000",
			"pub_key":{"type":"tendermint/PubKeyEd25519","value":"eA=="},
			"priv_key":{"type":"tendermint/PrivKeyEd25519","value":"eA=="}
		}`)},
	}
	r := newValidatorTestReconciler(t, nodeSet, secret)

	_, err := r.cosmosignerBackend(context.Background(), nodeSet, resolveSingleSigner(t, nodeSet))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid")
}

// TestReconcileSignerTeardownKeepsStatusWhileTerminating verifies that while the signer StatefulSet is
// still present (teardown is asynchronous), the recorded status entry is preserved — dropping it early
// would let a remove-and-immediate-re-add bind the surviving PVCs and inherit stale raft membership.
func TestReconcileSignerTeardownKeepsStatusWhileTerminating(t *testing.T) {
	const signerName = "test-nodeset-signer"
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: "nodeset-uid"},
		Spec:       appsv1.ChainNodeSetSpec{},
		Status: appsv1.ChainNodeSetStatus{
			ChainID: "test-1",
			Cosmosigners: []appsv1.CosmosignerStatus{{
				Name:          signerName,
				Replicas:      ptr.To(int32(3)),
				SigningDigest: "stale-digest",
			}},
		},
	}
	// A StatefulSet owned by the nodeSet with a finalizer: Undeploy issues a delete, but the fake
	// client retains it (deletionTimestamp set, object kept until finalizers clear), modelling the
	// window where teardown is still in flight.
	sts := &k8sappsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:       signerName,
			Namespace:  "default",
			Finalizers: []string{"cosmopilot.voluzi.com/test-hold"},
			Labels:     cosmosigner.InstanceLabels(signerName),
		},
	}
	require.NoError(t, controllerutil.SetControllerReference(nodeSet, sts, testScheme(t)))

	r := newValidatorTestReconciler(t, nodeSet, sts)

	done, err := r.reconcileSignerTeardown(context.Background(), nodeSet)
	require.NoError(t, err)
	assert.False(t, done, "teardown is not complete while the StatefulSet is still terminating")

	fresh := &appsv1.ChainNodeSet{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-nodeset"}, fresh))
	require.Len(t, fresh.Status.Cosmosigners, 1, "the status entry must be preserved while the signer is still terminating")
	assert.Equal(t, ptr.To(int32(3)), fresh.Status.Cosmosigners[0].Replicas)
	assert.Equal(t, "stale-digest", fresh.Status.Cosmosigners[0].SigningDigest)
}

// TestReconcileSignerTeardownDropsStatusWithForeignSameNameSigner verifies that a same-name
// StatefulSet owned by ANOTHER CR does not permanently block dropping this nodeSet's recorded status
// entry: Undeploy skips the foreign resource, and IsTornDown treats it as unrelated, so the stale
// entry is dropped and a later valid re-add is not rejected against it.
func TestReconcileSignerTeardownDropsStatusWithForeignSameNameSigner(t *testing.T) {
	const signerName = "test-nodeset-signer"
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: "nodeset-uid"},
		Spec:       appsv1.ChainNodeSetSpec{},
		Status: appsv1.ChainNodeSetStatus{
			ChainID: "test-1",
			Cosmosigners: []appsv1.CosmosignerStatus{{
				Name:          signerName,
				Replicas:      ptr.To(int32(3)),
				SigningDigest: "stale-digest",
			}},
		},
	}
	// A same-name StatefulSet owned by a DIFFERENT ChainNodeSet (distinct UID).
	foreignOwner := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{Name: "other-nodeset", Namespace: "default", UID: "other-uid"}}
	sts := &k8sappsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      signerName,
			Namespace: "default",
			Labels:    cosmosigner.InstanceLabels(signerName),
		},
	}
	require.NoError(t, controllerutil.SetControllerReference(foreignOwner, sts, testScheme(t)))

	r := newValidatorTestReconciler(t, nodeSet, sts)

	done, err := r.reconcileSignerTeardown(context.Background(), nodeSet)
	require.NoError(t, err)
	assert.True(t, done, "a foreign same-name signer must not block completion")

	fresh := &appsv1.ChainNodeSet{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-nodeset"}, fresh))
	assert.Empty(t, fresh.Status.Cosmosigners, "a foreign same-name signer must not block dropping our status entry")

	// The foreign StatefulSet must be left untouched.
	remaining := &k8sappsv1.StatefulSet{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: signerName}, remaining))
	assert.True(t, metav1.IsControlledBy(remaining, foreignOwner), "foreign signer must remain owned by the other CR")
}

func TestPreflightRemovedSignerFallbacksRequiresLocalKey(t *testing.T) {
	nodeSet := cosmosignerValidatorNodeSet(appsv1.CosmosignerBackend{Software: &appsv1.CosmosignerSoftwareBackend{}})
	nodeSet.UID = types.UID("nodeset-uid")
	recordSignerRollout(t, nodeSet)
	nodeSet.Spec.Cosmosigner = nil
	r := newValidatorTestReconciler(t, nodeSet)
	sts := &k8sappsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset-signer", Namespace: "default"}}
	require.NoError(t, controllerutil.SetControllerReference(nodeSet, sts, r.Scheme))
	require.NoError(t, r.Create(context.Background(), sts))

	err := r.preflightRemovedSignerFallbacks(context.Background(), nodeSet)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "val-priv-key")

	remaining := &k8sappsv1.StatefulSet{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: sts.Name}, remaining))

	keySecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "val-priv-key", Namespace: "default"},
		Data:       map[string][]byte{privKeyFilename: []byte("{}")},
	}
	require.NoError(t, r.Create(context.Background(), keySecret))
	err = r.preflightRemovedSignerFallbacks(context.Background(), nodeSet)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid")

	key, err := cometbft.GeneratePrivKey()
	require.NoError(t, err)
	parsed, err := cometbft.LoadPrivKey(key)
	require.NoError(t, err)
	nodeSet.Status.Cosmosigners[0].PublicKey = parsed.PubKey.Value
	keySecret.Data[privKeyFilename] = key
	require.NoError(t, r.Update(context.Background(), keySecret))
	require.NoError(t, r.preflightRemovedSignerFallbacks(context.Background(), nodeSet))
}

func TestPreflightRemovedSignerFallbacksUsesDesiredReplacement(t *testing.T) {
	nodeSet := cosmosignerValidatorNodeSet(cosmosignerVaultBackend())
	recordSignerRollout(t, nodeSet)
	nodeSet.Spec.Cosmosigner = nil
	nodeSet.Spec.Nodes[0].Cosmosigner = &appsv1.Cosmosigner{Backend: cosmosignerVaultBackend()}
	r := newValidatorTestReconciler(t, nodeSet)

	require.NoError(t, r.preflightRemovedSignerFallbacks(context.Background(), nodeSet))
}

func TestPreflightRemovedSignerFallbacksProtectsGenesisSentry(t *testing.T) {
	nodeSet := genesisSentryNodeSet("genesis-sentry-key", "genesis-sentry-key")
	recordSignerRollout(t, nodeSet)
	nodeSet.Spec.Nodes[0].Cosmosigner = nil
	r := newValidatorTestReconciler(t, nodeSet)

	err := r.preflightRemovedSignerFallbacks(context.Background(), nodeSet)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "on-chain consensus key")
}

func TestDesiredReplacementSignerMatchesGenesisSentryIdentity(t *testing.T) {
	const keySecret = "genesis-sentry-key"
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default"},
		Spec: appsv1.ChainNodeSetSpec{
			Validator: &appsv1.NodeSetValidatorConfig{Init: &appsv1.GenesisInitConfig{
				GenesisValidators: []appsv1.GenesisValidator{{PrivKeySecret: keySecret}},
			}},
			Nodes: []appsv1.NodeGroupSpec{{
				Name: "replacement-sentries",
				Cosmosigner: &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{
					Software: &appsv1.CosmosignerSoftwareBackend{PrivateKeySecret: ptr.To(keySecret)},
				}},
			}},
		},
	}
	desired := nodeSet.ResolveCosmosigners()
	require.Len(t, desired, 1)
	status := &appsv1.CosmosignerStatus{
		Name:            "test-nodeset-old-sentries-signer",
		AtEstablishment: ptr.To(desired[0].Identity()),
	}

	replacement, ok := desiredReplacementSigner(nodeSet, desired, status)
	require.True(t, ok)
	require.Equal(t, desired[0].Name, replacement.Name)

	nodeSet.Spec.Nodes[0].Cosmosigner.Backend.Software.PrivateKeySecret = ptr.To("different-key")
	desired = nodeSet.ResolveCosmosigners()
	_, ok = desiredReplacementSigner(nodeSet, desired, status)
	require.False(t, ok, "a different sentry key must not be treated as a replacement")
}

func TestDesiredReplacementSignerMatchesSentryTargetGroups(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default"},
		Spec: appsv1.ChainNodeSetSpec{
			Nodes: []appsv1.NodeGroupSpec{{
				Name: "sentries",
				Cosmosigner: &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{
					Software: &appsv1.CosmosignerSoftwareBackend{PrivateKeySecret: ptr.To("sentry-key")},
				}},
			}},
		},
	}
	desired := nodeSet.ResolveCosmosigners()
	require.Len(t, desired, 1)
	status := &appsv1.CosmosignerStatus{
		Name:         "test-nodeset-signer",
		TargetGroups: []string{"sentries"},
	}

	replacement, ok := desiredReplacementSigner(nodeSet, desired, status)
	require.True(t, ok)
	require.Equal(t, desired[0].Name, replacement.Name)

	status.TargetGroups = []string{"archive"}
	_, ok = desiredReplacementSigner(nodeSet, desired, status)
	require.False(t, ok, "a signer targeting a different group must not inherit the old resource identity")
}

func TestInitCosmosignerReplacementNamesCarriesSentryLifecycleState(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default"},
		Spec: appsv1.ChainNodeSetSpec{
			Nodes: []appsv1.NodeGroupSpec{{
				Name: "sentries",
				Cosmosigner: &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{
					Software: &appsv1.CosmosignerSoftwareBackend{PrivateKeySecret: ptr.To("sentry-key")},
				}},
			}},
		},
		Status: appsv1.ChainNodeSetStatus{Cosmosigners: []appsv1.CosmosignerStatus{{
			Name:             "test-nodeset-signer",
			AppliedDigest:    "old-digest",
			PublicKey:        "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
			TargetGroups:     []string{"sentries"},
			Replicas:         ptr.To(int32(1)),
			StateStorageSize: "1Gi",
		}}},
	}
	desired := nodeSet.ResolveCosmosigners()
	require.Len(t, desired, 1)
	r := newValidatorTestReconciler(t, nodeSet)

	recorded, err := r.initCosmosignerReplacementNames(context.Background(), nodeSet)
	require.NoError(t, err)
	require.True(t, recorded)

	status := nodeSet.GetCosmosignerStatus(desired[0].Name)
	require.NotNil(t, status)
	require.Equal(t, "test-nodeset-signer", status.ResourceName)
	require.Equal(t, "old-digest", status.AppliedDigest)
	require.Equal(t, []string{"sentries"}, status.TargetGroups)
}

func TestInitCosmosignerReplacementNamesAlwaysStartsPlacementMigration(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default"},
		Spec: appsv1.ChainNodeSetSpec{
			Nodes: []appsv1.NodeGroupSpec{{
				Name: "sentries",
				Cosmosigner: &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{
					Software: &appsv1.CosmosignerSoftwareBackend{PrivateKeySecret: ptr.To("sentry-key")},
				}},
			}},
		},
	}
	replacement := resolveSingleSigner(t, nodeSet)
	nodeSet.Status.Cosmosigners = []appsv1.CosmosignerStatus{{
		Name:             "test-nodeset-signer",
		AppliedDigest:    "applied-lifecycle-digest",
		PublicKey:        "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		TargetGroups:     []string{"sentries"},
		Replicas:         ptr.To(int32(1)),
		StateStorageSize: "1Gi",
	}}
	r := newValidatorTestReconciler(t, nodeSet)

	recorded, err := r.initCosmosignerReplacementNames(context.Background(), nodeSet)
	require.NoError(t, err)
	require.True(t, recorded)

	status := nodeSet.GetCosmosignerStatus(replacement.Name)
	require.NotNil(t, status)
	require.NotNil(t, status.Migration)
	require.Equal(t, replacement.Digest(), status.Migration.DesiredDigest)
	require.Equal(t, appsv1.CosmosignerMigrationQuiescing, status.Migration.Phase)
}

func TestInitCosmosignerReplacementNamesCarriesValidatorKeyHistory(t *testing.T) {
	nodeSet := cosmosignerValidatorNodeSet(cosmosignerVaultBackend())
	recordSignerRollout(t, nodeSet)
	oldStatus := nodeSet.Status.Cosmosigners[0].DeepCopy()
	backend := nodeSet.Spec.Cosmosigner.Backend
	nodeSet.Spec.Cosmosigner = nil
	nodeSet.Spec.Nodes[0].Cosmosigner = &appsv1.Cosmosigner{Backend: backend}
	replacement := resolveSingleSigner(t, nodeSet)
	r := newValidatorTestReconciler(t, nodeSet)

	recorded, err := r.initCosmosignerReplacementNames(context.Background(), nodeSet)
	require.NoError(t, err)
	require.True(t, recorded)

	status := nodeSet.GetCosmosignerStatus(replacement.Name)
	require.NotNil(t, status)
	require.NotNil(t, status.LocalKeyEverServed)
	assert.False(t, *status.LocalKeyEverServed)
	assert.Equal(t, oldStatus.AtEstablishment, status.AtEstablishment)
	assert.Equal(t, oldStatus.ServingGroup, status.ServingGroup)
	assert.Equal(t, oldStatus.ServingIdentity, status.ServingIdentity)
}

func TestPreflightRemovedSignerFallbacksSkipsUncreatedSigner(t *testing.T) {
	nodeSet := cosmosignerValidatorNodeSet(appsv1.CosmosignerBackend{Software: &appsv1.CosmosignerSoftwareBackend{}})
	signer := resolveSingleSigner(t, nodeSet)
	status := nodeSet.EnsureCosmosignerStatus(signer.Name)
	status.ServingGroup = signer.ValidatorGroup
	status.Replicas = ptr.To(int32(1))
	nodeSet.Spec.Cosmosigner = nil
	r := newValidatorTestReconciler(t, nodeSet)

	require.NoError(t, r.preflightRemovedSignerFallbacks(context.Background(), nodeSet))
}

func TestPreflightRemovedSignerFallbacksRejectsLegacyDigestWithoutServingGroup(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default"},
		Spec: appsv1.ChainNodeSetSpec{Nodes: []appsv1.NodeGroupSpec{{
			Name: "validators", Validator: &appsv1.NodeSetValidatorConfig{},
		}},
		},
		Status: appsv1.ChainNodeSetStatus{Cosmosigners: []appsv1.CosmosignerStatus{{
			Name: "test-nodeset-signer", SigningDigest: "legacy-served-digest",
		}}},
	}
	r := newValidatorTestReconciler(t, nodeSet)

	err := r.preflightRemovedSignerFallbacks(context.Background(), nodeSet)
	require.Error(t, err)
	require.Contains(t, err.Error(), "served validator group")
}

func TestPreflightRemovedSignerFallbacksRequiresMatchingLocalPublicKey(t *testing.T) {
	servedKey, err := cometbft.GeneratePrivKey()
	require.NoError(t, err)
	served, err := cometbft.LoadPrivKey(servedKey)
	require.NoError(t, err)
	fallbackKey, err := cometbft.GeneratePrivKey()
	require.NoError(t, err)

	nodeSet := cosmosignerValidatorNodeSet(appsv1.CosmosignerBackend{Software: &appsv1.CosmosignerSoftwareBackend{}})
	recordSignerRollout(t, nodeSet)
	nodeSet.Status.Cosmosigners[0].PublicKey = served.PubKey.Value
	nodeSet.Spec.Cosmosigner = nil
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "val-priv-key", Namespace: "default"},
		Data:       map[string][]byte{privKeyFilename: fallbackKey},
	}
	r := newValidatorTestReconciler(t, nodeSet, secret)

	err = r.preflightRemovedSignerFallbacks(context.Background(), nodeSet)
	require.Error(t, err)
	require.Contains(t, err.Error(), "does not match")

	secret.Data[privKeyFilename] = servedKey
	require.NoError(t, r.Update(context.Background(), secret))
	require.NoError(t, r.preflightRemovedSignerFallbacks(context.Background(), nodeSet))
}

func TestPreflightRemovedSignerFallbacksDoesNotTrustDifferentTmKMSTarget(t *testing.T) {
	nodeSet := cosmosignerValidatorNodeSet(cosmosignerVaultBackend())
	recordSignerRollout(t, nodeSet)
	nodeSet.Spec.Cosmosigner = nil
	nodeSet.Spec.Nodes[0].Config = &appsv1.Config{ServiceAccountName: ptr.To("group-service-account")}
	nodeSet.Spec.Nodes[0].Validator.Config = &appsv1.Config{ServiceAccountName: ptr.To("validator-service-account")}
	nodeSet.Spec.Nodes[0].Validator.TmKMS = &appsv1.TmKMS{Provider: appsv1.TmKmsProvider{
		Hashicorp: &appsv1.TmKmsHashicorpProvider{
			Address: "https://vault.example:8200",
			Key:     "different-key",
			TokenSecret: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "tmkms-token"},
				Key:                  "token",
			},
		},
	}}
	token := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "tmkms-token", Namespace: "default"},
		Data:       map[string][]byte{"token": []byte("vault-token")},
	}
	r := newValidatorTestReconciler(t, nodeSet, token)
	var createdPod string
	transport := chainNodeSetRoundTripperFunc(func(req *http.Request) (*http.Response, error) {
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
	r.ClientSet = clientSet

	err = r.preflightRemovedSignerFallbacks(context.Background(), nodeSet)
	require.Error(t, err)
	require.Contains(t, createdPod, "validator-service-account")
	require.NotContains(t, createdPod, "group-service-account")
	require.Contains(t, createdPod, `"--vault-key-version","1"`)
}

func TestCosmosignerPublicKeyUsesVaultAfterImportedSourceRemoval(t *testing.T) {
	vault := &appsv1.CosmosignerVaultBackend{
		Address: "https://vault.example:8200", KeyName: "validator-key", UploadGenerated: true,
		TokenSecret: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "vault-token"}, Key: "token"},
	}
	nodeSet := cosmosignerValidatorNodeSet(appsv1.CosmosignerBackend{Vault: vault})
	signer := resolveSingleSigner(t, nodeSet)
	status := nodeSet.EnsureCosmosignerStatus(signer.Name)
	status.KeyImported = vault.ImportFingerprint(signer.SoftwareKeySecret, []byte("imported-key"))
	token := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "vault-token", Namespace: "default"},
		Data:       map[string][]byte{"token": []byte("vault-token")},
	}
	r := newValidatorTestReconciler(t, nodeSet, token)

	_, err := r.cosmosignerPublicKey(context.Background(), nodeSet, signer)
	require.Error(t, err)
	require.Contains(t, err.Error(), "Kubernetes clientset")

	status.KeyImported = ""
	_, err = r.cosmosignerPublicKey(context.Background(), nodeSet, signer)
	require.Error(t, err)
	require.Contains(t, err.Error(), signer.SoftwareKeySecret)
}

func TestReconcilePreflightsRemovedSignerFallbackBeforeTeardown(t *testing.T) {
	nodeSet := cosmosignerValidatorNodeSet(appsv1.CosmosignerBackend{Software: &appsv1.CosmosignerSoftwareBackend{}})
	nodeSet.UID = types.UID("nodeset-uid")
	nodeSet.Spec.App = appsv1.AppSpec{Image: "image", App: "appd", Version: ptr.To("1.0.0")}
	recordSignerRollout(t, nodeSet)
	signerName := nodeSet.Status.Cosmosigners[0].Name
	nodeSet.Spec.Cosmosigner = nil
	sts := &k8sappsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
		Name: signerName, Namespace: "default", Labels: cosmosigner.InstanceLabels(signerName),
	}}
	require.NoError(t, controllerutil.SetControllerReference(nodeSet, sts, testScheme(t)))
	r := newValidatorTestReconciler(t, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}, nodeSet, sts)

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: nodeSet.Name}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "val-priv-key")

	remaining := &k8sappsv1.StatefulSet{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: signerName}, remaining))
}

func TestPreflightRemovedSignerFallbacksRequiresTmKMSSecrets(t *testing.T) {
	nodeSet := cosmosignerValidatorNodeSet(cosmosignerVaultBackend())
	nodeSet.UID = types.UID("nodeset-uid")
	recordSignerRollout(t, nodeSet)
	nodeSet.Spec.Cosmosigner = nil
	nodeSet.Spec.Nodes[0].Validator.TmKMS = &appsv1.TmKMS{Provider: appsv1.TmKmsProvider{
		Hashicorp: &appsv1.TmKmsHashicorpProvider{
			Address: "https://vault.example:8200",
			Key:     "val-key",
			TokenSecret: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "tmkms-token"},
				Key:                  "token",
			},
			CertificateSecret: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "tmkms-ca"},
				Key:                  "ca.crt",
			},
		},
	}}
	r := newValidatorTestReconciler(t, nodeSet)
	sts := &k8sappsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset-signer", Namespace: "default"}}
	require.NoError(t, controllerutil.SetControllerReference(nodeSet, sts, r.Scheme))
	require.NoError(t, r.Create(context.Background(), sts))

	err := r.preflightRemovedSignerFallbacks(context.Background(), nodeSet)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tmKMS Vault token")

	remaining := &k8sappsv1.StatefulSet{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: sts.Name}, remaining))

	require.NoError(t, r.Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "tmkms-token", Namespace: "default"},
		Data:       map[string][]byte{"token": []byte("vault-token")},
	}))
	err = r.preflightRemovedSignerFallbacks(context.Background(), nodeSet)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tmKMS Vault certificate")

	require.NoError(t, r.Create(context.Background(), &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "tmkms-ca", Namespace: "default"},
		Data:       map[string][]byte{"ca.crt": []byte("certificate")},
	}))
	require.NoError(t, r.preflightRemovedSignerFallbacks(context.Background(), nodeSet),
		"the same normalized Vault key needs no pubkey lookup after its Secrets are present")
}

func TestPreflightRemovedSignerFallbacksRequiresSupportedTmKMSProvider(t *testing.T) {
	nodeSet := cosmosignerValidatorNodeSet(cosmosignerVaultBackend())
	recordSignerRollout(t, nodeSet)
	nodeSet.Spec.Cosmosigner = nil
	nodeSet.Spec.Nodes[0].Validator.TmKMS = &appsv1.TmKMS{}
	r := newValidatorTestReconciler(t, nodeSet)

	err := r.preflightRemovedSignerFallbacks(context.Background(), nodeSet)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "supported tmKMS provider")
}

func TestPreflightRemovedSignerFallbacksRequiresTmKMSTarget(t *testing.T) {
	nodeSet := cosmosignerValidatorNodeSet(cosmosignerVaultBackend())
	recordSignerRollout(t, nodeSet)
	nodeSet.Spec.Cosmosigner = nil
	nodeSet.Spec.Nodes[0].Validator.TmKMS = &appsv1.TmKMS{Provider: appsv1.TmKmsProvider{
		Hashicorp: &appsv1.TmKmsHashicorpProvider{
			TokenSecret: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "tmkms-token"},
				Key:                  "token",
			},
		},
	}}
	r := newValidatorTestReconciler(t, nodeSet)

	err := r.preflightRemovedSignerFallbacks(context.Background(), nodeSet)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "address and key")
}

// TestSignerNameForNode verifies each node maps to the signer that must dial it: every pod of a
// signer's target groups is a signing endpoint — the (single-instance) validator group it serves and
// any sentry groups fronted alongside it.
func TestSignerNameForNode(t *testing.T) {
	// Top-level signer fronting a single-instance validator group AND a multi-instance sentry group.
	topLevel := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec: appsv1.ChainNodeSetSpec{
			Cosmosigner: &appsv1.Cosmosigner{NodeGroups: []string{"vg", "fullnodes"}, Backend: appsv1.CosmosignerBackend{Vault: &appsv1.CosmosignerVaultBackend{Address: "https://v:8200", KeyName: "k", TokenSecret: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "t"}, Key: "token"}}}},
			Nodes: []appsv1.NodeGroupSpec{
				{Name: "vg", Instances: ptr.To(1), Validator: &appsv1.NodeSetValidatorConfig{PrivateKeySecret: ptr.To("vgk")}},
				{Name: "fullnodes", Instances: ptr.To(3)},
			},
		},
	}
	name, ok := signerNameForNode(topLevel, "vg")
	assert.True(t, ok)
	assert.Equal(t, "cs-signer", name)
	// The sentry fan-out group is a signing endpoint too.
	name, ok = signerNameForNode(topLevel, "fullnodes")
	assert.True(t, ok, "fullnodes must be a signing endpoint")
	assert.Equal(t, "cs-signer", name)

	// An untargeted group maps to no signer.
	_, ok = signerNameForNode(topLevel, "other")
	assert.False(t, ok)

	// A single-instance validator group with its own per-group signer maps to that signer.
	perGroup := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec: appsv1.ChainNodeSetSpec{
			Nodes: []appsv1.NodeGroupSpec{{
				Name: "vg", Instances: ptr.To(1), Validator: &appsv1.NodeSetValidatorConfig{},
				Cosmosigner: &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{Vault: &appsv1.CosmosignerVaultBackend{Address: "https://v:8200", KeyName: "k", TokenSecret: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "t"}, Key: "token"}}}},
			}},
		},
	}
	name, ok = signerNameForNode(perGroup, "vg")
	assert.True(t, ok)
	assert.Equal(t, "cs-vg-signer", name)
}

// testScheme builds a scheme with the API + core + apps types for owner references in tests.
func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, discoveryv1.AddToScheme(scheme))
	require.NoError(t, k8sappsv1.AddToScheme(scheme))
	return scheme
}

// TestMaybeImportCosmosignerKeyPreservesCompletedImport verifies the absent-source fast-path: a
// recorded import (in the signer's CosmosignerStatus.KeyImported) whose TARGET half matches the
// current Vault destination and source secret keeps a completed import valid when the bootstrap Secret
// is deleted (the signer keeps running — Vault still holds the registered key). A record from a
// DIFFERENT target/source, or none at all, keeps the import pending: nothing usable was ever imported
// for the current spec.
func TestMaybeImportCosmosignerKeyPreservesCompletedImport(t *testing.T) {
	const signerName = "test-nodeset-signer"
	mk := func(imported string) *appsv1.ChainNodeSet {
		ns := &appsv1.ChainNodeSet{
			ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default"},
			Spec: appsv1.ChainNodeSetSpec{
				Genesis: &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
				Cosmosigner: &appsv1.Cosmosigner{
					NodeGroups: []string{"validators"},
					Backend: appsv1.CosmosignerBackend{Vault: &appsv1.CosmosignerVaultBackend{
						Address:         "https://vault.example:8200",
						KeyName:         "val-key",
						TokenSecret:     &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "vault-token"}, Key: "token"},
						UploadGenerated: true,
					}},
				},
				Nodes: []appsv1.NodeGroupSpec{{
					Name:      "validators",
					Instances: ptr.To(1),
					Validator: &appsv1.NodeSetValidatorConfig{PrivateKeySecret: ptr.To("val-priv-key")},
				}},
			},
			Status: appsv1.ChainNodeSetStatus{ChainID: "test-localnet"},
		}
		if imported != "" {
			ns.Status.Cosmosigners = []appsv1.CosmosignerStatus{{Name: signerName, KeyImported: imported}}
		}
		return ns
	}
	params := cosmosigner.Params{Name: signerName, Namespace: "default"}
	// The value a completed import would have recorded for the CURRENT target/source (key material hash
	// differs from target hash, but only the target half matters when the source is gone).
	matching := mk("").Spec.Cosmosigner.Backend.Vault.ImportFingerprint("val-priv-key", []byte("imported-key-bytes"))

	// Source Secret absent but a prior import for the CURRENT target/source recorded: NOT pending.
	ns := mk(matching)
	r := newValidatorTestReconciler(t, ns)
	pending, _, err := r.maybeImportCosmosignerKey(context.Background(), ns, resolveSingleSigner(t, ns), params)
	require.NoError(t, err)
	assert.False(t, pending, "a completed import must survive deletion of the bootstrap source Secret")

	// Source Secret absent and the recorded import belongs to a DIFFERENT Vault target: error — this
	// validator uses an explicit external-genesis privateKeySecret, so no controller flow will create it
	// later. Keeping the signer merely pending would leave target children in remote-signer mode forever.
	otherTarget := mk("")
	otherTarget.Spec.Cosmosigner.Backend.Vault.KeyName = "old-key"
	stale := otherTarget.Spec.Cosmosigner.Backend.Vault.ImportFingerprint("val-priv-key", []byte("imported-key-bytes"))
	ns = mk(stale)
	r = newValidatorTestReconciler(t, ns)
	pending, _, err = r.maybeImportCosmosignerKey(context.Background(), ns, resolveSingleSigner(t, ns), params)
	require.Error(t, err)
	assert.False(t, pending)

	// Source Secret absent and nothing imported yet: explicit external-genesis key is missing -> error.
	ns = mk("")
	r = newValidatorTestReconciler(t, ns)
	pending, _, err = r.maybeImportCosmosignerKey(context.Background(), ns, resolveSingleSigner(t, ns), params)
	require.Error(t, err)
	assert.False(t, pending)

	// Generated init/createValidator key flow still pending (no status pubkey yet, no explicit secret):
	// wait instead of erroring because ensureValidator will create the source key.
	ns = mk("")
	ns.Spec.Nodes[0].Validator.PrivateKeySecret = nil
	ns.Spec.Nodes[0].Validator.Init = &appsv1.GenesisInitConfig{ChainID: "test-localnet", Assets: []string{"100stake"}, StakeAmount: "1stake"}
	r = newValidatorTestReconciler(t, ns)
	pending, _, err = r.maybeImportCosmosignerKey(context.Background(), ns, resolveSingleSigner(t, ns), params)
	require.NoError(t, err)
	assert.True(t, pending, "generated key flow with no recorded pubkey may still produce the source key")
}

func TestMaybeImportCosmosignerKeyUpgradesLegacyFingerprintWithoutScaleDown(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: "nodeset-uid"},
		Spec: appsv1.ChainNodeSetSpec{
			Genesis: &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
			Cosmosigner: &appsv1.Cosmosigner{
				NodeGroups: []string{"validators"},
				Backend: appsv1.CosmosignerBackend{Vault: &appsv1.CosmosignerVaultBackend{
					Address: "https://vault.example:8200", KeyName: "val-key", UploadGenerated: true,
				}},
			},
			Nodes: []appsv1.NodeGroupSpec{{
				Name: "validators", Instances: ptr.To(1),
				Validator: &appsv1.NodeSetValidatorConfig{PrivateKeySecret: ptr.To("val-priv-key")},
			}},
		},
		Status: appsv1.ChainNodeSetStatus{ChainID: "test-localnet"},
	}
	signer := resolveSingleSigner(t, nodeSet)
	vault := signer.Spec.Backend.Vault
	key, err := cometbft.GeneratePrivKey()
	require.NoError(t, err)
	nodeSet.Status.Cosmosigners = []appsv1.CosmosignerStatus{{
		Name: signer.Name, ResourceName: nodeSet.CosmosignerResourceName(signer),
		KeyImported: vault.LegacyImportFingerprint(signer.SoftwareKeySecret, key),
	}}
	source := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: signer.SoftwareKeySecret, Namespace: nodeSet.Namespace},
		Data:       map[string][]byte{privKeyFilename: key},
	}
	sts := &k8sappsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: nodeSet.CosmosignerResourceName(signer), Namespace: nodeSet.Namespace},
		Spec:       k8sappsv1.StatefulSetSpec{Replicas: ptr.To(int32(1))},
	}
	require.NoError(t, controllerutil.SetControllerReference(nodeSet, sts, testScheme(t)))
	r := newValidatorTestReconciler(t, nodeSet, source, sts)

	pending, changed, err := r.maybeImportCosmosignerKey(context.Background(), nodeSet, signer, cosmosigner.Params{Name: sts.Name})
	require.NoError(t, err)
	require.False(t, pending)
	require.False(t, changed)
	require.Equal(t, vault.ImportFingerprint(signer.SoftwareKeySecret, key), nodeSet.GetCosmosignerStatus(signer.Name).KeyImported)

	fresh := &k8sappsv1.StatefulSet{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(sts), fresh))
	require.Equal(t, int32(1), ptr.Deref(fresh.Spec.Replicas, 0), "format-only status upgrade must not quiesce the live signer")
}

func TestMaybeImportCosmosignerKeyRejectsInPlaceSourceRotationBeforeScaleDown(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: "nodeset-uid"},
		Spec: appsv1.ChainNodeSetSpec{
			Genesis: &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
			Cosmosigner: &appsv1.Cosmosigner{
				NodeGroups: []string{"validators"},
				Backend: appsv1.CosmosignerBackend{Vault: &appsv1.CosmosignerVaultBackend{
					Address: "https://vault.example:8200", KeyName: "val-key", UploadGenerated: true,
				}},
			},
			Nodes: []appsv1.NodeGroupSpec{{
				Name: "validators", Instances: ptr.To(1),
				Validator: &appsv1.NodeSetValidatorConfig{PrivateKeySecret: ptr.To("val-priv-key")},
			}},
		},
		Status: appsv1.ChainNodeSetStatus{ChainID: "test-localnet"},
	}
	signer := resolveSingleSigner(t, nodeSet)
	vault := signer.Spec.Backend.Vault
	oldKey, err := cometbft.GeneratePrivKey()
	require.NoError(t, err)
	newKey, err := cometbft.GeneratePrivKey()
	require.NoError(t, err)
	nodeSet.Status.Cosmosigners = []appsv1.CosmosignerStatus{{
		Name: signer.Name, ResourceName: nodeSet.CosmosignerResourceName(signer),
		KeyImported: vault.ImportFingerprint(signer.SoftwareKeySecret, oldKey),
	}}
	source := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: signer.SoftwareKeySecret, Namespace: nodeSet.Namespace},
		Data:       map[string][]byte{privKeyFilename: newKey},
	}
	sts := &k8sappsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: nodeSet.CosmosignerResourceName(signer), Namespace: nodeSet.Namespace},
		Spec:       k8sappsv1.StatefulSetSpec{Replicas: ptr.To(int32(1))},
	}
	require.NoError(t, controllerutil.SetControllerReference(nodeSet, sts, testScheme(t)))
	r := newValidatorTestReconciler(t, nodeSet, source, sts)

	pending, _, err := r.maybeImportCosmosignerKey(context.Background(), nodeSet, signer, cosmosigner.Params{Name: sts.Name})
	require.ErrorContains(t, err, "new Vault keyName")
	require.False(t, pending)

	fresh := &k8sappsv1.StatefulSet{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(sts), fresh))
	require.Equal(t, int32(1), ptr.Deref(fresh.Spec.Replicas, 0), "an unsupported in-place rekey must leave the serving signer intact")
}

// TestInitCosmosignerReplacementNamesLeavesMissingReplicaLockForLiveRecovery verifies that a
// placement move copying an old status entry with no replica lock (partial restore or pre-lock
// record) does NOT default the lock to the replacement spec: the lock stays missing so
// initCosmosignerLocks recovers it from the live StatefulSet, keeping a 3-replica raft cluster
// from being re-locked to a 1-replica spec.
func TestInitCosmosignerReplacementNamesLeavesMissingReplicaLockForLiveRecovery(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: "nodeset-uid"},
		Spec: appsv1.ChainNodeSetSpec{
			Nodes: []appsv1.NodeGroupSpec{{
				Name: "sentries",
				Cosmosigner: &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{
					Software: &appsv1.CosmosignerSoftwareBackend{PrivateKeySecret: ptr.To("sentry-key")},
				}},
			}},
		},
		Status: appsv1.ChainNodeSetStatus{ChainID: "test-1"},
	}
	replacement := resolveSingleSigner(t, nodeSet)
	nodeSet.Status.Cosmosigners = []appsv1.CosmosignerStatus{{
		Name:             "test-nodeset-signer",
		AppliedDigest:    "old-digest",
		PublicKey:        "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		TargetGroups:     []string{"sentries"},
		StateStorageSize: "1Gi",
		// Replicas deliberately unset: a partial restore / pre-lock record.
	}}
	live := &k8sappsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset-signer", Namespace: "default"},
		Spec:       k8sappsv1.StatefulSetSpec{Replicas: ptr.To(int32(3))},
	}
	require.NoError(t, controllerutil.SetControllerReference(nodeSet, live, testScheme(t)))
	r := newValidatorTestReconciler(t, nodeSet, live)

	recorded, err := r.initCosmosignerReplacementNames(context.Background(), nodeSet)
	require.NoError(t, err)
	require.True(t, recorded)

	status := nodeSet.GetCosmosignerStatus(replacement.Name)
	require.NotNil(t, status)
	assert.Nil(t, status.Replicas, "a missing old replica lock must not default to the replacement spec")

	// The next pass recovers the lock from the live 3-replica StatefulSet, not the 1-replica spec.
	changed, err := r.initCosmosignerLocks(context.Background(), nodeSet)
	require.NoError(t, err)
	assert.True(t, changed)
	fresh := &appsv1.ChainNodeSet{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-nodeset"}, fresh))
	require.NotNil(t, fresh.GetCosmosignerStatus(replacement.Name).Replicas)
	assert.Equal(t, int32(3), *fresh.GetCosmosignerStatus(replacement.Name).Replicas)
}

// TestReconcileSignerTeardownFailsClosedOnOrphanedLiveSigner verifies that an owned signer
// StatefulSet backed by no status entry and no desired signer (status lost or restored incomplete)
// blocks reconciliation instead of being silently ignored while children change signing paths.
func TestReconcileSignerTeardownFailsClosedOnOrphanedLiveSigner(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: "nodeset-uid"},
		Spec:       appsv1.ChainNodeSetSpec{},
		Status:     appsv1.ChainNodeSetStatus{ChainID: "test-1"},
	}
	orphan := &k8sappsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset-signer", Namespace: "default"},
		Spec: k8sappsv1.StatefulSetSpec{Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: cosmosigner.InstanceLabels("test-nodeset-signer")},
		}},
	}
	require.NoError(t, controllerutil.SetControllerReference(nodeSet, orphan, testScheme(t)))
	foreign := &k8sappsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "other-signer", Namespace: "default", UID: "other-uid"},
		Spec: k8sappsv1.StatefulSetSpec{Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: cosmosigner.InstanceLabels("other-signer")},
		}},
	}
	r := newValidatorTestReconciler(t, nodeSet, orphan, foreign)

	done, err := r.reconcileSignerTeardown(context.Background(), nodeSet)
	require.Error(t, err)
	assert.False(t, done)
	assert.Contains(t, err.Error(), "no status entry")
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-nodeset-signer"}, &k8sappsv1.StatefulSet{}),
		"the orphaned signer must be left in place for the operator to resolve")

	// Once the operator deletes the orphan (or restores its status), teardown proceeds.
	require.NoError(t, r.Delete(context.Background(), orphan))
	done, err = r.reconcileSignerTeardown(context.Background(), nodeSet)
	require.NoError(t, err)
	assert.True(t, done)
}

// TestPreflightCosmosignersRejectsRecoveredSignerServingUntargetedGroup verifies that a signer
// recovered from live state (recorded digests lost) cannot be adopted under a spec whose target
// set no longer covers the pods it still serves: the live pods' target label is the last applied
// truth, and the removed group's fallback guards would otherwise never run.
func TestPreflightCosmosignersRejectsRecoveredSignerServingUntargetedGroup(t *testing.T) {
	keyBytes, err := cometbft.GeneratePrivKey()
	require.NoError(t, err)
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: "nodeset-uid"},
		Spec: appsv1.ChainNodeSetSpec{
			Nodes: []appsv1.NodeGroupSpec{{
				Name: "sentries-b",
				Cosmosigner: &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{
					Software: &appsv1.CosmosignerSoftwareBackend{PrivateKeySecret: ptr.To("sentry-key")},
				}},
			}},
		},
		Status: appsv1.ChainNodeSetStatus{
			ChainID: "test-1",
			Cosmosigners: []appsv1.CosmosignerStatus{{
				Name:             "test-nodeset-sentries-b-signer",
				Replicas:         ptr.To(int32(1)),
				StateStorageSize: "1Gi",
			}},
		},
	}
	keySecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "sentry-key", Namespace: "default"},
		Data:       map[string][]byte{privKeyFilename: keyBytes},
	}
	servedPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "test-nodeset-sentries-a-0", Namespace: "default",
		Labels: map[string]string{
			controllers.LabelChainNodeSet:      "test-nodeset",
			controllers.LabelChainNodeSetGroup: "sentries-a",
			controllers.LabelCosmosignerTarget: "test-nodeset-sentries-b-signer",
		},
	}}
	r := newValidatorTestReconciler(t, nodeSet, keySecret, servedPod)

	err = r.preflightCosmosigners(context.Background(), nodeSet)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"sentries-a"`)
	assert.Contains(t, err.Error(), "no longer targets")

	// A spec restored with the original target set adopts the live signer.
	matchingPod := servedPod.DeepCopy()
	matchingPod.Labels[controllers.LabelChainNodeSetGroup] = "sentries-b"
	r = newValidatorTestReconciler(t, nodeSet, keySecret, matchingPod)
	require.NoError(t, r.preflightCosmosigners(context.Background(), nodeSet))
}
