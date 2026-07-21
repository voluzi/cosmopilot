package chainnode

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
)

func TestEnsureServiceRefusesForeignController(t *testing.T) {
	r, owner, foreign := ownershipTestReconciler(t)
	current := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: owner.Name, Namespace: owner.Namespace}}
	desired := current.DeepCopy()
	require.NoError(t, controllerutil.SetControllerReference(foreign, current, r.Scheme))
	require.NoError(t, controllerutil.SetControllerReference(owner, desired, r.Scheme))
	require.NoError(t, r.Create(context.Background(), current))

	err := r.ensureService(context.Background(), desired)
	require.Error(t, err)
	require.Contains(t, err.Error(), "managed by another owner")
}

func TestEnsureConfigMapRefusesForeignController(t *testing.T) {
	r, owner, foreign := ownershipTestReconciler(t)
	current := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: owner.Name, Namespace: owner.Namespace}}
	desired := current.DeepCopy()
	require.NoError(t, controllerutil.SetControllerReference(foreign, current, r.Scheme))
	require.NoError(t, controllerutil.SetControllerReference(owner, desired, r.Scheme))
	require.NoError(t, r.Create(context.Background(), current))

	err := r.ensureConfigMap(context.Background(), desired)
	require.Error(t, err)
	require.Contains(t, err.Error(), "managed by another owner")
}

func ownershipTestReconciler(t *testing.T) (*Reconciler, *appsv1.ChainNode, *appsv1.ChainNode) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	owner := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: "foo-signer", Namespace: "default", UID: "owner-uid"}}
	foreign := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "default", UID: "foreign-uid"}}
	return &Reconciler{Client: fake.NewClientBuilder().WithScheme(scheme).Build(), Scheme: scheme}, owner, foreign
}
