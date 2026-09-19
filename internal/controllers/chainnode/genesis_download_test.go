package chainnode

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	appsv1 "github.com/voluzi/cosmopilot/v3/api/v1"
	"github.com/voluzi/cosmopilot/v3/internal/controllers"
)

func TestGetGenesisContainerDownloadPropagatesDigestAndGatesSuccessState(t *testing.T) {
	const digest = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

	for _, tt := range []struct {
		name          string
		genesisSHA    *string
		podPhase      corev1.PodPhase
		existingPod   bool
		wantVerifySHA string
		wantDigest    string
		wantErr       bool
		wantSuccess   bool
	}{
		{name: "successful pod", genesisSHA: ptr.To(digest), podPhase: corev1.PodSucceeded, wantVerifySHA: "1", wantDigest: digest, wantSuccess: true},
		{name: "failed pod", genesisSHA: ptr.To(digest), podPhase: corev1.PodFailed, wantVerifySHA: "1", wantDigest: digest, wantErr: true},
		{name: "nil digest after deleting existing pod", podPhase: corev1.PodSucceeded, existingPod: true, wantVerifySHA: "0", wantSuccess: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, appsv1.AddToScheme(scheme))
			require.NoError(t, corev1.AddToScheme(scheme))

			chainNode := &appsv1.ChainNode{
				ObjectMeta: metav1.ObjectMeta{Name: "node", Namespace: "default"},
				Spec: appsv1.ChainNodeSpec{Genesis: &appsv1.GenesisConfig{
					Url:           ptr.To("https://example.invalid/genesis.json.zst"),
					GenesisSHA:    tt.genesisSHA,
					UseDataVolume: ptr.To(true),
					ChainID:       ptr.To("chain-1"),
				}},
			}
			pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: chainNode.Name, Namespace: chainNode.Namespace}}
			backing := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(chainNode).WithObjects(chainNode, pvc).Build()

			var capturedMu sync.Mutex
			var captured *corev1.Pod
			clientSet := newGenesisDownloadClientset(t, tt.podPhase, tt.existingPod, func(pod *corev1.Pod) {
				capturedMu.Lock()
				defer capturedMu.Unlock()
				captured = pod.DeepCopy()
			})
			r := &Reconciler{
				Client:     backing,
				ClientSet:  clientSet,
				RestConfig: &rest.Config{Host: "https://kubernetes.invalid", ContentConfig: rest.ContentConfig{ContentType: "application/json"}},
				Scheme:     scheme,
				opts:       &controllers.ControllerRunOptions{},
			}

			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			err := r.getGenesis(ctx, nil, chainNode)
			if tt.wantErr {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}

			capturedMu.Lock()
			require.NotNil(t, captured)
			args := append([]string(nil), captured.Spec.Containers[0].Args...)
			capturedMu.Unlock()
			require.GreaterOrEqual(t, len(args), 8)
			assert.Equal(t, tt.wantVerifySHA, args[6])
			assert.Equal(t, tt.wantDigest, args[7])

			persistedPVC := &corev1.PersistentVolumeClaim{}
			require.NoError(t, backing.Get(t.Context(), client.ObjectKeyFromObject(pvc), persistedPVC))
			persistedNode := &appsv1.ChainNode{}
			require.NoError(t, backing.Get(t.Context(), client.ObjectKeyFromObject(chainNode), persistedNode))
			if tt.wantSuccess {
				assert.Equal(t, controllers.StringValueTrue, persistedPVC.Annotations[controllers.AnnotationGenesisDownloaded])
				assert.Equal(t, "chain-1", persistedNode.Status.ChainID)
			} else {
				assert.Empty(t, persistedPVC.Annotations[controllers.AnnotationGenesisDownloaded])
				assert.Empty(t, persistedNode.Status.ChainID)
			}
		})
	}
}

