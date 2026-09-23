package controllers

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"
)

func ownershipScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, gwapiv1.Install(scheme))
	return scheme
}

func ownershipOwners() (*corev1.ConfigMap, *corev1.ConfigMap) {
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "node", Namespace: "default", UID: "owner-uid"}}
	foreign := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: "default", UID: "foreign-uid"}}
	return owner, foreign
}

func serviceControlledBy(t *testing.T, scheme *runtime.Scheme, name string, owner metav1.Object) *corev1.Service {
	t.Helper()
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID(name + "-uid")}}
	if owner != nil {
		require.NoError(t, controllerutil.SetControllerReference(owner, svc, scheme))
	}
	return svc
}

func TestDeleteIfControlledBy(t *testing.T) {
	owner, foreign := ownershipOwners()
	cases := []struct {
		name        string
		controller  metav1.Object
		wantDeleted bool
	}{
		{name: "owned", controller: owner, wantDeleted: true},
		{name: "foreign", controller: foreign},
		{name: "unowned"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scheme := ownershipScheme(t)
			c := fake.NewClientBuilder().WithScheme(scheme).
				WithObjects(serviceControlledBy(t, scheme, "node-p2p", tc.controller)).Build()

			key := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "node-p2p", Namespace: "default"}}
			deleted, err := DeleteIfControlledBy(context.Background(), c, key, owner)
			require.NoError(t, err)
			require.Equal(t, tc.wantDeleted, deleted)

			err = c.Get(context.Background(), client.ObjectKey{Name: "node-p2p", Namespace: "default"}, &corev1.Service{})
			if tc.wantDeleted {
				require.True(t, errors.IsNotFound(err), "owned object must be deleted, got %v", err)
			} else {
				require.NoError(t, err, "object controlled by someone else must be kept")
			}
		})
	}
}

func TestDeleteIfControlledByMissingObject(t *testing.T) {
	owner, _ := ownershipOwners()
	c := fake.NewClientBuilder().WithScheme(ownershipScheme(t)).Build()

	deleted, err := DeleteIfControlledBy(context.Background(), c,
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "node-p2p", Namespace: "default"}}, owner)
	require.NoError(t, err)
	require.False(t, deleted)
}

func TestDeleteControlledObjectSendsUIDPrecondition(t *testing.T) {
	owner, _ := ownershipOwners()
	scheme := ownershipScheme(t)
	svc := serviceControlledBy(t, scheme, "node-p2p", owner)
	var gotUID *types.UID
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(svc).
		WithInterceptorFuncs(interceptor.Funcs{
			Delete: func(ctx context.Context, cl client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				options := &client.DeleteOptions{}
				options.ApplyOptions(opts)
				if options.Preconditions != nil {
					gotUID = options.Preconditions.UID
				}
				return cl.Delete(ctx, obj, opts...)
			},
		}).Build()

	live := &corev1.Service{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(svc), live))
	deleted, err := DeleteControlledObject(context.Background(), c, live, owner)
	require.NoError(t, err)
	require.True(t, deleted)
	require.NotNil(t, gotUID, "delete must carry a UID precondition")
	require.Equal(t, live.GetUID(), *gotUID)
}

func TestDeleteControlledObjectIgnoresSameNameReplacement(t *testing.T) {
	owner, _ := ownershipOwners()
	scheme := ownershipScheme(t)
	svc := serviceControlledBy(t, scheme, "node-p2p", owner)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(svc).
		WithInterceptorFuncs(interceptor.Funcs{
			// The apiserver answers 409 when the UID precondition no longer matches the live object.
			Delete: func(context.Context, client.WithWatch, client.Object, ...client.DeleteOption) error {
				return errors.NewConflict(schema.GroupResource{Resource: "services"}, "node-p2p", nil)
			},
		}).Build()

	deleted, err := DeleteControlledObject(context.Background(), c, svc, owner)
	require.NoError(t, err)
	require.False(t, deleted)
}

func TestRequireSameController(t *testing.T) {
	owner, foreign := ownershipOwners()
	scheme := ownershipScheme(t)
	desired := serviceControlledBy(t, scheme, "node", owner)

	require.NoError(t, RequireSameController(serviceControlledBy(t, scheme, "node", owner), desired, "Service"))
	require.ErrorContains(t, RequireSameController(serviceControlledBy(t, scheme, "node", foreign), desired, "Service"),
		"managed by another owner")
	require.ErrorContains(t, RequireSameController(serviceControlledBy(t, scheme, "node", nil), desired, "Service"),
		"managed by another owner")
	require.ErrorContains(t, RequireSameController(desired, serviceControlledBy(t, scheme, "node", nil), "Service"),
		"has no controller owner")
}

func TestEnsureHTTPRouteRefusesForeignRoute(t *testing.T) {
	owner, foreign := ownershipOwners()
	scheme := ownershipScheme(t)
	current := &gwapiv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{
		Name: "node-api", Namespace: "default", Labels: map[string]string{"app": "foreign"},
	}}
	require.NoError(t, controllerutil.SetControllerReference(foreign, current, scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(current).Build()

	desired := &gwapiv1.HTTPRoute{ObjectMeta: metav1.ObjectMeta{
		Name: "node-api", Namespace: "default", Labels: map[string]string{"app": "node"},
	}}
	require.NoError(t, controllerutil.SetControllerReference(owner, desired, scheme))

	_, err := EnsureHTTPRoute(context.Background(), c, desired)
	require.ErrorContains(t, err, "managed by another owner")

	live := &gwapiv1.HTTPRoute{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(current), live))
	require.Equal(t, "foreign", live.Labels["app"])
	require.Equal(t, types.UID("foreign-uid"), metav1.GetControllerOf(live).UID)
}
