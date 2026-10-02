package chainnode

import (
	"context"
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
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	appsv1 "github.com/voluzi/cosmopilot/v5/api/v1"
	"github.com/voluzi/cosmopilot/v5/internal/controllers"
	"github.com/voluzi/cosmopilot/v5/internal/cosmosigner"
	"github.com/voluzi/cosmopilot/v5/internal/resourcecleanup"
)

func TestReconcileRefusesLegacyTmKMS(t *testing.T) {
	for _, tc := range []struct {
		name               string
		pod, config, owned bool
		secret             bool
		secretForeign      bool
		secretLegacy       bool
		secretUnrelated    bool
		finalized          bool
		cacheMissing       bool
		configOwner        *metav1.OwnerReference
		artifact           string
	}{
		{name: "pod", pod: true, artifact: "tmkms container in Pod default/validator"},
		{name: "owned config without pod", config: true, owned: true, artifact: "owned ConfigMap default/validator-tmkms"},
		{name: "refused before key generation", pod: true, finalized: true, artifact: "tmkms container in Pod default/validator"},
		{name: "unowned config", config: true},
		{name: "pod absent from cache", pod: true, cacheMissing: true, artifact: "tmkms container in Pod default/validator"},
		{name: "config absent from cache", config: true, owned: true, cacheMissing: true, artifact: "owned ConfigMap default/validator-tmkms"},
		{name: "predecessor config", config: true, configOwner: &metav1.OwnerReference{APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNode", Name: "validator", UID: "old-node-uid", Controller: ptr.To(true)}, artifact: "owned ConfigMap default/validator-tmkms"},
		{name: "other api version in same group", config: true, configOwner: &metav1.OwnerReference{APIVersion: appsv1.GroupVersion.Group + "/v1alpha1", Kind: "ChainNode", Name: "validator", UID: "old-node-uid", Controller: ptr.To(true)}, artifact: "owned ConfigMap default/validator-tmkms"},
		{name: "foreign group", config: true, configOwner: &metav1.OwnerReference{APIVersion: "other.example/v1", Kind: "ChainNode", Name: "validator", UID: "node-uid", Controller: ptr.To(true)}},
		{name: "foreign kind", config: true, configOwner: &metav1.OwnerReference{APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNodeSet", Name: "validator", UID: "node-uid", Controller: ptr.To(true)}},
		{name: "foreign name", config: true, configOwner: &metav1.OwnerReference{APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNode", Name: "other", UID: "node-uid", Controller: ptr.To(true)}},
		{name: "non-controller reference", config: true, configOwner: &metav1.OwnerReference{APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNode", Name: "validator", UID: "node-uid"}},
		{name: "identity secret after predecessor garbage collection", secret: true, artifact: "identity Secret default/validator-tmkms"},
		{name: "identity secret absent from cache", secret: true, cacheMissing: true, artifact: "identity Secret default/validator-tmkms"},
		{name: "unstamped legacy identity secret", secret: true, secretLegacy: true, artifact: "identity Secret default/validator-tmkms"},
		{name: "unrelated same-name secret", secret: true, secretLegacy: true, secretUnrelated: true},
		{name: "foreign identity secret", secret: true, secretForeign: true},
		{name: "migrated"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			scheme := resourceCleanupScheme(t)
			node := &appsv1.ChainNode{
				ObjectMeta: metav1.ObjectMeta{Name: "validator", Namespace: "default", UID: "node-uid"},
				Spec: appsv1.ChainNodeSpec{
					App:       appsv1.AppSpec{Image: "image", App: "appd"},
					Validator: &appsv1.ValidatorConfig{Init: &appsv1.GenesisInitConfig{}},
				},
			}
			if tc.finalized {
				node.Finalizers = []string{resourcecleanup.Finalizer, cosmosigner.ReservationOwnerFinalizer}
			}
			owner := []metav1.OwnerReference{{APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNode", Name: node.Name, UID: node.UID, Controller: ptr.To(true)}}
			objects := []client.Object{node, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: node.Namespace}}}
			if tc.finalized {
				objects = append(objects, &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: node.Name + "-init-data", Namespace: node.Namespace},
					Status:     corev1.PodStatus{Phase: corev1.PodRunning},
				})
			}
			if tc.pod {
				objects = append(objects, &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: node.Name, Namespace: node.Namespace, OwnerReferences: owner},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}, {Name: "tmkms"}}},
				})
			}
			if tc.config {
				cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: node.Name + "-tmkms", Namespace: node.Namespace}}
				if tc.owned {
					cm.OwnerReferences = owner
				}
				if tc.configOwner != nil {
					cm.OwnerReferences = []metav1.OwnerReference{*tc.configOwner}
				}
				objects = append(objects, cm)
			}
			if tc.secret {
				secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: node.Name + "-tmkms", Namespace: node.Namespace}, Immutable: ptr.To(true), Data: map[string][]byte{"kms-identity.key": []byte("identity")}}
				resourcecleanup.Stamp(secret, resourcecleanup.RootOwnerFor(node), "tmkmsIdentity")
				secret.Annotations[resourcecleanup.AnnotationRootOwnerUID] = "old-node-uid"
				if tc.secretLegacy {
					secret.Annotations = nil
				}
				if tc.secretUnrelated {
					secret.Data = map[string][]byte{"unrelated": []byte("value")}
				}
				if tc.secretForeign {
					secret.Annotations[resourcecleanup.AnnotationRootOwnerName] = "other"
				}
				objects = append(objects, secret)
			}
			recorder := record.NewFakeRecorder(10)
			backing := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(node).WithObjects(objects...).Build()
			r := &Reconciler{
				Client: backing, APIReader: backing,
				Scheme: scheme, recorder: recorder, opts: &controllers.ControllerRunOptions{},
			}
			if tc.cacheMissing {
				cache := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects[:2]...).Build()
				r.Client = interceptor.NewClient(backing, interceptor.Funcs{
					Get: func(ctx context.Context, _ client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						return cache.Get(ctx, key, obj, opts...)
					},
					List: func(ctx context.Context, _ client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
						return cache.List(ctx, list, opts...)
					},
				})
			}
			before := &appsv1.ChainNode{}
			require.NoError(t, r.APIReader.Get(ctx, client.ObjectKeyFromObject(node), before))
			_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(node)})
			current := &appsv1.ChainNode{}
			require.NoError(t, r.APIReader.Get(ctx, client.ObjectKeyFromObject(node), current))
			if tc.artifact != "" {
				assert.Equal(t, before.ObjectMeta, current.ObjectMeta)
				assert.Equal(t, before.Status, current.Status, "refusal must leave status untouched")
				secrets := &corev1.SecretList{}
				require.NoError(t, r.APIReader.List(ctx, secrets, client.InNamespace(node.Namespace)))
				if tc.secret {
					assert.Len(t, secrets.Items, 1, "refusal must preserve only the existing identity secret")
				} else {
					assert.Empty(t, secrets.Items, "refusal must not create node or consensus keys")
				}
				require.ErrorContains(t, err, tc.artifact)
				assert.Contains(t, err.Error(), "migrate to cosmosigner on Cosmopilot 4.x")
				switch {
				case tc.pod:
					assert.Contains(t, err.Error(), "Pod must be retired")
					assert.Contains(t, err.Error(), "deleted manually")
					assert.NotContains(t, err.Error(), "delete the stale validator-tmkms ConfigMap")
				case tc.secret:
					assert.Contains(t, err.Error(), "stale validator-tmkms identity Secret")
				default:
					assert.Contains(t, err.Error(), "delete the stale validator-tmkms ConfigMap")
				}
				select {
				case event := <-recorder.Events:
					assert.Contains(t, event, "Warning")
					assert.Contains(t, event, err.Error())
				default:
					t.Fatal("missing warning event")
				}
			} else {
				require.NoError(t, err)
				assert.Contains(t, current.Finalizers, resourcecleanup.Finalizer, "accepted nodes must advance reconciliation")
			}
			if tc.pod {
				remaining := &corev1.Pod{}
				require.NoError(t, r.APIReader.Get(ctx, client.ObjectKeyFromObject(node), remaining), "refusal must preserve the running pod")
				assert.Equal(t, "tmkms", remaining.Spec.Containers[1].Name)
			}
		})
	}
}

func TestReconcileRequiresAuthoritativeLegacySignerReader(t *testing.T) {
	node := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: "validator", Namespace: "default", UID: "node-uid"}}
	c := fake.NewClientBuilder().WithScheme(resourceCleanupScheme(t)).WithObjects(node, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: node.Namespace}}).Build()
	r := &Reconciler{Client: c, opts: &controllers.ControllerRunOptions{}}
	_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(node)})
	require.ErrorContains(t, err, "legacy tmKMS guard requires an authoritative APIReader")
	fresh := &appsv1.ChainNode{}
	require.NoError(t, c.Get(t.Context(), client.ObjectKeyFromObject(node), fresh))
	assert.Empty(t, fresh.Finalizers)
	assert.Equal(t, node.Status, fresh.Status)
}
