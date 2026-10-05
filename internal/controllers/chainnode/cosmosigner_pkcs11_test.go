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
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

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
	r := claimTestReconciler(t, claimSecret("pin"))
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
		Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(chainNode, claimSecret("pin")).Build(),
		Scheme: scheme, opts: &controllers.ControllerRunOptions{},
	}

	_, err := r.preflightCosmosigner(context.Background(), chainNode)
	require.ErrorContains(t, err, "on-chain validator public key")
	reservation := &appsv1.ConsensusKeyReservation{}
	getErr := r.Get(context.Background(), client.ObjectKey{Name: cosmosigner.ConsensusKeyReservationName("test-1", desiredKey)}, reservation)
	require.True(t, apierrors.IsNotFound(getErr), "a rejected signer key must not leave an immutable reservation: %v", getErr)
}

func pkcs11TestNode(t *testing.T, validator bool) (*Reconciler, *appsv1.ChainNode) {
	t.Helper()
	node := signerChainNode(appsv1.CosmosignerBackend{PKCS11: &appsv1.CosmosignerPKCS11Backend{Module: "/vendor/lib.so", TokenLabel: "validator", KeyLabel: "consensus", PublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", PINSecret: *claimSelector("pin")}})
	node.UID = "node-uid"
	if validator {
		node.Spec.Validator = &appsv1.ValidatorConfig{}
	}
	scheme := runtime.NewScheme()
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(node).WithObjects(node, claimSecret("pin")).Build()
	return &Reconciler{Client: cl, APIReader: cl, Scheme: scheme, opts: &controllers.ControllerRunOptions{}}, node
}

func TestPKCS11SentrySpecAddressMigrationRetainsState(t *testing.T) {
	for name, change := range map[string]func(*appsv1.CosmosignerPKCS11Backend){
		"module":   func(p *appsv1.CosmosignerPKCS11Backend) { p.Module = "/other.so" },
		"selector": func(p *appsv1.CosmosignerPKCS11Backend) { p.TokenLabel = ""; p.Slot = ptr.To(int64(0)) },
		"key ID":   func(p *appsv1.CosmosignerPKCS11Backend) { p.KeyID = "01" },
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			r, node := pkcs11TestNode(t, false)
			old, err := r.preflightCosmosigner(ctx, node)
			require.NoError(t, err)
			claim := standaloneCosmosignerReservationClaim(node)
			createPKCS11LiveSigner(t, r, node, old)
			digest, err := old.LifecycleDigest(node.CosmosignerSigningDigest())
			require.NoError(t, err)
			node.Status.CosmosignerAppliedDigest = digest
			node.Status.CosmosignerPublicKey = old.ExpectedPublicKey
			node.Status.CosmosignerReplicas = ptr.To(int32(1))
			require.NoError(t, r.Status().Update(ctx, node))
			change(node.Spec.Cosmosigner.Backend.PKCS11)
			require.NoError(t, r.Update(ctx, node))
			prepared, err := r.preflightCosmosigner(ctx, node)
			require.NoError(t, err)
			reservations := &appsv1.ConsensusKeyReservationList{}
			require.NoError(t, r.List(ctx, reservations))
			require.Len(t, reservations.Items, 1)
			require.Equal(t, claim, reservations.Items[0].Spec.Claim)
			pending, err := r.reconcileCosmosignerMigration(ctx, node, prepared)
			require.NoError(t, err)
			require.True(t, pending)
			require.False(t, node.Status.CosmosignerMigration.ResetState)
		})
	}
}

func createPKCS11LiveSigner(t *testing.T, r *Reconciler, node *appsv1.ChainNode, p cosmosigner.Params) *k8sappsv1.StatefulSet {
	t.Helper()
	_, err := r.initCosmosignerLocks(context.Background(), node)
	require.NoError(t, err)
	config, err := p.ConfigYAML()
	require.NoError(t, err)
	cm, err := p.ConfigMap(config)
	require.NoError(t, err)
	sts, err := p.StatefulSet(config)
	require.NoError(t, err)
	sts.UID = "signer-uid"
	for _, obj := range []client.Object{cm, sts} {
		require.NoError(t, controllerutil.SetControllerReference(node, obj, r.Scheme))
		require.NoError(t, r.Create(context.Background(), obj))
	}
	pvc := sts.Spec.VolumeClaimTemplates[0].DeepCopy()
	pvc.Name = "data-" + sts.Name + "-0"
	pvc.Namespace = node.Namespace
	pvc.Spec.VolumeName = "signer-state"
	pvc.Status.Phase = corev1.ClaimBound
	require.NoError(t, r.Create(context.Background(), pvc))
	return sts
}

func TestPKCS11InitialPublicKeyCorrection(t *testing.T) {
	for _, tc := range []struct {
		name                               string
		validator, recorded, everRolledOut bool
	}{
		{name: "validator", validator: true}, {name: "sentry"},
		{name: "recorded validator", validator: true, recorded: true},
		{name: "lost validator status", validator: true, everRolledOut: true},
		{name: "lost sentry status", everRolledOut: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			r, node := pkcs11TestNode(t, tc.validator)
			old, err := r.preflightCosmosigner(ctx, node)
			require.NoError(t, err)
			sts := createPKCS11LiveSigner(t, r, node, old)
			if tc.everRolledOut {
				sts.Annotations = map[string]string{cosmosigner.EverRolledOutAnnotation: "true"}
				require.NoError(t, r.Update(ctx, sts))
			}
			oldReservation := &appsv1.ConsensusKeyReservation{}
			require.NoError(t, r.Get(ctx, client.ObjectKey{Name: cosmosigner.ConsensusKeyReservationName(node.Status.ChainID, old.ExpectedPublicKey)}, oldReservation))
			oldReservation.UID = "reservation-uid"
			require.NoError(t, r.Update(ctx, oldReservation))
			if tc.recorded {
				node.Status.CosmosignerAppliedDigest = "applied"
				node.Status.CosmosignerPublicKey = old.ExpectedPublicKey
				node.Status.CosmosignerReplicas = ptr.To(int32(1))
				require.NoError(t, r.Status().Update(ctx, node))
			}
			const correctedKey = "AQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
			node.Spec.Cosmosigner.Backend.PKCS11.PublicKey = correctedKey
			require.NoError(t, r.Update(ctx, node))
			var prepared cosmosigner.Params
			for i := 0; i < 3; i++ {
				prepared, err = r.preflightCosmosigner(ctx, node)
				if err == nil && prepared.ExpectedPublicKey == correctedKey {
					break
				}
			}
			if tc.recorded || tc.everRolledOut {
				require.Error(t, err)
				require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(oldReservation), &appsv1.ConsensusKeyReservation{}))
				return
			}
			require.NoError(t, err)
			require.Equal(t, correctedKey, prepared.ExpectedPublicKey)
			reservations := &appsv1.ConsensusKeyReservationList{}
			require.NoError(t, r.List(ctx, reservations))
			require.Len(t, reservations.Items, 1)
			require.Equal(t, correctedKey, reservations.Items[0].Spec.PublicKey)
			for i := 0; i < 2; i++ {
				_, err = r.ensureCosmosignerWithParams(ctx, node, prepared)
				require.NoError(t, err)
			}
			liveKey, found, err := cosmosigner.LiveSigningPublicKey(ctx, r.Client, node, node.Namespace, prepared.Name)
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, correctedKey, liveKey)
		})
	}
}

