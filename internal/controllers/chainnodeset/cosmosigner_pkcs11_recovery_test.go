package chainnodeset

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1 "github.com/voluzi/cosmopilot/v5/api/v1"
	"github.com/voluzi/cosmopilot/v5/internal/controllers"
	"github.com/voluzi/cosmopilot/v5/internal/cosmosigner"
)

const manualRecoveryPublicKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
const manualRecoveryCorrectedKey = "AQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

func manualRecoveryPVC(t *testing.T, c client.Client, sts *apps.StatefulSet) *core.PersistentVolumeClaim {
	t.Helper()
	pvc := sts.Spec.VolumeClaimTemplates[0].DeepCopy()
	pvc.Name = "data-" + sts.Name + "-0"
	pvc.Namespace = sts.Namespace
	pvc.UID = "raft-pvc-uid"
	pvc.Spec.VolumeName = "retained-raft-state"
	pvc.Status.Phase = core.ClaimBound
	require.NoError(t, c.Create(context.Background(), pvc))
	return pvc
}

func readManualRecoveryConfig(t *testing.T, c client.Client, key client.ObjectKey) *cosmosigner.Config {
	t.Helper()
	cm := &core.ConfigMap{}
	require.NoError(t, c.Get(context.Background(), key, cm))
	config := &cosmosigner.Config{}
	require.NoError(t, yaml.Unmarshal([]byte(cm.Data["config.yaml"]), config))
	return config
}

func manualRecoveryFixture(t *testing.T, validator, grouped bool) (*Reconciler, *appsv1.ChainNodeSet, *apps.StatefulSet, *core.PersistentVolumeClaim) {
	t.Helper()
	signer := &appsv1.Cosmosigner{NodeGroups: []string{"group"}, Backend: appsv1.CosmosignerBackend{PKCS11: &appsv1.CosmosignerPKCS11Backend{Module: "/vendor/lib.so", TokenLabel: "validator", KeyLabel: "consensus", PublicKey: manualRecoveryPublicKey, PINSecret: *claimSelector("pin")}}}
	node := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{Name: "nodes", Namespace: "default", UID: "nodes-uid"}, Spec: appsv1.ChainNodeSetSpec{App: appsv1.AppSpec{Image: "image", App: "appd", Version: ptr.To("1.0.0")}, Genesis: &appsv1.GenesisConfig{ConfigMap: ptr.To("genesis")}, Cosmosigner: signer, Nodes: []appsv1.NodeGroupSpec{{Name: "group", Instances: ptr.To(1)}}}}
	if validator {
		node.Spec.Nodes[0].Validator = &appsv1.NodeSetValidatorConfig{}
	}
	if grouped {
		node.Spec.Nodes[0].Cosmosigner = signer
		signer.NodeGroups = nil
		node.Spec.Cosmosigner = nil
	}
	node.SetEstablishedChainID("test-1")
	r := newValidatorTestReconciler(t, node, claimSecret("pin"), &core.Namespace{ObjectMeta: metav1.ObjectMeta{Name: node.Namespace}}, &core.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "genesis", Namespace: node.Namespace}, Data: map[string]string{"genesis.json": `{"chain_id":"test-1"}`}})
	r.recorder = record.NewFakeRecorder(1000)
	resolved := node.ResolveCosmosigners()[0]
	key := client.ObjectKey{Namespace: node.Namespace, Name: node.CosmosignerResourceName(resolved)}
	var sts *apps.StatefulSet
	for i := 0; i < 10; i++ {
		_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(node)})
		require.NoError(t, err)
		sts = &apps.StatefulSet{}
		if r.Get(context.Background(), key, sts) == nil {
			break
		}
	}
	require.NotEmpty(t, sts.Name)
	sts.UID = "signer-uid"
	require.NoError(t, r.Update(context.Background(), sts))
	pvc := manualRecoveryPVC(t, r.Client, sts)
	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(node)})
	require.NoError(t, err)
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(node), node))
	status := node.GetCosmosignerStatus(resolved.Name)
	require.NotNil(t, status)
	require.Empty(t, status.AppliedDigest)
	require.Empty(t, status.PublicKey)

	target := &core.Pod{ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: node.Namespace, Labels: map[string]string{
		controllers.LabelChainNodeSet:      node.Name,
		controllers.LabelChainNodeSetGroup: "group",
		controllers.LabelCosmosignerTarget: sts.Name,
		controllers.LabelValidator:         controllers.StringValueFalse,
	}}}
	if validator {
		target.Labels[controllers.LabelValidator] = controllers.StringValueTrue
	}
	require.NoError(t, r.Create(context.Background(), target))
	return r, node, sts, pvc
}

