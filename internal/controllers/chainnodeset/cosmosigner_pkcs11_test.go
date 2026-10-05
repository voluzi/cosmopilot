package chainnodeset

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	k8sappsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	appsv1 "github.com/voluzi/cosmopilot/v5/api/v1"
	"github.com/voluzi/cosmopilot/v5/internal/cosmosigner"
)

func TestPKCS11CosmosignerSuppliedPublicKey(t *testing.T) {
	const publicKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	for _, placement := range []string{"validator", "sentry", "group"} {
		t.Run(placement, func(t *testing.T) {
			signer := &appsv1.Cosmosigner{Image: ptr.To("ghcr.io/voluzi/cosmosigner:edge-pkcs11"), Backend: appsv1.CosmosignerBackend{PKCS11: &appsv1.CosmosignerPKCS11Backend{
				Module: "/vendor/lib.so", Slot: ptr.To(int64(0)), KeyLabel: "validator", PublicKey: publicKey, PINSecret: *claimSelector("pin"),
			}}}

			set := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{Name: "nodes", Namespace: "default"}, Spec: appsv1.ChainNodeSetSpec{Cosmosigner: signer}, Status: appsv1.ChainNodeSetStatus{ChainID: "test-1"}}
			if placement == "validator" {
				set.Spec.Validator = &appsv1.NodeSetValidatorConfig{}
			} else {
				set.Spec.Nodes = []appsv1.NodeGroupSpec{{Name: "sentries", Instances: ptr.To(1)}}
				if placement == "sentry" {
					signer.NodeGroups = []string{"sentries"}
				} else {
					set.Spec.Cosmosigner = nil
					set.Spec.Nodes[0].Cosmosigner = signer
				}
			}
			r := newValidatorTestReconciler(t, set, claimSecret("pin"))
			resolved := resolveSingleSigner(t, set)
			params, err := r.cosmosignerParams(context.Background(), set, resolved)
			require.NoError(t, err)
			require.NotNil(t, params.Backend.PKCS11)
			require.Nil(t, params.Backend.Software)
			require.Equal(t, ptr.To(int64(0)), params.Backend.PKCS11.Slot)
			require.Equal(t, "ghcr.io/voluzi/cosmosigner:edge-pkcs11", params.Image)
			key, err := r.cosmosignerPublicKeyWithParams(context.Background(), set, resolved, params)
			require.NoError(t, err)
			require.Equal(t, publicKey, key)
			pending, changed, err := r.maybeImportCosmosignerKey(context.Background(), set, resolved, params)
			require.NoError(t, err)
			require.False(t, pending)
			require.False(t, changed)
		})
	}
}

func TestPrepareCosmosignerParamsRejectsDifferentRecordedValidatorPublicKeyPKCS11(t *testing.T) {
	const onChainKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	onChainValidator := []appsv1.ChainNodeSetValidatorStatus{{
		Name: "test-nodeset-validators-0", Group: "validators",
		PubKey: `{"@type":"/cosmos.crypto.ed25519.PubKey","key":"` + onChainKey + `"}`,
	}}
	cases := []struct {
		name         string
		appliedKey   string
		validators   []appsv1.ChainNodeSetValidatorStatus
		wantErr      string
		wantReplicas int32
	}{
		{
			// A refused desired key must not stop a signer still serving the verified on-chain key.
			name: "desired key mismatches on-chain key", appliedKey: onChainKey, validators: onChainValidator,
			wantErr: "on-chain validator public key", wantReplicas: 1,
		},
		{
			name: "desired key differs from applied key", appliedKey: onChainKey,
			wantErr: "cannot change a validator public key after rollout", wantReplicas: 1,
		},
		{
			// The live signer is itself serving a key that does not match the chain.
			name: "applied key mismatches on-chain key", appliedKey: "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBA=", validators: onChainValidator,
			wantErr: "on-chain validator public key", wantReplicas: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			const desiredKey = "AQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
			nodeSet := &appsv1.ChainNodeSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: "nodeset-uid"},
				Spec: appsv1.ChainNodeSetSpec{
					Genesis: &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
					Nodes: []appsv1.NodeGroupSpec{{
						Name: "validators", Instances: ptr.To(1),
						Validator: &appsv1.NodeSetValidatorConfig{PrivateKeySecret: ptr.To("validator-key")},
						Cosmosigner: &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{
							PKCS11: &appsv1.CosmosignerPKCS11Backend{Module: "/vendor/lib.so", TokenLabel: "validator", KeyLabel: "consensus", PublicKey: desiredKey, PINSecret: *claimSelector("pin")},
						}},
					}},
				},
				Status: appsv1.ChainNodeSetStatus{
					ChainID: "test-1",
					Cosmosigners: []appsv1.CosmosignerStatus{{
						Name: "test-nodeset-validators-signer", AppliedDigest: "rolled-out", PublicKey: tc.appliedKey,
					}},
					Validators: tc.validators,
				},
			}
			one := int32(1)
			sts := &k8sappsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset-validators-signer", Namespace: nodeSet.Namespace}, Spec: k8sappsv1.StatefulSetSpec{
				Replicas: &one, Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: cosmosigner.InstanceLabels("test-nodeset-validators-signer")}},
			}}
			require.NoError(t, controllerutil.SetControllerReference(nodeSet, sts, testScheme(t)))
			r := newValidatorTestReconciler(t, nodeSet, sts, claimSecret("pin"))

			_, err := r.prepareCosmosignerParams(context.Background(), nodeSet)
			require.ErrorContains(t, err, tc.wantErr)
			reservation := &appsv1.ConsensusKeyReservation{}
			getErr := r.Get(context.Background(), client.ObjectKey{Name: cosmosigner.ConsensusKeyReservationName("test-1", desiredKey)}, reservation)
			require.True(t, apierrors.IsNotFound(getErr), "a rejected signer key must not leave an immutable reservation: %v", getErr)
			freshSTS := &k8sappsv1.StatefulSet{}
			require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(sts), freshSTS))
			require.Equal(t, tc.wantReplicas, ptr.Deref(freshSTS.Spec.Replicas, -1))
		})
	}
}

