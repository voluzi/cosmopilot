package chainnode

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	k8sappsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	appsv1 "github.com/voluzi/cosmopilot/v5/api/v1"
	"github.com/voluzi/cosmopilot/v5/internal/controllers"
	"github.com/voluzi/cosmopilot/v5/internal/cosmosigner"
)

func TestPKCS11CosmosignerSuppliedPublicKey(t *testing.T) {
	const publicKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	node := signerChainNode(appsv1.CosmosignerBackend{PKCS11: &appsv1.CosmosignerPKCS11Backend{
		Module: "/vendor/lib.so", Slot: ptr.To(int64(0)), KeyLabel: "validator", PublicKey: publicKey,
		PINSecret: *claimSelector("pin"),
	}})
	node.Spec.Cosmosigner.Image = ptr.To("ghcr.io/voluzi/cosmosigner:edge-pkcs11")
	r := claimTestReconciler(t)
	r.opts = &controllers.ControllerRunOptions{}
	params, err := r.cosmosignerParams(context.Background(), node)
	require.NoError(t, err)
	require.NotNil(t, params.Backend.PKCS11)
	require.Nil(t, params.Backend.Software)
	require.Equal(t, ptr.To(int64(0)), params.Backend.PKCS11.Slot)
	for i := 0; i < 3; i++ {
		key, err := r.cosmosignerPublicKey(context.Background(), node, params)
		require.NoError(t, err)
		require.Equal(t, publicKey, key)
	}
	pending, err := r.maybeImportCosmosignerKey(context.Background(), node, params)
	require.NoError(t, err)
	require.False(t, pending)
	pods := &corev1.PodList{}
	require.NoError(t, r.List(context.Background(), pods))
	require.Empty(t, pods.Items)
}

func TestPreflightCosmosignerRejectsDifferentRecordedValidatorPublicKeyPKCS11(t *testing.T) {
	const desiredKey = "AQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "validator", Namespace: "default", UID: "validator-uid"},
		Spec: appsv1.ChainNodeSpec{
			Validator:   &appsv1.ValidatorConfig{PrivateKeySecret: ptr.To("validator-key")},
			Cosmosigner: &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{PKCS11: &appsv1.CosmosignerPKCS11Backend{Module: "/vendor/lib.so", TokenLabel: "validator", KeyLabel: "consensus", PublicKey: desiredKey, PINSecret: *claimSelector("pin")}}},
		},
		Status: appsv1.ChainNodeStatus{
			ChainID: "test-1",
			PubKey:  `{"@type":"/cosmos.crypto.ed25519.PubKey","key":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="}`,
		},
	}
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, k8sappsv1.AddToScheme(scheme))
	require.NoError(t, policyv1.AddToScheme(scheme))
	r := &Reconciler{
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(chainNode).Build(),
		Scheme: scheme, opts: &controllers.ControllerRunOptions{},
	}

	_, err := r.preflightCosmosigner(context.Background(), chainNode)
	require.ErrorContains(t, err, "on-chain validator public key")
	require.NotEqual(t, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", desiredKey)
	reservation := &appsv1.ConsensusKeyReservation{}
	getErr := r.Get(context.Background(), client.ObjectKey{Name: cosmosigner.ConsensusKeyReservationName("test-1", desiredKey)}, reservation)
	require.True(t, apierrors.IsNotFound(getErr), "a rejected signer key must not leave an immutable reservation: %v", getErr)
}
