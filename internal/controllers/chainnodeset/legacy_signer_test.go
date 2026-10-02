package chainnodeset

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	appsv1 "github.com/voluzi/cosmopilot/v5/api/v1"
	"github.com/voluzi/cosmopilot/v5/internal/controllers"
	"github.com/voluzi/cosmopilot/v5/internal/resourcecleanup"
)

func TestReconcileRefusesLegacyTmKMSChild(t *testing.T) {
	for _, tc := range []struct {
		name     string
		pod      bool
		config   bool
		owned    bool
		artifact string
	}{
		{name: "child pod", pod: true, owned: true, artifact: "tmkms container in Pod default/chain-validator"},
		{name: "child config", config: true, owned: true, artifact: "owned ConfigMap default/chain-validator-tmkms"},
		{name: "unowned child", pod: true},
		{name: "migrated child", owned: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			scheme := nodeSetCleanupScheme(t)
			nodeSet := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{Name: "chain", Namespace: "default", UID: "set-uid"}}
			child := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: "chain-validator", Namespace: nodeSet.Namespace, UID: "child-uid"}}
			if tc.owned {
				child.OwnerReferences = []metav1.OwnerReference{{APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNodeSet", Name: nodeSet.Name, UID: nodeSet.UID, Controller: ptr.To(true)}}
			}
			objects := []client.Object{nodeSet, child, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: nodeSet.Namespace}}}
			if tc.pod {
				objects = append(objects, &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: child.Name, Namespace: child.Namespace},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "tmkms"}}},
				})
			}
			if tc.config {
				objects = append(objects, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{
					Name: child.Name + "-tmkms", Namespace: child.Namespace,
					OwnerReferences: []metav1.OwnerReference{{APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNode", Name: child.Name, UID: child.UID, Controller: ptr.To(true)}},
				}})
			}
			recorder := record.NewFakeRecorder(10)
			r := &Reconciler{
				Client: fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(nodeSet, child).WithObjects(objects...).Build(),
				Scheme: scheme, recorder: recorder, opts: &controllers.ControllerRunOptions{},
			}
			beforeSet := &appsv1.ChainNodeSet{}
			beforeChild := &appsv1.ChainNode{}
			require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(nodeSet), beforeSet))
			require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(child), beforeChild))
			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(nodeSet)})
			currentSet := &appsv1.ChainNodeSet{}
			currentChild := &appsv1.ChainNode{}
			require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(nodeSet), currentSet))
			require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(child), currentChild))
			if tc.artifact != "" {
				assert.Equal(t, beforeSet.ObjectMeta, currentSet.ObjectMeta)
				assert.Equal(t, beforeSet.Status, currentSet.Status)
				assert.Equal(t, beforeChild, currentChild)
				secrets := &corev1.SecretList{}
				require.NoError(t, r.List(ctx, secrets, client.InNamespace(nodeSet.Namespace)))
				assert.Empty(t, secrets.Items)
				require.ErrorContains(t, err, tc.artifact)
				select {
				case event := <-recorder.Events:
					assert.Contains(t, event, "Warning")
					assert.Contains(t, event, err.Error())
				default:
					t.Fatal("missing warning event")
				}
			} else {
				require.NoError(t, err)
				assert.Contains(t, currentSet.Finalizers, resourcecleanup.Finalizer)
			}
		})
	}
}