func TestPKCS11ManualRecovery(t *testing.T) {
	var changes = []struct {
		name      string
		change    func(*appsv1.CosmosignerPKCS11Backend)
		keyChange bool
	}{
		{name: "module", change: func(p *appsv1.CosmosignerPKCS11Backend) { p.Module = "/correct.so" }},
		{name: "token label", change: func(p *appsv1.CosmosignerPKCS11Backend) { p.TokenLabel = "correct-token" }},
		{name: "slot", change: func(p *appsv1.CosmosignerPKCS11Backend) { p.TokenLabel = ""; p.Slot = ptr.To(int64(4)) }},
		{name: "key label", change: func(p *appsv1.CosmosignerPKCS11Backend) { p.KeyLabel = "correct-key" }},
		{name: "key ID", change: func(p *appsv1.CosmosignerPKCS11Backend) { p.KeyID = "01" }},
		{name: "public key", keyChange: true, change: func(p *appsv1.CosmosignerPKCS11Backend) { p.PublicKey = manualRecoveryCorrectedKey }},
	}
	for _, placement := range []struct {
		name               string
		validator, grouped bool
	}{{"validator group", true, true}, {"sentry", false, false}, {"group sentry", false, true}} {
		t.Run(placement.name, func(t *testing.T) {
			for _, tc := range changes {
				t.Run(tc.name, func(t *testing.T) {
					r, node, sts, pvc := manualRecoveryFixture(t, placement.validator, placement.grouped)

					ctx := context.Background()
					req := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(node)}
					key := client.ObjectKeyFromObject(sts)
					oldCM := &core.ConfigMap{}
					require.NoError(t, r.Get(ctx, key, oldCM))
					oldReservation := &appsv1.ConsensusKeyReservation{}
					reservationKey := client.ObjectKey{Name: cosmosigner.ConsensusKeyReservationName("test-1", manualRecoveryPublicKey)}
					require.NoError(t, r.Get(ctx, reservationKey, oldReservation))
					old := node.DeepCopy()
					tc.change(node.ResolveCosmosigners()[0].Spec.Backend.PKCS11)
					_, err := node.Validate(old)
					require.NoError(t, err)
					require.NoError(t, r.Update(ctx, node))
					for i := 0; i < 2; i++ {
						_, err = r.Reconcile(ctx, req)
						require.ErrorIs(t, err, cosmosigner.ErrRecoveredIdentityMismatch)
					}
					currentCM := &core.ConfigMap{}
					require.NoError(t, r.Get(ctx, key, currentCM))
					require.Equal(t, oldCM.Data, currentCM.Data, "spec correction must not re-pin the live signer")
					currentSTS := &apps.StatefulSet{}
					require.NoError(t, r.Get(ctx, key, currentSTS))
					require.Equal(t, sts.Spec.Template, currentSTS.Spec.Template)
					currentReservation := &appsv1.ConsensusKeyReservation{}
					require.NoError(t, r.Get(ctx, reservationKey, currentReservation))
					require.Equal(t, oldReservation.Spec, currentReservation.Spec)
					reservations := &appsv1.ConsensusKeyReservationList{}
					require.NoError(t, r.List(ctx, reservations))
					require.Len(t, reservations.Items, 1)
					require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(node), node))
					status := node.GetCosmosignerStatus(node.ResolveCosmosigners()[0].Name)
					require.Empty(t, status.AppliedDigest)
					require.Empty(t, status.PublicKey)
					currentSTS.Spec.PersistentVolumeClaimRetentionPolicy.WhenDeleted = apps.RetainPersistentVolumeClaimRetentionPolicyType
					require.NoError(t, r.Update(ctx, currentSTS))
					require.NoError(t, r.Delete(ctx, currentSTS, client.PropagationPolicy(metav1.DeletePropagationForeground)))
					if tc.keyChange {
						require.NoError(t, r.Delete(ctx, currentReservation))
					}
					for i := 0; i < 6; i++ {
						_, err = r.Reconcile(ctx, req)
						require.NoError(t, err, "manual recovery reconcile %d", i)
					}
					config := readManualRecoveryConfig(t, r.Client, key)
					want := node.ResolveCosmosigners()[0].Spec.Backend.PKCS11
					require.Equal(t, want.PublicKey, config.ExpectedPublicKey)
					require.Equal(t, want.Module, config.Backend.PKCS11.Module)
					require.Equal(t, want.TokenLabel, config.Backend.PKCS11.TokenLabel)
					require.Equal(t, want.Slot, config.Backend.PKCS11.Slot)
					require.Equal(t, want.KeyLabel, config.Backend.PKCS11.KeyLabel)
					require.Equal(t, want.KeyID, config.Backend.PKCS11.KeyID)
					require.NoError(t, r.Get(ctx, key, currentSTS))
					require.Equal(t, int32(1), *currentSTS.Spec.Replicas)
					require.Contains(t, currentSTS.Spec.Template.Spec.Containers[0].Args, want.PublicKey)
					currentPVC := &core.PersistentVolumeClaim{}
					require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(pvc), currentPVC))
					require.Equal(t, pvc.UID, currentPVC.UID)
					require.Equal(t, pvc.Spec, currentPVC.Spec)
					require.Nil(t, currentPVC.DeletionTimestamp)
					require.Contains(t, currentPVC.Finalizers, cosmosigner.RetainedStateFinalizer)
					require.NoError(t, r.Get(ctx, client.ObjectKey{Name: cosmosigner.ConsensusKeyReservationName("test-1", want.PublicKey)}, currentReservation))
					if tc.keyChange {
						require.True(t, apierrors.IsNotFound(r.Get(ctx, reservationKey, &appsv1.ConsensusKeyReservation{})))
					}
				})
			}
		})
	}
}