func newGenesisDownloadClientset(t *testing.T, phase corev1.PodPhase, existingPod bool, capture func(*corev1.Pod)) *kubernetes.Clientset {
	t.Helper()
	pod := corev1.Pod{
		TypeMeta:   metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{Name: "node-download-genesis", Namespace: "default", ResourceVersion: "1"},
		Status:     corev1.PodStatus{Phase: phase},
	}
	var stateMu sync.Mutex
	previousPodExists := existingPod
	deleteRequested := false
	podCreated := false
	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		status := http.StatusOK
		body := `{"kind":"Status","apiVersion":"v1","status":"Success"}`
		switch {
		case req.Method == http.MethodDelete:
			stateMu.Lock()
			deleteRequested = previousPodExists
			stateMu.Unlock()
		case req.Method == http.MethodPost:
			stateMu.Lock()
			previousExists := previousPodExists
			stateMu.Unlock()
			if previousExists {
				status = http.StatusConflict
				body = `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"AlreadyExists","code":409}`
				break
			}
			createdPod := &corev1.Pod{}
			if err := json.NewDecoder(req.Body).Decode(createdPod); err != nil {
				return nil, err
			}
			capture(createdPod)
			createdPod.Status.Phase = corev1.PodPending
			stateMu.Lock()
			podCreated = true
			stateMu.Unlock()
			encoded, err := json.Marshal(createdPod)
			if err != nil {
				return nil, err
			}
			body = string(encoded)
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/"+pod.Name):
			stateMu.Lock()
			previousExists := previousPodExists
			isCreated := podCreated
			stateMu.Unlock()
			if !previousExists && !isCreated {
				status = http.StatusNotFound
				body = `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`
				break
			}
			encoded, err := json.Marshal(pod)
			if err != nil {
				return nil, err
			}
			body = string(encoded)
		case req.Method == http.MethodGet && req.URL.Query().Get("watch") == "true":
			stateMu.Lock()
			previousExists := previousPodExists
			requested := deleteRequested
			if previousExists && requested {
				previousPodExists = false
			}
			stateMu.Unlock()
			if previousExists {
				if !requested {
					status = http.StatusInternalServerError
					body = `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"watched before delete","code":500}`
					break
				}
				added, err := json.Marshal(map[string]any{"type": "ADDED", "object": pod})
				if err != nil {
					return nil, err
				}
				bookmarkPod := corev1.Pod{
					TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
					ObjectMeta: metav1.ObjectMeta{
						ResourceVersion: "2",
						Annotations:     map[string]string{metav1.InitialEventsAnnotationKey: "true"},
					},
				}
				bookmark, err := json.Marshal(map[string]any{"type": "BOOKMARK", "object": bookmarkPod})
				if err != nil {
					return nil, err
				}
				deletedPod := pod.DeepCopy()
				deletedPod.ResourceVersion = "3"
				deleted, err := json.Marshal(map[string]any{"type": "DELETED", "object": deletedPod})
				if err != nil {
					return nil, err
				}
				body = string(added) + "\n" + string(bookmark) + "\n" + string(deleted) + "\n"
				break
			}
			added, err := json.Marshal(map[string]any{"type": "ADDED", "object": pod})
			if err != nil {
				return nil, err
			}
			bookmarkPod := corev1.Pod{
				TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
				ObjectMeta: metav1.ObjectMeta{
					ResourceVersion: "2",
					Annotations:     map[string]string{metav1.InitialEventsAnnotationKey: "true"},
				},
			}
			bookmark, err := json.Marshal(map[string]any{"type": "BOOKMARK", "object": bookmarkPod})
			if err != nil {
				return nil, err
			}
			body = string(added) + "\n" + string(bookmark) + "\n"
		case req.Method == http.MethodGet:
			stateMu.Lock()
			previousExists := previousPodExists
			isCreated := podCreated
			stateMu.Unlock()
			list := corev1.PodList{
				TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"},
			}
			if previousExists || isCreated {
				list.Items = []corev1.Pod{pod}
			}
			encoded, err := json.Marshal(list)
			if err != nil {
				return nil, err
			}
			body = string(encoded)
		default:
			status = http.StatusInternalServerError
			body = `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"unexpected request","code":500}`
		}
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})
	httpClient := &http.Client{Transport: transport}
	clientSet, err := kubernetes.NewForConfigAndClient(&rest.Config{
		Host:          "https://kubernetes.invalid",
		ContentConfig: rest.ContentConfig{ContentType: "application/json"},
	}, httpClient)
	require.NoError(t, err)
	return clientSet
}
