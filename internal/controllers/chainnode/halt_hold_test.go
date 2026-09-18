package chainnode

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	appsv1 "github.com/voluzi/cosmopilot/v3/api/v1"
	"github.com/voluzi/cosmopilot/v3/internal/controllers"
	"github.com/voluzi/cosmopilot/v3/pkg/nodeutils"
)

func TestReconcileHaltHeightHoldUsesOnlyTerminalBoundaryEvidence(t *testing.T) {
	for _, tt := range []struct {
		name               string
		haltHeight         *int64
		appRunning         bool
		statusHeight       *int64
		terminationMessage string
		existingHold       string
		wantHold           string
		podPresent         bool
		podHaltHeight      *int64
	}{
		{name: "running at H-1 does not hold", haltHeight: ptr.To[int64](100), appRunning: true, statusHeight: ptr.To[int64](99), podPresent: true, podHaltHeight: ptr.To[int64](100)},
		{name: "terminal at H-1 holds", haltHeight: ptr.To[int64](100), statusHeight: ptr.To[int64](99), podPresent: true, podHaltHeight: ptr.To[int64](100), wantHold: "100"},
		{name: "terminal well before H recovers", haltHeight: ptr.To[int64](100), statusHeight: ptr.To[int64](98), podPresent: true, podHaltHeight: ptr.To[int64](100)},
		{name: "terminated sidecar preserves final H-1 observation", haltHeight: ptr.To[int64](100), terminationMessage: `{"latestHeight":99,"requiredUpgrade":null}`, podPresent: true, podHaltHeight: ptr.To[int64](100), wantHold: "100"},
		{name: "terminated sidecar preserves exact H observation", haltHeight: ptr.To[int64](100), terminationMessage: `{"latestHeight":100,"requiredUpgrade":null}`, podPresent: true, podHaltHeight: ptr.To[int64](100), wantHold: "100"},
		{name: "held node remains held while pod is missing", haltHeight: ptr.To[int64](100), existingHold: "100", wantHold: "100"},
		{name: "changed halt releases old hold", haltHeight: ptr.To[int64](101), existingHold: "100"},
		{name: "old pod evidence cannot hold changed halt", haltHeight: ptr.To[int64](101), existingHold: "100", terminationMessage: `{"latestHeight":100,"requiredUpgrade":null}`, podPresent: true, podHaltHeight: ptr.To[int64](100)},
		{name: "removed halt releases old hold", existingHold: "100"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			scheme := nodeUtilsAuthTestScheme(t)
			node := nodeUtilsAuthTestNode()
			node.Spec.Config.HaltHeight = tt.haltHeight
			if tt.existingHold != "" {
				node.Annotations = map[string]string{appsv1.AnnotationHaltHeightHold: tt.existingHold}
			}
			backing := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()
			r := &Reconciler{Client: backing}
			var pod *corev1.Pod
			if tt.podPresent {
				appState := corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}
				if tt.appRunning {
					appState = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
				}
				pod = &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: node.Name, Namespace: node.Namespace},
					Spec: corev1.PodSpec{InitContainers: []corev1.Container{{
						Name: nodeUtilsContainerName,
						Env:  []corev1.EnvVar{{Name: "HALT_HEIGHT", Value: strconv.FormatInt(ptr.Deref(tt.podHaltHeight, 0), 10)}},
					}}},
					Status: corev1.PodStatus{
						ContainerStatuses: []corev1.ContainerStatus{{Name: node.Spec.App.App, State: appState}},
						InitContainerStatuses: []corev1.ContainerStatus{{
							Name: nodeUtilsContainerName,
							State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
								Message: tt.terminationMessage,
							}},
						}},
					},
				}
			}
			status := nodeutils.UpgradeStatus{LatestHeight: tt.statusHeight}

			require.NoError(t, r.reconcileHaltHeightHold(t.Context(), node, pod, status))

			stored := &appsv1.ChainNode{}
			require.NoError(t, backing.Get(t.Context(), client.ObjectKeyFromObject(node), stored))
			assert.Equal(t, tt.wantHold, stored.Annotations[appsv1.AnnotationHaltHeightHold])
		})
	}
}

