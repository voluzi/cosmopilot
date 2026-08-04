package resourcecleanup

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
)

func TestRootOwnerForChainNodeSetChild(t *testing.T) {
	child := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{
		Name: "set-fullnodes-0", Namespace: "default", UID: types.UID("child-uid"),
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNodeSet", Name: "set",
			UID: types.UID("root-uid"), Controller: ptr.To(true),
		}},
	}}

	assert.Equal(t, RootOwner{
		APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNodeSet", Name: "set",
		Namespace: "default", UID: types.UID("root-uid"),
	}, RootOwnerFor(child))
}

func TestFinalizeClassRetainRemovesOnlyMatchingControllerOwnerReference(t *testing.T) {
	scheme := cleanupScheme(t)
	root := RootOwner{APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNode", Name: "node", Namespace: "default", UID: "root-uid"}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: "node-priv-key", Namespace: "default", UID: "secret-uid",
		OwnerReferences: []metav1.OwnerReference{
			{APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNode", Name: "node", UID: "root-uid", Controller: ptr.To(true)},
			{APIVersion: "v1", Kind: "ConfigMap", Name: "audit", UID: "audit-uid"},
		},
	}}
	Stamp(secret, root, ClassGeneratedKeys)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()

	done, err := FinalizeClass(context.Background(), c, root, ClassGeneratedKeys, appsv1.DeletionPolicyRetain, root.UID)
	require.NoError(t, err)
	assert.True(t, done)

	retained := &corev1.Secret{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(secret), retained))
	assert.Equal(t, []metav1.OwnerReference{{APIVersion: "v1", Kind: "ConfigMap", Name: "audit", UID: "audit-uid"}}, retained.OwnerReferences)
	assert.True(t, IsAttributed(retained, root, ClassGeneratedKeys))
}

func TestFinalizeClassDeleteIgnoresUnattributedResources(t *testing.T) {
	scheme := cleanupScheme(t)
	root := RootOwner{APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNode", Name: "node", Namespace: "default", UID: "root-uid"}
	generated := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "node-priv-key", Namespace: "default", UID: "generated-uid"}}
	Stamp(generated, root, ClassGeneratedKeys)
	ambiguous := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "node-account", Namespace: "default", UID: "ambiguous-uid"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(generated, ambiguous).Build()

	done, err := FinalizeClass(context.Background(), c, root, ClassGeneratedKeys, appsv1.DeletionPolicyDelete, root.UID)
	require.NoError(t, err)
	assert.True(t, done)
	assert.True(t, apierrors.IsNotFound(c.Get(context.Background(), client.ObjectKeyFromObject(generated), &corev1.Secret{})))
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(ambiguous), &corev1.Secret{}))
}

func TestFinalizeClassDeleteUsesUIDPrecondition(t *testing.T) {
	scheme := cleanupScheme(t)
	root := RootOwner{APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNode", Name: "node", Namespace: "default", UID: "root-uid"}
	original := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "node-priv-key", Namespace: "default", UID: "original-uid"}}
	Stamp(original, root, ClassGeneratedKeys)
	replacement := original.DeepCopy()
	replacement.UID = "replacement-uid"
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(original).Build()
	c := &replaceBeforeDeleteClient{Client: base, replacement: replacement}

	_, err := FinalizeClass(context.Background(), c, root, ClassGeneratedKeys, appsv1.DeletionPolicyDelete, root.UID)
	require.Error(t, err)
	assert.Equal(t, types.UID("original-uid"), c.deleteUID)

	remaining := &corev1.Secret{}
	require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(replacement), remaining))
	assert.Equal(t, types.UID("replacement-uid"), remaining.UID)
}

func TestFinalizeClassDeleteWaitsWhenSameNameResourceIsRecreated(t *testing.T) {
	scheme := cleanupScheme(t)
	root := RootOwner{APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNode", Name: "node", Namespace: "default", UID: "root-uid"}
	original := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "node-priv-key", Namespace: "default", UID: "original-uid"}}
	Stamp(original, root, ClassGeneratedKeys)
	replacement := original.DeepCopy()
	replacement.UID = "replacement-uid"
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(original).Build()
	c := &replaceAfterDeleteClient{Client: base, replacement: replacement}

	done, err := FinalizeClass(context.Background(), c, root, ClassGeneratedKeys, appsv1.DeletionPolicyDelete, root.UID)
	require.NoError(t, err)
	assert.False(t, done)
	assert.Equal(t, types.UID("original-uid"), c.deleteUID)

	remaining := &corev1.Secret{}
	require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(replacement), remaining))
	assert.Equal(t, types.UID("replacement-uid"), remaining.UID)
}