func TestPKCS11RolledOutValidatorKeyChangeRefused(t *testing.T) {
	r, node, sts, _ := manualRecoveryFixture(t, true, true)
	ctx := context.Background()
	node.Status.Cosmosigners[0].AppliedDigest = "recorded-rollout"
	node.Status.Cosmosigners[0].PublicKey = manualRecoveryPublicKey
	require.NoError(t, r.Status().Update(ctx, node))
	old := node.DeepCopy()
	node.ResolveCosmosigners()[0].Spec.Backend.PKCS11.PublicKey = manualRecoveryCorrectedKey
	_, err := node.Validate(old)
	require.NoError(t, err)
	require.NoError(t, r.Update(ctx, node))
	_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(node)})
	require.ErrorContains(t, err, "cannot change a validator public key after rollout")
	require.Equal(t, manualRecoveryPublicKey, readManualRecoveryConfig(t, r.Client, client.ObjectKeyFromObject(sts)).ExpectedPublicKey)
	require.NoError(t, r.Get(ctx, client.ObjectKey{Name: cosmosigner.ConsensusKeyReservationName("test-1", manualRecoveryPublicKey)}, &appsv1.ConsensusKeyReservation{}))
	require.True(t, apierrors.IsNotFound(r.Get(ctx, client.ObjectKey{Name: cosmosigner.ConsensusKeyReservationName("test-1", manualRecoveryCorrectedKey)}, &appsv1.ConsensusKeyReservation{})))
}

func TestPKCS11PublicKeyCorrectionPreservesRetention(t *testing.T) {
	r, node, sts, _ := manualRecoveryFixture(t, true, true)
	ctx := context.Background()
	old := node.DeepCopy()
	node.ResolveCosmosigners()[0].Spec.Backend.PKCS11.PublicKey = manualRecoveryCorrectedKey
	_, err := node.Validate(old)
	require.NoError(t, err)
	require.NoError(t, r.Update(ctx, node))
	sts.Spec.PersistentVolumeClaimRetentionPolicy.WhenDeleted = apps.RetainPersistentVolumeClaimRetentionPolicyType
	require.NoError(t, r.Update(ctx, sts))
	for i := 0; i < 2; i++ {
		_, err = r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(node)})
		assert.ErrorIs(t, err, cosmosigner.ErrRecoveredIdentityMismatch)
		current := &apps.StatefulSet{}
		require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(sts), current))
		require.Equal(t, sts.Spec.Template, current.Spec.Template)
		require.Equal(t, sts.Spec.PersistentVolumeClaimRetentionPolicy, current.Spec.PersistentVolumeClaimRetentionPolicy)
		require.Equal(t, manualRecoveryPublicKey, readManualRecoveryConfig(t, r.Client, client.ObjectKeyFromObject(sts)).ExpectedPublicKey)
	}
}
