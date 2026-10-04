package cosmosigner

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func pkcs11Params() Params {
	p := testParams()
	p.Image = "ghcr.io/voluzi/cosmosigner:edge-pkcs11"
	p.Backend = Backend{PKCS11: &PKCS11Backend{Module: "/vendor/lib.so", Slot: ptr.To(int64(0)), KeyLabel: "consensus", KeyID: "01", PINSecret: secretKey("pin", "user-pin")}}
	return p
}

func TestPKCS11Runtime(t *testing.T) {
	p := pkcs11Params()
	config, err := p.ConfigYAML()
	require.NoError(t, err)
	require.Contains(t, config, "type: pkcs11")
	require.Contains(t, config, "slot: 0")
	require.NotContains(t, config, "token_label:")
	require.Contains(t, config, "module: /vendor/lib.so")
	require.Contains(t, config, "key_label: consensus")
	require.Contains(t, config, `key_id: "01"`)
	require.Contains(t, config, "pin_file: /pkcs11/pin/pin")
	require.Contains(t, config, "binding_file: /data/cluster-binding.json")
	sts := mustStatefulSet(t, p)
	require.Equal(t, "true", signerEnv(t, p)["COSMOSIGNER_CLAIM_IF_UNCLAIMED"])
	require.Contains(t, sts.Spec.Template.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{Name: "data", MountPath: "/data"})
	require.Len(t, sts.Spec.VolumeClaimTemplates, 1)
	found := false
	for _, v := range sts.Spec.Template.Spec.Volumes {
		if v.Name == "pkcs11-pin" {
			found = true
			require.Equal(t, "pin", v.Secret.SecretName)
			require.Equal(t, "user-pin", v.Secret.Items[0].Key)
			require.Equal(t, "pin", v.Secret.Items[0].Path)
		}
	}
	require.True(t, found)
	for _, m := range sts.Spec.Template.Spec.Containers[0].VolumeMounts {
		if m.Name == "pkcs11-pin" {
			require.Equal(t, "/pkcs11/pin", m.MountPath)
			require.True(t, m.ReadOnly)
			require.Empty(t, m.SubPath)
		}
	}
	for _, c := range p.Backend.volumeMounts() {
		require.NotEqual(t, "/data", c.MountPath)
	}
}

func TestPKCS11RecoveredSigningPublicKey(t *testing.T) {
	owner := fakeOwner("owner", types.UID("owner-uid"))
	p := pkcs11Params()
	config, err := p.ConfigYAML()
	require.NoError(t, err)
	cm, err := p.ConfigMap(config)
	require.NoError(t, err)
	sts := mustStatefulSet(t, p)
	cm.OwnerReferences = []metav1.OwnerReference{ownerRef(owner)}
	sts.OwnerReferences = []metav1.OwnerReference{ownerRef(owner)}
	c := fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(cm, sts).Build()
	key, found, err := RecoveredSigningPublicKey(context.Background(), c, owner, p)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, p.ExpectedPublicKey, key)
	p.Backend.PKCS11.PINSecret = secretKey("other-pin", "pin")
	_, found, err = RecoveredSigningPublicKey(context.Background(), c, owner, p)
	require.NoError(t, err)
	require.True(t, found)
	p.Backend.PKCS11.TokenLabel = "another"
	p.Backend.PKCS11.Slot = nil
	_, _, err = RecoveredSigningPublicKey(context.Background(), c, owner, p)
	require.ErrorIs(t, err, ErrRecoveredIdentityMismatch)
}
