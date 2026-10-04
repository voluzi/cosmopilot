package chainnodeset

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	k8sappsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1 "github.com/voluzi/cosmopilot/v5/api/v1"
	"github.com/voluzi/cosmopilot/v5/internal/cometbft"
)

func TestChainNodeSetSignerScheduling(t *testing.T) {
	for _, group := range []bool{false, true} {
		for _, configured := range []bool{false, true} {
			t.Run(map[bool]string{false: "top-level", true: "group"}[group]+"/"+map[bool]string{false: "unset", true: "configured"}[configured], func(t *testing.T) {
				ctx := context.Background()
				c := &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{Software: &appsv1.CosmosignerSoftwareBackend{PrivateKeySecret: ptr.To("key")}}}
				if configured {
					c.NodeSelector = map[string]string{"pool": "signers"}
					c.Affinity = &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{}}
					c.Env = []corev1.EnvVar{{Name: "VENDOR", Value: "config"}}
					c.Volumes = []corev1.Volume{{Name: "vendor", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}}
					c.VolumeMounts = []corev1.VolumeMount{{Name: "vendor", MountPath: "/vendor"}}
				}
				set := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: "default", UID: "set-uid"}, Spec: appsv1.ChainNodeSetSpec{Nodes: []appsv1.NodeGroupSpec{{Name: "sentries", Instances: ptr.To(1), NodeSelector: map[string]string{"pool": "nodes"}, Affinity: &corev1.Affinity{PodAffinity: &corev1.PodAffinity{}}}}}, Status: appsv1.ChainNodeSetStatus{ChainID: "test-1"}}
				if group {
					set.Spec.Nodes[0].Cosmosigner = c
				} else {
					c.NodeGroups = []string{"sentries"}
					set.Spec.Cosmosigner = c
				}
				key, err := cometbft.GeneratePrivKey()
				require.NoError(t, err)
				secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "key", Namespace: set.Namespace}, Data: map[string][]byte{privKeyFilename: key}}
				r := newValidatorTestReconciler(t, set, secret)
				_, err = r.initCosmosignerLocks(ctx, set)
				require.NoError(t, err)
				signer := resolveSingleSigner(t, set)
				params, err := r.cosmosignerParams(ctx, set, signer)
				require.NoError(t, err)
				params.ExpectedPublicKey, err = r.cosmosignerPublicKeyWithParams(ctx, set, signer, params)
				require.NoError(t, err)
				_, err = r.reconcileSigner(ctx, set, signer, params)
				require.NoError(t, err)
				sts := &k8sappsv1.StatefulSet{}
				require.NoError(t, r.Get(ctx, client.ObjectKey{Namespace: set.Namespace, Name: params.Name}, sts))
				require.Equal(t, c.NodeSelector, sts.Spec.Template.Spec.NodeSelector)
				require.Equal(t, c.Affinity, sts.Spec.Template.Spec.Affinity)
				if configured {
					require.Contains(t, sts.Spec.Template.Spec.Containers[0].Env, c.Env[0])
					require.Contains(t, sts.Spec.Template.Spec.Volumes, c.Volumes[0])
					require.Contains(t, sts.Spec.Template.Spec.Containers[0].VolumeMounts, c.VolumeMounts[0])
				}
			})
		}
	}
}
