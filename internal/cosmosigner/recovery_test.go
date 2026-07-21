package cosmosigner

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
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
		{name: "expected public key", mutate: func(p *Params) { p.ExpectedPublicKey = "BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB=" }},
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
			require.Error(t, err)
			require.Contains(t, err.Error(), "live signing identity")
		})
	}
}