func TestPrepareGeneratedResourceDoesNotAdoptAmbiguousLegacyObject(t *testing.T) {
	scheme := cleanupScheme(t)
	owner := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: "node", Namespace: "default", UID: "node-uid"}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "node-priv-key", Namespace: "default"}}

	managed, changed, err := PrepareGeneratedResource(secret, owner, scheme, ClassGeneratedKeys, false)
	require.NoError(t, err)
	assert.False(t, managed)
	assert.False(t, changed)
	assert.Empty(t, secret.Annotations)
	assert.Empty(t, secret.OwnerReferences)
}

func TestIsLegacyGeneratedKeySecret(t *testing.T) {
	for _, tc := range []struct {
		name       string
		secret     *corev1.Secret
		knownNames []string
		want       bool
	}{
		{name: "known deterministic name", secret: &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "node-key"}}, knownNames: []string{"node-key"}, want: true},
		{name: "mnemonic data", secret: &corev1.Secret{Data: map[string][]byte{"mnemonic": []byte("words")}}, want: true},
		{name: "consensus key data", secret: &corev1.Secret{Data: map[string][]byte{"priv_validator_key.json": []byte("key")}}, want: true},
		{name: "operational secret", secret: &corev1.Secret{Data: map[string][]byte{"encryptionKey": []byte("key")}}, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, IsLegacyGeneratedKeySecret(tc.secret, tc.knownNames...))
		})
	}
}

func TestPrepareGeneratedResourceStampsNewObject(t *testing.T) {
	scheme := cleanupScheme(t)
	owner := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: "node", Namespace: "default", UID: "node-uid"}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "node-priv-key", Namespace: "default"}}

	managed, changed, err := PrepareGeneratedResource(secret, owner, scheme, ClassGeneratedKeys, true)
	require.NoError(t, err)
	assert.True(t, managed)
	assert.True(t, changed)
	assert.True(t, IsAttributed(secret, RootOwnerFor(owner), ClassGeneratedKeys))
	assert.True(t, metav1.IsControlledBy(secret, owner))
}

func TestPrepareGeneratedResourceReattachesRetainedChildResource(t *testing.T) {
	scheme := cleanupScheme(t)
	parentRef := metav1.OwnerReference{
		APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNodeSet", Name: "set", UID: "set-uid", Controller: ptr.To(true),
	}
	oldChild := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: "set-fullnodes-0", Namespace: "default", UID: "old-child", OwnerReferences: []metav1.OwnerReference{parentRef}}}
	newChild := oldChild.DeepCopy()
	newChild.UID = "new-child"
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "set-fullnodes-0", Namespace: "default"}}
	Stamp(secret, RootOwnerFor(oldChild), ClassGeneratedKeys)

	managed, changed, err := PrepareGeneratedResource(secret, newChild, scheme, ClassGeneratedKeys, false)
	require.NoError(t, err)
	assert.True(t, managed)
	assert.True(t, changed)
	assert.True(t, metav1.IsControlledBy(secret, newChild))
	assert.Equal(t, types.UID("new-child"), metav1.GetControllerOf(secret).UID)
}

func cleanupScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, appsv1.AddToScheme(scheme))
	return scheme
}

type replaceBeforeDeleteClient struct {
	client.Client
	replacement *corev1.Secret
	deleteUID   types.UID
}

type replaceAfterDeleteClient struct {
	client.Client
	replacement *corev1.Secret
	deleteUID   types.UID
}

func (c *replaceAfterDeleteClient) Delete(ctx context.Context, object client.Object, opts ...client.DeleteOption) error {
	secret, ok := object.(*corev1.Secret)
	if !ok {
		return c.Client.Delete(ctx, object, opts...)
	}
	deleteOptions := (&client.DeleteOptions{}).ApplyOptions(opts)
	if deleteOptions.Preconditions != nil && deleteOptions.Preconditions.UID != nil {
		c.deleteUID = *deleteOptions.Preconditions.UID
	}
	if err := c.Client.Delete(ctx, secret); err != nil {
		return err
	}
	return c.Client.Create(ctx, c.replacement.DeepCopy())
}

func (c *replaceBeforeDeleteClient) Delete(ctx context.Context, object client.Object, opts ...client.DeleteOption) error {
	secret, ok := object.(*corev1.Secret)
	if !ok {
		return c.Client.Delete(ctx, object, opts...)
	}
	deleteOptions := (&client.DeleteOptions{}).ApplyOptions(opts)
	if deleteOptions.Preconditions != nil && deleteOptions.Preconditions.UID != nil {
		c.deleteUID = *deleteOptions.Preconditions.UID
	}
	if err := c.Client.Delete(ctx, secret); err != nil {
		return err
	}
	if err := c.Client.Create(ctx, c.replacement.DeepCopy()); err != nil {
		return err
	}
	return apierrors.NewConflict(schema.GroupResource{Resource: "secrets"}, secret.Name, errors.New("UID precondition did not match"))
}
