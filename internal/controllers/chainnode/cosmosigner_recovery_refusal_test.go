package chainnode

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	k8sappsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	appsv1 "github.com/voluzi/cosmopilot/v5/api/v1"
	"github.com/voluzi/cosmopilot/v5/internal/controllers"
	"github.com/voluzi/cosmopilot/v5/internal/cosmosigner"
)

func TestRefusedKeyChangeKeepsRecoveringSignerRunning(t *testing.T) {
	const liveKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	for _, tc := range []struct {
		name         string
		onChain      string
		validator    bool
		migration    bool
		conflict     bool
		wantReplicas int32
	}{
		{name: "live key is on-chain", onChain: liveKey, validator: true, wantReplicas: 1},
		{name: "different on-chain key", onChain: "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBA=", validator: true},
		{name: "unknown on-chain key", validator: true},
		{name: "sentry", onChain: liveKey},
		{name: "demoted validator", onChain: liveKey},
		{name: "migration in progress", onChain: liveKey, validator: true, migration: true},
		{name: "reservation conflict", onChain: liveKey, validator: true, conflict: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			tokenSelector := &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "vault-token"}, Key: "token"}
			node := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: "validator", Namespace: "default", UID: "validator-uid"}, Spec: appsv1.ChainNodeSpec{
				Cosmosigner: &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{Vault: &appsv1.CosmosignerVaultBackend{Address: "https://vault:8200", KeyName: "old-key", TokenSecret: tokenSelector}}},
			}, Status: appsv1.ChainNodeStatus{ChainID: "test-1"}}
			if tc.name == "demoted validator" {
				node.Status.CosmosignerValidatorTargeted = ptr.To(true)
			}
			if tc.validator {
				node.Spec.Validator = &appsv1.ValidatorConfig{}
			}
			if tc.onChain != "" {
				node.Status.PubKey = `{"key":"` + tc.onChain + `"}`
			}
			if tc.migration {
				node.Status.CosmosignerMigration = &appsv1.CosmosignerMigrationStatus{Phase: appsv1.CosmosignerMigrationQuiescing}
			}
			scheme := runtime.NewScheme()
			require.NoError(t, clientgoscheme.AddToScheme(scheme))
			require.NoError(t, appsv1.AddToScheme(scheme))
			token := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "vault-token", Namespace: node.Namespace}, Data: map[string][]byte{"token": []byte("token")}}
			cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(node).WithObjects(node, token).Build()
			r := &Reconciler{Client: cl, APIReader: cl, Scheme: scheme, opts: &controllers.ControllerRunOptions{}, recorder: record.NewFakeRecorder(10)}
			oldParams, err := r.cosmosignerParams(ctx, node)
			require.NoError(t, err)
			oldParams.ExpectedPublicKey = liveKey
			yaml, err := oldParams.ConfigYAML()
			require.NoError(t, err)
			cm, err := oldParams.ConfigMap(yaml)
			require.NoError(t, err)
			sts, err := oldParams.StatefulSet(yaml)
			require.NoError(t, err)
			sts.UID = "original-sts"
			require.NoError(t, controllerutil.SetControllerReference(node, cm, scheme))
			require.NoError(t, controllerutil.SetControllerReference(node, sts, scheme))
			require.NoError(t, r.Create(ctx, cm))
			require.NoError(t, r.Create(ctx, sts))
			pvc := sts.Spec.VolumeClaimTemplates[0].DeepCopy()
			pvc.Name = "data-" + sts.Name + "-0"
			pvc.Namespace = node.Namespace
			pvc.Spec.VolumeName = "signer-state"
			pvc.Status.Phase = corev1.ClaimBound
			require.NoError(t, r.Create(ctx, pvc))
			node.Spec.Cosmosigner.Backend.Vault.KeyName = "new-key"
			require.NoError(t, r.Update(ctx, node))
			if tc.conflict {
				require.NoError(t, r.Create(ctx, &appsv1.ConsensusKeyReservation{ObjectMeta: metav1.ObjectMeta{Name: cosmosigner.ConsensusKeyReservationName(node.Status.ChainID, liveKey)}, Spec: appsv1.ConsensusKeyReservationSpec{
					ChainID: node.Status.ChainID, PublicKey: liveKey, OwnerUID: node.UID, OwnerKind: "ChainNode", Namespace: node.Namespace, OwnerName: node.Name, Claim: "independent-signer",
				}}))
			}
			before := node.Status.DeepCopy()
			_, err = r.preflightCosmosigner(ctx, node)
			require.ErrorContains(t, err, "live signing identity")
			fresh := &k8sappsv1.StatefulSet{}
			require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(sts), fresh))
			require.Equal(t, tc.wantReplicas, ptr.Deref(fresh.Spec.Replicas, int32(-1)))
			require.Equal(t, sts.UID, fresh.UID)
			freshCM := &corev1.ConfigMap{}
			require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(cm), freshCM))
			require.Equal(t, cm.Data, freshCM.Data)
			stored := &appsv1.ChainNode{}
			require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(node), stored))
			require.Equal(t, *before, stored.Status)
			if tc.wantReplicas > 0 {
				reservation := &appsv1.ConsensusKeyReservation{}
				require.NoError(t, r.Get(ctx, client.ObjectKey{Name: cosmosigner.ConsensusKeyReservationName(node.Status.ChainID, liveKey)}, reservation))
				require.Equal(t, standaloneCosmosignerReservationClaim(node), reservation.Spec.Claim)
				require.Equal(t, node.UID, reservation.Spec.OwnerUID)
			}
		})
	}
}
