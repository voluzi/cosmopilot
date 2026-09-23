package chainnode

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	appsv1 "github.com/voluzi/cosmopilot/v3/api/v1"
	"github.com/voluzi/cosmopilot/v3/internal/controllers"
)

func newGenesisRestoreScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	return scheme
}

func downloadedGenesisPVC(name string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: "default",
		Annotations: map[string]string{controllers.AnnotationGenesisDownloaded: controllers.StringValueTrue},
	}}
}

// TestEnsureGenesisRestoresChainIDWhenMarkerIsPersisted covers the downloaded marker being present
// with an empty status.chainID: the chain ID must be restored from the spec or the marker annotation
// without fetching or rewriting genesis (no ClientSet is configured, so any helper pod would panic).
func TestEnsureGenesisRestoresChainIDWhenMarkerIsPersisted(t *testing.T) {
	for _, tc := range []struct {
		name        string
		genesis     *appsv1.GenesisConfig
		annotations map[string]string
		want        string
		wantErr     string
	}{
		{
			name:    "container download uses spec chainID",
			genesis: &appsv1.GenesisConfig{Url: ptr.To("https://example.invalid/genesis.json"), ChainID: ptr.To("chain-1"), UseDataVolume: ptr.To(true)},
			want:    "chain-1",
		},
		{
			name:        "operator download uses the recorded chain ID",
			genesis:     &appsv1.GenesisConfig{Url: ptr.To("https://example.invalid/genesis.json"), UseDataVolume: ptr.To(true)},
			annotations: map[string]string{controllers.AnnotationGenesisChainID: "chain-from-volume"},
			want:        "chain-from-volume",
		},
		{
			name:    "marker without a recorded chain ID fails closed",
			genesis: &appsv1.GenesisConfig{Url: ptr.To("https://example.invalid/genesis.json"), UseDataVolume: ptr.To(true)},
			wantErr: "has no recorded chain ID",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chainNode := &appsv1.ChainNode{
				ObjectMeta: metav1.ObjectMeta{Name: "node", Namespace: "default"},
				Spec:       appsv1.ChainNodeSpec{Genesis: tc.genesis},
			}
			pvc := downloadedGenesisPVC(chainNode.Name)
			for k, v := range tc.annotations {
				pvc.Annotations[k] = v
			}
			cl := fake.NewClientBuilder().WithScheme(newGenesisRestoreScheme(t)).WithStatusSubresource(chainNode).
				WithObjects(chainNode, pvc).Build()
			r := &Reconciler{Client: cl, opts: &controllers.ControllerRunOptions{}}

			current := &appsv1.ChainNode{}
			require.NoError(t, cl.Get(t.Context(), client.ObjectKeyFromObject(chainNode), current))
			err := r.ensureGenesis(t.Context(), nil, current)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				return
			}
			require.NoError(t, err)

			persisted := &appsv1.ChainNode{}
			require.NoError(t, cl.Get(t.Context(), client.ObjectKeyFromObject(chainNode), persisted))
			require.Equal(t, tc.want, persisted.Status.ChainID)
		})
	}
}

func TestMarkGenesisOnVolumeRecordsChainIDWithMarker(t *testing.T) {
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "node", Namespace: "default"}}
	cl := fake.NewClientBuilder().WithScheme(newGenesisRestoreScheme(t)).WithObjects(pvc).Build()
	r := &Reconciler{Client: cl}

	require.NoError(t, r.markGenesisOnVolume(t.Context(), pvc, "chain-1"))

	persisted := &corev1.PersistentVolumeClaim{}
	require.NoError(t, cl.Get(t.Context(), client.ObjectKeyFromObject(pvc), persisted))
	require.Equal(t, controllers.StringValueTrue, persisted.Annotations[controllers.AnnotationGenesisDownloaded])
	require.Equal(t, "chain-1", persisted.Annotations[controllers.AnnotationGenesisChainID])
}

// TestEnsureGenesisRecoversFromStatusWriteFailureAfterDownload fails the status write that follows the
// PVC marker update, then reconciles again and expects the chain ID to be established.
func TestEnsureGenesisRecoversFromStatusWriteFailureAfterDownload(t *testing.T) {
	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "node", Namespace: "default"},
		Spec: appsv1.ChainNodeSpec{Genesis: &appsv1.GenesisConfig{
			Url: ptr.To("https://example.invalid/genesis.json"), ChainID: ptr.To("chain-1"), UseDataVolume: ptr.To(true),
		}},
	}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: chainNode.Name, Namespace: chainNode.Namespace}}
	failStatus := true
	cl := fake.NewClientBuilder().WithScheme(newGenesisRestoreScheme(t)).WithStatusSubresource(chainNode).
		WithObjects(chainNode, pvc).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if failStatus {
					return errors.New("injected status write failure")
				}
				return c.SubResource(sub).Update(ctx, obj, opts...)
			},
		}).Build()
	r := &Reconciler{
		Client:     cl,
		ClientSet:  newGenesisDownloadClientset(t, corev1.PodSucceeded, false, func(*corev1.Pod) {}),
		RestConfig: &rest.Config{Host: "https://kubernetes.invalid", ContentConfig: rest.ContentConfig{ContentType: "application/json"}},
		Scheme:     newGenesisRestoreScheme(t),
		opts:       &controllers.ControllerRunOptions{},
	}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	current := &appsv1.ChainNode{}
	require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(chainNode), current))
	require.ErrorContains(t, r.ensureGenesis(ctx, nil, current), "injected status write failure")

	persistedPVC := &corev1.PersistentVolumeClaim{}
	require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(pvc), persistedPVC))
	require.Equal(t, controllers.StringValueTrue, persistedPVC.Annotations[controllers.AnnotationGenesisDownloaded])

	failStatus = false
	current = &appsv1.ChainNode{}
	require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(chainNode), current))
	require.Empty(t, current.Status.ChainID)
	require.NoError(t, r.ensureGenesis(ctx, nil, current))

	persisted := &appsv1.ChainNode{}
	require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(chainNode), persisted))
	require.Equal(t, "chain-1", persisted.Status.ChainID)
}

func TestEnsureValidatorConsensusKeyReservationFailsClosedWithoutChainID(t *testing.T) {
	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "validator", Namespace: "default", UID: "uid"},
		Spec:       appsv1.ChainNodeSpec{Validator: &appsv1.ValidatorConfig{}},
	}
	r := &Reconciler{Client: fake.NewClientBuilder().WithScheme(newGenesisRestoreScheme(t)).Build()}

	recorded, err := r.ensureValidatorConsensusKeyReservation(t.Context(), chainNode)
	require.ErrorContains(t, err, "chain ID is not established")
	require.False(t, recorded)
}
