package chainnode

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	k8sappsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
	"github.com/voluzi/cosmopilot/v2/internal/resourcecleanup"
)

func TestReconcileInstallsResourceCleanupFinalizerBeforeDurableCreation(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, k8sappsv1.AddToScheme(scheme))
	node := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: "node", Namespace: "default", UID: "node-uid"}}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node, namespace).Build()
	r := &Reconciler{Client: c, Scheme: scheme, opts: &controllers.ControllerRunOptions{}}

	result, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(node)})
	require.NoError(t, err)
	assert.True(t, result.Requeue)

	current := &appsv1.ChainNode{}
	require.NoError(t, c.Get(context.Background(), client.ObjectKeyFromObject(node), current))
	assert.Contains(t, current.Finalizers, resourcecleanup.Finalizer)
	secrets := &corev1.SecretList{}
	require.NoError(t, c.List(context.Background(), secrets, client.InNamespace("default")))
	assert.Empty(t, secrets.Items)
}
