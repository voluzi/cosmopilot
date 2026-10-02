package cosmosigner

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestValidateRecoveredSigningIdentityRejectsOrphanedRaftState(t *testing.T) {
	const namespace, name = "default", "validator-signer"
	owner := fakeOwner("validator", types.UID("validator-uid"))
	desired := Params{
		Name: name, Namespace: namespace,
		Backend: Backend{Vault: &VaultBackend{Address: "https://vault.example:8200", Mount: "transit", KeyName: "desired-key"}},
	}
	live := desired
	liveVault := *desired.Backend.Vault
	liveVault.KeyName = "live-key"
	live.Backend.Vault = &liveVault
	liveYAML, err := live.ConfigYAML()
	require.NoError(t, err)
	configMap, err := live.ConfigMap(liveYAML)
	require.NoError(t, err)
	configMap.OwnerReferences = []metav1.OwnerReference{ownerRef(owner)}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: dataVolumeName + "-" + name + "-0", Namespace: namespace, Labels: pvcOwnerLabels(name, owner.UID),
	}}
	c := fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(configMap, pvc).Build()

	err = ValidateRecoveredSigningIdentity(context.Background(), c, owner, desired)
	require.Error(t, err)
	require.Contains(t, err.Error(), "orphaned raft-state")
}

func TestValidateRecoveredSigningIdentityRejectsTornConfigUpdate(t *testing.T) {
	const namespace, name = "default", "validator-signer"
	owner := fakeOwner("validator", types.UID("validator-uid"))
	desired := Params{
		Name: name, Namespace: namespace, Replicas: 1, StateStorageSize: "1Gi",
		ExpectedPublicKey: reservationTestPublicKey,
		Backend: Backend{Vault: &VaultBackend{
			Address: "https://vault.example:8200", Mount: "transit", KeyName: "desired-key",
			TokenSecret: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "vault-token"}, Key: "token"},
		}},
	}
	desiredYAML, err := desired.ConfigYAML()
	require.NoError(t, err)
	configMap, err := desired.ConfigMap(desiredYAML)
	require.NoError(t, err)
	configMap.OwnerReferences = []metav1.OwnerReference{ownerRef(owner)}

	live := desired
	liveVault := *desired.Backend.Vault
	liveVault.KeyName = "live-key"
	live.Backend.Vault = &liveVault
	liveYAML, err := live.ConfigYAML()
	require.NoError(t, err)
	statefulSet, err := live.StatefulSet(liveYAML)
	require.NoError(t, err)
	statefulSet.OwnerReferences = []metav1.OwnerReference{ownerRef(owner)}
	c := fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(configMap, statefulSet).Build()

	err = ValidateRecoveredSigningIdentity(context.Background(), c, owner, desired)
	require.Error(t, err)
	require.Contains(t, err.Error(), "ROLLME")
}

func TestValidateRecoveredSigningIdentityRequiresPinnedRuntimeIdentity(t *testing.T) {
	const namespace, name = "default", "validator-signer"
	owner := fakeOwner("validator", types.UID("validator-uid"))
	base := Params{
		Name: name, Namespace: namespace, Replicas: 1, StateStorageSize: "1Gi",
		ExpectedPublicKey: reservationTestPublicKey,
		Backend: Backend{Vault: &VaultBackend{
			Address: "https://vault.example:8200", Mount: "transit", KeyName: "validator", KeyVersion: 1,
			TokenSecret: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "vault-token"}, Key: "token"},
		}},
	}

	for _, tc := range []struct {
		name   string
		mutate func(*Params)
	}{
		{name: "expected public key", mutate: func(p *Params) { p.ExpectedPublicKey = "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBA=" }},
		{name: "vault key version", mutate: func(p *Params) { p.Backend.Vault.KeyVersion = 2 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			live := base
			liveVault := *base.Backend.Vault
			live.Backend.Vault = &liveVault
			tc.mutate(&live)
			liveYAML, err := live.ConfigYAML()
			require.NoError(t, err)
			configMap, err := live.ConfigMap(liveYAML)
			require.NoError(t, err)
			statefulSet, err := live.StatefulSet(liveYAML)
			require.NoError(t, err)
			configMap.OwnerReferences = []metav1.OwnerReference{ownerRef(owner)}
			statefulSet.OwnerReferences = []metav1.OwnerReference{ownerRef(owner)}
			c := fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(configMap, statefulSet).Build()

			err = ValidateRecoveredSigningIdentity(context.Background(), c, owner, base)
			require.ErrorIs(t, err, ErrRecoveredIdentityMismatch)
			require.Contains(t, err.Error(), "live signing identity")
		})
	}
}

func TestRecoveredSigningPublicKeyRequiresMatchingArgAndConfig(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*appsv1.StatefulSet)
		err    string
	}{
		{name: "coherent runtime pin", mutate: func(sts *appsv1.StatefulSet) {}},
		{name: "missing argument", mutate: func(sts *appsv1.StatefulSet) { sts.Spec.Template.Spec.Containers[0].Args = []string{"run"} }, err: "live signing identity is not pinned by exactly one expected-public-key argument"},
		{name: "duplicate argument", mutate: func(sts *appsv1.StatefulSet) {
			sts.Spec.Template.Spec.Containers[0].Args = append(sts.Spec.Template.Spec.Containers[0].Args, "--expected-public-key", reservationTestPublicKey)
		}, err: "live signing identity is not pinned by exactly one expected-public-key argument"},
		{name: "different argument", mutate: func(sts *appsv1.StatefulSet) {
			args := sts.Spec.Template.Spec.Containers[0].Args
			for i, arg := range args {
				if arg == "--expected-public-key" {
					args[i+1] = "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBA="
				}
			}
		}, err: "live signing identity argument does not match its ConfigMap"},
		{name: "retained state lost", mutate: func(sts *appsv1.StatefulSet) {
			sts.Annotations = map[string]string{retainedStateLostAnnotation: "true"}
		}, err: "retained state is lost; refusing to trust its live signing identity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			owner := fakeOwner("owner", types.UID("owner-uid"))
			params := testParams()
			yaml, err := params.ConfigYAML()
			require.NoError(t, err)
			cm, err := params.ConfigMap(yaml)
			require.NoError(t, err)
			sts, err := params.StatefulSet(yaml)
			require.NoError(t, err)
			cm.OwnerReferences = []metav1.OwnerReference{ownerRef(owner)}
			sts.OwnerReferences = []metav1.OwnerReference{ownerRef(owner)}
			tc.mutate(sts)
			c := fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(cm, sts).Build()
			publicKey, live, err := RecoveredSigningPublicKey(context.Background(), c, owner, params)
			if tc.err == "" {
				require.NoError(t, err)
				require.True(t, live)
				require.Equal(t, reservationTestPublicKey, publicKey)
			} else {
				require.EqualError(t, err, `cosmosigner "mychain-signer" `+tc.err)
				require.False(t, live)
				require.Empty(t, publicKey)
			}
		})
	}
}