func TestPKCS11InitialPublicKeyCorrectionRequiresStoppedOwnedClaim(t *testing.T) {
	for _, foreign := range []bool{false, true} {
		t.Run(map[bool]string{false: "terminating pod", true: "foreign reservation"}[foreign], func(t *testing.T) {
			ctx := context.Background()
			r, node := pkcs11TestNode(t, true)
			old, err := r.preflightCosmosigner(ctx, node)
			require.NoError(t, err)
			createPKCS11LiveSigner(t, r, node, old)
			reservation := &appsv1.ConsensusKeyReservation{}
			require.NoError(t, r.Get(ctx, client.ObjectKey{Name: cosmosigner.ConsensusKeyReservationName(node.Status.ChainID, old.ExpectedPublicKey)}, reservation))
			reservation.UID = "reservation-uid"
			if foreign {
				reservation.Spec.OwnerUID = "another-owner"
			}
			require.NoError(t, r.Update(ctx, reservation))
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: old.Name + "-0", Namespace: node.Namespace, Finalizers: []string{"test.example/hold"}}}
			if !foreign {
				require.NoError(t, r.Create(ctx, pod))
				require.NoError(t, r.Delete(ctx, pod))
			}
			node.Spec.Cosmosigner.Backend.PKCS11.PublicKey = "AQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
			require.NoError(t, r.Update(ctx, node))
			for i := 0; i < 3; i++ {
				_, err = r.preflightCosmosigner(ctx, node)
				require.Error(t, err)
				require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(reservation), &appsv1.ConsensusKeyReservation{}))
			}
			if !foreign {
				require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(pod), pod))
				pod.Finalizers = nil
				require.NoError(t, r.Update(ctx, pod))
				var prepared cosmosigner.Params
				for i := 0; i < 3; i++ {
					prepared, err = r.preflightCosmosigner(ctx, node)
					if err == nil {
						break
					}
				}
				require.NoError(t, err)
				require.Equal(t, node.Spec.Cosmosigner.Backend.PKCS11.PublicKey, prepared.ExpectedPublicKey)
			}
		})
	}
}
