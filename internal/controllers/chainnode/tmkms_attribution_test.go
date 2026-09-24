package chainnode

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	appsv1 "github.com/voluzi/cosmopilot/v4/api/v1"
	"github.com/voluzi/cosmopilot/v4/internal/tmkms"
)

func TestTmkmsAttributionSurvivesRootRecreation(t *testing.T) {
	original := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: "val", Namespace: "default", UID: "uid-1"}}
	recreated := original.DeepCopy()
	recreated.UID = "uid-2"
	secret := &corev1.Secret{}

	require.True(t, newTmkmsAttribution(original).Stamp(secret, tmkms.ClassIdentity))
	stamped, owned := newTmkmsAttribution(recreated).Attributed(secret, tmkms.ClassIdentity)
	require.True(t, stamped)
	require.True(t, owned, "a root recreated under the same name keeps its TmKMS identity")

	_, owned = newTmkmsAttribution(recreated).Attributed(secret, tmkms.ClassState)
	require.False(t, owned, "attribution is per class")

	other := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "default", UID: "uid-3"}}
	_, owned = newTmkmsAttribution(other).Attributed(secret, tmkms.ClassIdentity)
	require.False(t, owned)

	stamped, _ = newTmkmsAttribution(original).Attributed(&corev1.Secret{}, tmkms.ClassIdentity)
	require.False(t, stamped)
}

func TestTmkmsAttributionUsesChainNodeSetRoot(t *testing.T) {
	child := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{
		Name: "set-validator-0", Namespace: "default", UID: "child-uid",
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNodeSet", Name: "set", UID: "set-uid", Controller: ptr.To(true),
		}},
	}}
	recreatedChild := child.DeepCopy()
	recreatedChild.UID = "child-uid-2"
	pvc := &corev1.PersistentVolumeClaim{}

	newTmkmsAttribution(child).Stamp(pvc, tmkms.ClassState)
	_, owned := newTmkmsAttribution(recreatedChild).Attributed(pvc, tmkms.ClassState)
	require.True(t, owned, "a recreated ChainNodeSet child keeps its signing state")

	standalone := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: "set-validator-0", Namespace: "default", UID: "x"}}
	_, owned = newTmkmsAttribution(standalone).Attributed(pvc, tmkms.ClassState)
	require.False(t, owned, "a standalone ChainNode does not claim state attributed to a ChainNodeSet")
}
