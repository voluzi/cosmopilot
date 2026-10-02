package chainnodeset

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	k8sappsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	appsv1 "github.com/voluzi/cosmopilot/v5/api/v1"
	"github.com/voluzi/cosmopilot/v5/internal/cosmosigner"
)

func TestRefusedKeyChangeKeepsRecoveringSignerRunning(t *testing.T) {
	const liveKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	for _, entryPoint := range []string{"preflight", "prepare"} {
		for _, tc := range []struct {
			name         string
			onChain      string
			validator    bool
			child        bool
			migration    bool
			conflict     bool
			wantReplicas int32
		}{
			{name: "recorded validator key", onChain: liveKey, validator: true, wantReplicas: 1},
			{name: "owned child key", onChain: liveKey, validator: true, child: true, wantReplicas: 1},
			{name: "different on-chain key", onChain: "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBA=", validator: true},
			{name: "unknown on-chain key", validator: true},
			{name: "sentry", onChain: liveKey},
			{name: "demoted validator", onChain: liveKey},
			{name: "migration in progress", onChain: liveKey, validator: true, migration: true},
			{name: "reservation conflict", onChain: liveKey, validator: true, conflict: true},
		} {
			t.Run(entryPoint+"/"+tc.name, func(t *testing.T) {
				ctx := context.Background()
				tokenSelector := &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "vault-token"}, Key: "token"}
				set := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: "default", UID: "set-uid"}, Spec: appsv1.ChainNodeSetSpec{Nodes: []appsv1.NodeGroupSpec{{
					Name: "validators", Instances: ptr.To(1), Cosmosigner: &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{Vault: &appsv1.CosmosignerVaultBackend{Address: "https://vault:8200", KeyName: "old-key", TokenSecret: tokenSelector}}},
				}}}, Status: appsv1.ChainNodeSetStatus{ChainID: "test-1"}}
				if tc.validator {
					set.Spec.Nodes[0].Validator = &appsv1.NodeSetValidatorConfig{}
				}
				signer := resolveSingleSigner(t, set)
				if tc.name == "demoted validator" {
					set.EnsureCosmosignerStatus(signer.Name).ServingGroup = "validators"
				}
				if tc.migration {
					set.EnsureCosmosignerStatus(signer.Name).Migration = &appsv1.CosmosignerMigrationStatus{Phase: appsv1.CosmosignerMigrationQuiescing}
				}
				token := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "vault-token", Namespace: set.Namespace}, Data: map[string][]byte{"token": []byte("token")}}
				r := newValidatorTestReconciler(t, set, token)
				if tc.onChain != "" {
					pubkey := `{"key":"` + tc.onChain + `"}`
					if tc.child {
						child := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: validatorNodeName(set, "validators", 0), Namespace: set.Namespace}, Status: appsv1.ChainNodeStatus{ChainID: set.Status.ChainID, PubKey: pubkey}}
						require.NoError(t, controllerutil.SetControllerReference(set, child, r.Scheme))
						require.NoError(t, r.Create(ctx, child))
					} else {
						set.Status.Validators = []appsv1.ChainNodeSetValidatorStatus{{Name: validatorNodeName(set, "validators", 0), Group: "validators", PubKey: pubkey}}
						require.NoError(t, r.Status().Update(ctx, set))
					}
				}
				oldParams, err := r.cosmosignerParams(ctx, set, signer)
				require.NoError(t, err)
				oldParams.ExpectedPublicKey = liveKey
				yaml, err := oldParams.ConfigYAML()
				require.NoError(t, err)
				cm, err := oldParams.ConfigMap(yaml)
				require.NoError(t, err)
				sts, err := oldParams.StatefulSet(yaml)
				require.NoError(t, err)
				sts.UID = "original-sts"
				require.NoError(t, controllerutil.SetControllerReference(set, cm, r.Scheme))
				require.NoError(t, controllerutil.SetControllerReference(set, sts, r.Scheme))
				require.NoError(t, r.Create(ctx, cm))
				require.NoError(t, r.Create(ctx, sts))
				pvc := sts.Spec.VolumeClaimTemplates[0].DeepCopy()
				pvc.Name = "data-" + sts.Name + "-0"
				pvc.Namespace = set.Namespace
				pvc.Spec.VolumeName = "signer-state"
				pvc.Status.Phase = corev1.ClaimBound
				require.NoError(t, r.Create(ctx, pvc))
				set.Spec.Nodes[0].Cosmosigner.Backend.Vault.KeyName = "new-key"
				require.NoError(t, r.Update(ctx, set))
				if tc.conflict {
					require.NoError(t, r.Create(ctx, &appsv1.ConsensusKeyReservation{ObjectMeta: metav1.ObjectMeta{Name: cosmosigner.ConsensusKeyReservationName(set.Status.ChainID, liveKey)}, Spec: appsv1.ConsensusKeyReservationSpec{
						ChainID: set.Status.ChainID, PublicKey: liveKey, OwnerUID: set.UID, OwnerKind: "ChainNodeSet", Namespace: set.Namespace, OwnerName: set.Name, Claim: "independent-signer",
					}}))
				}
				before := set.Status.DeepCopy()
				if entryPoint == "preflight" {
					err = r.preflightCosmosigners(ctx, set)
				} else {
					_, err = r.prepareCosmosignerParams(ctx, set)
				}
				require.ErrorContains(t, err, "live signing identity")
				fresh := &k8sappsv1.StatefulSet{}
				require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(sts), fresh))
				require.Equal(t, tc.wantReplicas, ptr.Deref(fresh.Spec.Replicas, int32(-1)))
				require.Equal(t, sts.UID, fresh.UID)
				freshCM := &corev1.ConfigMap{}
				require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(cm), freshCM))
				require.Equal(t, cm.Data, freshCM.Data)
				stored := &appsv1.ChainNodeSet{}
				require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(set), stored))
				require.Equal(t, *before, stored.Status)
				if tc.wantReplicas > 0 {
					reservation := &appsv1.ConsensusKeyReservation{}
					require.NoError(t, r.Get(ctx, client.ObjectKey{Name: cosmosigner.ConsensusKeyReservationName(set.Status.ChainID, liveKey)}, reservation))
					require.Equal(t, nodeSetCosmosignerReservationClaim(set, signer), reservation.Spec.Claim)
					require.Equal(t, set.UID, reservation.Spec.OwnerUID)
				}
			})
		}
	}
}