func TestEnsurePodReleasesHoldWhenConfiguredHaltChangesOrIsRemoved(t *testing.T) {
	for _, tt := range []struct {
		name       string
		haltHeight *int64
	}{
		{name: "changed", haltHeight: ptr.To[int64](101)},
		{name: "removed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			scheme := nodeUtilsAuthTestScheme(t)
			node := nodeUtilsAuthTestNode()
			node.Spec.App.Image = "repo/app:v1"
			node.Spec.Config.HaltHeight = ptr.To[int64](100)
			node.Annotations = map[string]string{appsv1.AnnotationHaltHeightHold: "100"}
			credential := nodeUtilsShutdownCredential{name: nodeUtilsShutdownSecretNameForToken(testShutdownToken), uid: "secret-uid", token: testShutdownToken}
			bindNodeUtilsCredential(node, credential.name, credential.uid)
			secret := ownedNodeUtilsSecret(t, scheme, node, credential.token, credential.uid)
			config := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: node.Name, Namespace: node.Namespace}}
			specClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(config).Build()
			specReconciler := &Reconciler{Client: specClient, Scheme: scheme, opts: &controllers.ControllerRunOptions{NodeUtilsImage: "node-utils:test"}}
			current, err := specReconciler.getPodSpec(ctx, node, "config-hash", credential.name)
			require.NoError(t, err)
			stampNodeUtilsShutdownCredential(current, credential)
			current.Status = corev1.PodStatus{
				Phase: corev1.PodRunning,
				ContainerStatuses: []corev1.ContainerStatus{{
					Name: node.Spec.App.App,
					State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
						ExitCode: 0,
					}},
				}},
				InitContainerStatuses: []corev1.ContainerStatus{{
					Name: nodeUtilsContainerName,
					State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
						Message: `{"latestHeight":100,"requiredUpgrade":null}`,
					}},
				}},
			}
			node.Spec.Config.HaltHeight = tt.haltHeight
			backing := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(node).WithObjects(node, current, secret, config).Build()

			var creates atomic.Int32
			kubeHTTPClient := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				status := http.StatusOK
				body := `{"kind":"Status","apiVersion":"v1","status":"Success"}`
				switch req.Method {
				case http.MethodGet:
					status = http.StatusNotFound
					body = `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`
				case http.MethodPost:
					creates.Add(1)
					status = http.StatusInternalServerError
					body = `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"expected replacement","code":500}`
				}
				return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			clientSet, err := kubernetes.NewForConfigAndClient(&rest.Config{Host: "https://kubernetes.invalid"}, kubeHTTPClient)
			require.NoError(t, err)
			r := &Reconciler{
				Client:                backing,
				APIReader:             backing,
				ClientSet:             clientSet,
				Scheme:                scheme,
				recorder:              record.NewFakeRecorder(10),
				opts:                  &controllers.ControllerRunOptions{NodeUtilsImage: "node-utils:test"},
				shutdownClientFactory: func(string, string) nodeUtilsShutdownClient { return &fakeNodeUtilsShutdownClient{} },
			}

			err = r.ensurePod(ctx, nil, node, "config-hash")
			require.ErrorContains(t, err, "expected replacement")
			assert.Equal(t, int32(1), creates.Load())
			assert.NotContains(t, node.Annotations, appsv1.AnnotationHaltHeightHold)
		})
	}
}

func TestNodeUtilsTerminationStatusIgnoresOtherContainers(t *testing.T) {
	pod := &corev1.Pod{Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{
		{Name: "other-sidecar", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: `{"latestHeight":99,"requiredUpgrade":null}`}}},
		{Name: nodeUtilsContainerName, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: `not-json`}}},
	}}}

	status, ok := nodeUtilsTerminationStatus(pod)
	assert.False(t, ok)
	assert.Nil(t, status.LatestHeight)
}

func TestNodeUtilsTerminationStatusUsesNamedSidecarLastTermination(t *testing.T) {
	pod := &corev1.Pod{Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{{
		Name:  nodeUtilsContainerName,
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
			Message: `{"latestHeight":100,"requiredUpgrade":null}`,
		}},
	}}}}

	status, ok := nodeUtilsTerminationStatus(pod)
	require.True(t, ok)
	require.NotNil(t, status.LatestHeight)
	assert.Equal(t, int64(100), *status.LatestHeight)
}

