package chainnode

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	k8sappsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	appsv1 "github.com/voluzi/cosmopilot/v5/api/v1"
	"github.com/voluzi/cosmopilot/v5/internal/cometbft"
	"github.com/voluzi/cosmopilot/v5/internal/controllers"
)

func TestChainNodeSignerScheduling(t *testing.T) {
	for _, configured := range []bool{false, true} {
		t.Run(map[bool]string{false: "unset", true: "configured"}[configured], func(t *testing.T) {
			ctx := context.Background()
			node := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: "node", Namespace: "default", UID: "node-uid"}, Spec: appsv1.ChainNodeSpec{
				NodeSelector: map[string]string{"pool": "nodes"}, Affinity: &corev1.Affinity{PodAffinity: &corev1.PodAffinity{}},
				Cosmosigner: &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{Software: &appsv1.CosmosignerSoftwareBackend{PrivateKeySecret: ptr.To("key")}}},
			}, Status: appsv1.ChainNodeStatus{ChainID: "test-1", CosmosignerReplicas: ptr.To(int32(1)), CosmosignerStateStorageSize: "1Gi", CosmosignerValidatorTargeted: ptr.To(false)}}
			if configured {
				node.Spec.Cosmosigner.NodeSelector = map[string]string{"pool": "signers"}
				node.Spec.Cosmosigner.Affinity = &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{}}
			}
			key, err := cometbft.GeneratePrivKey()
			require.NoError(t, err)
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "key", Namespace: node.Namespace}, Data: map[string][]byte{PrivKeyFilename: key}}
			scheme := runtime.NewScheme()
			require.NoError(t, clientgoscheme.AddToScheme(scheme))
			require.NoError(t, appsv1.AddToScheme(scheme))
			cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(node).WithObjects(node, secret).Build()
			r := &Reconciler{Client: cl, Scheme: scheme, opts: &controllers.ControllerRunOptions{}}
			params, err := r.cosmosignerParams(ctx, node)
			require.NoError(t, err)
			params.ExpectedPublicKey, err = r.cosmosignerPublicKey(ctx, node, params)
			require.NoError(t, err)
			_, err = r.ensureCosmosignerWithParams(ctx, node, params)
			require.NoError(t, err)
			sts := &k8sappsv1.StatefulSet{}
			require.NoError(t, r.Get(ctx, client.ObjectKey{Namespace: node.Namespace, Name: params.Name}, sts))
			require.Equal(t, node.Spec.Cosmosigner.NodeSelector, sts.Spec.Template.Spec.NodeSelector)
			require.Equal(t, node.Spec.Cosmosigner.Affinity, sts.Spec.Template.Spec.Affinity)
		})
	}
}