func TestPKCS11AlternateSelectorsCannotReserveSamePublicKey(t *testing.T) {
	signer := func(label string) *appsv1.Cosmosigner {
		return &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{PKCS11: &appsv1.CosmosignerPKCS11Backend{Module: "/vendor/lib.so", TokenLabel: label, KeyLabel: "validator", PublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", PINSecret: *claimSelector("pin")}}}
	}
	set := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{Name: "nodes", Namespace: "default", UID: "nodes-uid"}, Spec: appsv1.ChainNodeSetSpec{Nodes: []appsv1.NodeGroupSpec{{Name: "first", Instances: ptr.To(1), Cosmosigner: signer("label-one")}, {Name: "second", Instances: ptr.To(1), Cosmosigner: signer("label-two")}}}, Status: appsv1.ChainNodeSetStatus{ChainID: "test-1"}}
	r := newValidatorTestReconciler(t, set, claimSecret("pin"))
	_, err := r.prepareCosmosignerParams(context.Background(), set)
	require.ErrorIs(t, err, cosmosigner.ErrConsensusKeyReservationConflict)
}

func TestPKCS11AddressMigrationRetainsState(t *testing.T) {
	for name, change := range map[string]func(*appsv1.CosmosignerPKCS11Backend){
		"module":        func(p *appsv1.CosmosignerPKCS11Backend) { p.Module = "/other.so" },
		"selector":      func(p *appsv1.CosmosignerPKCS11Backend) { p.TokenLabel = ""; p.Slot = ptr.To(int64(0)) },
		"key ID":        func(p *appsv1.CosmosignerPKCS11Backend) { p.KeyID = "01" },
		"PIN reference": func(p *appsv1.CosmosignerPKCS11Backend) { p.PINSecret = *claimSelector("other-pin") },
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			c := &appsv1.Cosmosigner{NodeGroups: []string{"sentries"}, Backend: appsv1.CosmosignerBackend{PKCS11: &appsv1.CosmosignerPKCS11Backend{Module: "/vendor/lib.so", TokenLabel: "validator", KeyLabel: "consensus", PublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", PINSecret: *claimSelector("pin")}}}
			set := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{Name: "nodes", Namespace: "default", UID: "nodes-uid"}, Spec: appsv1.ChainNodeSetSpec{Cosmosigner: c, Nodes: []appsv1.NodeGroupSpec{{Name: "sentries", Instances: ptr.To(1)}}}, Status: appsv1.ChainNodeSetStatus{ChainID: "test-1"}}
			r := newValidatorTestReconciler(t, set, claimSecret("pin"), claimSecret("other-pin"))
			signer := resolveSingleSigner(t, set)
			prepared, err := r.prepareCosmosignerParams(ctx, set)
			require.NoError(t, err)
			old := prepared[signer.Name]
			claim := nodeSetCosmosignerReservationClaim(set, signer)
			digest, err := old.LifecycleDigest(signer.Digest())
			require.NoError(t, err)
			set.Status.Cosmosigners = []appsv1.CosmosignerStatus{{Name: signer.Name, AppliedDigest: digest, PublicKey: old.ExpectedPublicKey}}
			require.NoError(t, r.Status().Update(ctx, set))
			change(set.Spec.Cosmosigner.Backend.PKCS11)
			require.NoError(t, r.Update(ctx, set))
			prepared, err = r.prepareCosmosignerParams(ctx, set)
			require.NoError(t, err)
			reservations := &appsv1.ConsensusKeyReservationList{}
			require.NoError(t, r.List(ctx, reservations))
			require.Len(t, reservations.Items, 1)
			require.Equal(t, claim, reservations.Items[0].Spec.Claim)
			pending, err := r.reconcileCosmosignerMigrations(ctx, set, prepared)
			require.NoError(t, err)
			require.True(t, pending)
			migration := set.Status.Cosmosigners[0].Migration
			require.NotNil(t, migration)
			require.False(t, migration.ResetState)
			require.Equal(t, appsv1.CosmosignerMigrationQuiescing, migration.Phase)
		})
	}
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
			c := &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{PKCS11: &appsv1.CosmosignerPKCS11Backend{Module: "/vendor/lib.so", TokenLabel: "validator", KeyLabel: "consensus", PublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", PINSecret: *claimSelector("pin")}}}
			set := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{Name: "nodes", Namespace: "default", UID: "nodes-uid"}, Spec: appsv1.ChainNodeSetSpec{Genesis: &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis.json")}, Cosmosigner: c, Nodes: []appsv1.NodeGroupSpec{{Name: "sentries", Instances: ptr.To(1)}}}, Status: appsv1.ChainNodeSetStatus{ChainID: "test-1"}}
			if tc.validator {
				set.Spec.Validator = &appsv1.NodeSetValidatorConfig{}
			} else {
				c.NodeGroups = []string{"sentries"}
			}
			r := newValidatorTestReconciler(t, set, claimSecret("pin"))
			signer := resolveSingleSigner(t, set)
			prepared, err := r.prepareCosmosignerParams(ctx, set)
			require.NoError(t, err)
			old := prepared[signer.Name]
			_, err = r.initCosmosignerLocks(ctx, set)
			require.NoError(t, err)
			config, err := old.ConfigYAML()
			require.NoError(t, err)
			cm, err := old.ConfigMap(config)
			require.NoError(t, err)
			sts, err := old.StatefulSet(config)
			require.NoError(t, err)
			sts.UID = "signer-uid"
			if tc.everRolledOut {
				sts.Annotations = map[string]string{cosmosigner.EverRolledOutAnnotation: "true"}
			}
			for _, obj := range []client.Object{cm, sts} {
				require.NoError(t, controllerutil.SetControllerReference(set, obj, r.Scheme))
				require.NoError(t, r.Create(ctx, obj))
			}
			pvc := sts.Spec.VolumeClaimTemplates[0].DeepCopy()
			pvc.Name = "data-" + sts.Name + "-0"
			pvc.Namespace = set.Namespace
			pvc.Spec.VolumeName = "signer-state"
			pvc.Status.Phase = corev1.ClaimBound
			require.NoError(t, r.Create(ctx, pvc))
			oldReservation := &appsv1.ConsensusKeyReservation{}
			require.NoError(t, r.Get(ctx, client.ObjectKey{Name: cosmosigner.ConsensusKeyReservationName(set.Status.ChainID, old.ExpectedPublicKey)}, oldReservation))
			oldReservation.UID = "reservation-uid"
			require.NoError(t, r.Update(ctx, oldReservation))
			if tc.recorded {
				set.Status.Cosmosigners = []appsv1.CosmosignerStatus{{Name: signer.Name, AppliedDigest: "applied", PublicKey: old.ExpectedPublicKey, Replicas: ptr.To(int32(1))}}
				require.NoError(t, r.Status().Update(ctx, set))
			}
			const correctedKey = "AQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
			set.Spec.Cosmosigner.Backend.PKCS11.PublicKey = correctedKey
			require.NoError(t, r.Update(ctx, set))
			for i := 0; i < 3; i++ {
				err = r.preflightCosmosigners(ctx, set)
				if err == nil {
					prepared, err = r.prepareCosmosignerParams(ctx, set)
				}
				if err == nil && prepared[signer.Name].ExpectedPublicKey == correctedKey {
					break
				}
			}
			if tc.recorded || tc.everRolledOut {
				require.Error(t, err)
				require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(oldReservation), &appsv1.ConsensusKeyReservation{}))
				return
			}
			require.NoError(t, err)
			require.Equal(t, correctedKey, prepared[signer.Name].ExpectedPublicKey)
			reservations := &appsv1.ConsensusKeyReservationList{}
			require.NoError(t, r.List(ctx, reservations))
			require.Len(t, reservations.Items, 1)
			require.Equal(t, correctedKey, reservations.Items[0].Spec.PublicKey)
			_, err = r.reconcileSigner(ctx, set, resolveSingleSigner(t, set), prepared[signer.Name])
			require.NoError(t, err)
			liveKey, found, err := cosmosigner.LiveSigningPublicKey(ctx, r.Client, set, set.Namespace, old.Name)
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, correctedKey, liveKey)
		})
	}
}