func TestEnsurePodDoesNotRecreateTerminalAppAtAmbiguousHaltBoundary(t *testing.T) {
	ctx := t.Context()
	scheme := nodeUtilsAuthTestScheme(t)
	node := nodeUtilsAuthTestNode()
	node.Spec.App.Image = "repo/app:v1"
	node.Spec.Config.HaltHeight = ptr.To[int64](100)
	credential := nodeUtilsShutdownCredential{name: nodeUtilsShutdownSecretNameForToken(testShutdownToken), uid: "secret-uid", token: testShutdownToken}
	bindNodeUtilsCredential(node, credential.name, credential.uid)
	secret := ownedNodeUtilsSecret(t, scheme, node, credential.token, credential.uid)
	config := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: node.Name, Namespace: node.Namespace}}
	specClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(config).Build()
	specReconciler := &Reconciler{Client: specClient, Scheme: scheme, opts: &controllers.ControllerRunOptions{NodeUtilsImage: "node-utils:test"}}
	current, err := specReconciler.getPodSpec(ctx, node, "config-hash", credential.name)
	require.NoError(t, err)
	stampNodeUtilsShutdownCredential(current, credential)
	current.Status = corev1.PodStatus{
		Phase: corev1.PodRunning,
		ContainerStatuses: []corev1.ContainerStatus{{
			Name:  node.Spec.App.App,
			Ready: false,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 0,
			}},
		}},
		InitContainerStatuses: []corev1.ContainerStatus{{
			Name:  nodeUtilsContainerName,
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}},
	}
	backing := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(node).WithObjects(node, current, secret, config).Build()

	var creates atomic.Int32
	kubeHTTPClient := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		status := http.StatusOK
		body := `{"kind":"Status","apiVersion":"v1","status":"Success"}`
		switch req.Method {
		case http.MethodGet:
			status = http.StatusNotFound
			body = `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`
		case http.MethodPost:
			creates.Add(1)
			status = http.StatusInternalServerError
			body = `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"unexpected create","code":500}`
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	clientSet, err := kubernetes.NewForConfigAndClient(&rest.Config{Host: "https://kubernetes.invalid"}, kubeHTTPClient)
	require.NoError(t, err)
	r := &Reconciler{
		Client:    backing,
		APIReader: backing,
		ClientSet: clientSet,
		Scheme:    scheme,
		recorder:  record.NewFakeRecorder(10),
		opts:      &controllers.ControllerRunOptions{NodeUtilsImage: "node-utils:test"},
		upgradeClientFactory: func(string) upgradeStatusClient {
			return staticUpgradeStatusClient{status: nodeutils.UpgradeStatus{LatestHeight: ptr.To[int64](99)}}
		},
		shutdownClientFactory: func(string, string) nodeUtilsShutdownClient { return &fakeNodeUtilsShutdownClient{} },
	}

	require.NoError(t, r.ensurePod(ctx, nil, node, "config-hash"))
	assert.Zero(t, creates.Load())
	assert.Equal(t, "100", node.Annotations[appsv1.AnnotationHaltHeightHold])
}

func TestEnsurePodDoesNotRecreateTerminalAppWithExactHaltTerminationObservation(t *testing.T) {
	ctx := t.Context()
	scheme := nodeUtilsAuthTestScheme(t)
	node := nodeUtilsAuthTestNode()
	node.Spec.App.Image = "repo/app:v1"
	node.Spec.Config.HaltHeight = ptr.To[int64](100)
	node.Status.LatestHeight = 99
	credential := nodeUtilsShutdownCredential{name: nodeUtilsShutdownSecretNameForToken(testShutdownToken), uid: "secret-uid", token: testShutdownToken}
	bindNodeUtilsCredential(node, credential.name, credential.uid)
	secret := ownedNodeUtilsSecret(t, scheme, node, credential.token, credential.uid)
	config := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: node.Name, Namespace: node.Namespace}}
	specClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(config).Build()
	specReconciler := &Reconciler{Client: specClient, Scheme: scheme, opts: &controllers.ControllerRunOptions{NodeUtilsImage: "node-utils:test"}}
	current, err := specReconciler.getPodSpec(ctx, node, "config-hash", credential.name)
	require.NoError(t, err)
	stampNodeUtilsShutdownCredential(current, credential)
	current.Status = corev1.PodStatus{
		Phase: corev1.PodRunning,
		ContainerStatuses: []corev1.ContainerStatus{{
			Name:  node.Spec.App.App,
			Ready: false,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 0,
			}},
		}},
		InitContainerStatuses: []corev1.ContainerStatus{{
			Name: nodeUtilsContainerName,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 0,
				Message:  `{"latestHeight":100,"requiredUpgrade":null}`,
			}},
		}},
	}
	backing := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(node).WithObjects(node, current, secret, config).Build()

	var creates atomic.Int32
	kubeHTTPClient := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		status := http.StatusOK
		body := `{"kind":"Status","apiVersion":"v1","status":"Success"}`
		switch req.Method {
		case http.MethodGet:
			status = http.StatusNotFound
			body = `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`
		case http.MethodPost:
			creates.Add(1)
			status = http.StatusInternalServerError
			body = `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"unexpected create","code":500}`
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	clientSet, err := kubernetes.NewForConfigAndClient(&rest.Config{Host: "https://kubernetes.invalid"}, kubeHTTPClient)
	require.NoError(t, err)
	r := &Reconciler{
		Client:    backing,
		APIReader: backing,
		ClientSet: clientSet,
		Scheme:    scheme,
		recorder:  record.NewFakeRecorder(10),
		opts:      &controllers.ControllerRunOptions{NodeUtilsImage: "node-utils:test"},
		upgradeClientFactory: func(string) upgradeStatusClient {
			t.Fatal("early failed-pod recovery must use the terminal sidecar observation")
			return nil
		},
		shutdownClientFactory: func(string, string) nodeUtilsShutdownClient { return &fakeNodeUtilsShutdownClient{} },
	}

	require.NoError(t, r.ensurePod(ctx, nil, node, "config-hash"))
	assert.Zero(t, creates.Load())
	assert.Equal(t, "100", node.Annotations[appsv1.AnnotationHaltHeightHold])
	assert.Equal(t, int64(99), node.Status.LatestHeight)
}
