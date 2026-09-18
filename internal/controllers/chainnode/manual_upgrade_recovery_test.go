package chainnode

import (
	"errors"
	"io"
	"net/http"
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

func TestManualUpgradePodIdentityDistinguishesSameImage(t *testing.T) {
	upgradeA := &appsv1.Upgrade{Height: 100, Source: appsv1.ManualUpgrade, Name: "plan-a", Image: "repo/app:v2", Status: appsv1.UpgradeOnGoing}
	upgradeB := &appsv1.Upgrade{Height: 100, Source: appsv1.ManualUpgrade, Name: "plan-b", Image: "repo/app:v2", Status: appsv1.UpgradeOnGoing}
	pod := &corev1.Pod{}
	oldPod := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{Image: upgradeA.Image}}}}

	assert.False(t, podMatchesManualUpgrade(oldPod, upgradeA))
	require.NoError(t, stampManualUpgradeIdentity(pod, upgradeA))
	assert.True(t, podMatchesManualUpgrade(pod, upgradeA))
	assert.False(t, podMatchesManualUpgrade(pod, upgradeB))
}

func TestRecoverOngoingManualUpgradeReplacesUnstampedOldPod(t *testing.T) {
	ctx := t.Context()
	scheme := nodeUtilsAuthTestScheme(t)
	node := nodeUtilsAuthTestNode()
	node.Spec.App.Image = "repo/app:v2"
	node.Status.Upgrades = []appsv1.Upgrade{{Height: 100, Source: appsv1.ManualUpgrade, Name: "v2", Image: "repo/app:v2", Status: appsv1.UpgradeOnGoing}}
	credential := nodeUtilsShutdownCredential{name: nodeUtilsShutdownSecretNameForToken(testShutdownToken), uid: "secret-uid", token: testShutdownToken}
	bindNodeUtilsCredential(node, credential.name, credential.uid)
	secret := ownedNodeUtilsSecret(t, scheme, node, credential.token, credential.uid)
	config := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: node.Name, Namespace: node.Namespace}}
	upgradesConfig := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: node.Name + "-upgrades", Namespace: node.Namespace}, Data: map[string]string{upgradesConfigFile: `{"upgrades":[]}`}}
	specClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(config).Build()
	specReconciler := &Reconciler{Client: specClient, Scheme: scheme, opts: &controllers.ControllerRunOptions{NodeUtilsImage: "node-utils:test"}}
	desired, err := specReconciler.getPodSpec(ctx, node, "config-hash", credential.name)
	require.NoError(t, err)
	stampNodeUtilsShutdownCredential(desired, credential)
	current := desired.DeepCopy()
	delete(current.Annotations, controllers.AnnotationManualUpgradeIdentity)
	backing := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(node).WithObjects(node, current, secret, config, upgradesConfig).Build()

	var deletes, creates atomic.Int32
	kubeHTTPClient := &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		status := http.StatusOK
		body := `{"kind":"Status","apiVersion":"v1","status":"Success"}`
		switch {
		case req.Method == http.MethodDelete:
			deletes.Add(1)
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/"+node.Name):
			status = http.StatusNotFound
			body = `{"kind":"Status","apiVersion":"v1","status":"Failure","reason":"NotFound","code":404}`
		case req.Method == http.MethodPost:
			creates.Add(1)
			status = http.StatusInternalServerError
			body = `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"stop after create attempt","code":500}`
		case req.Method == http.MethodGet:
			status = http.StatusInternalServerError
			body = `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"stop waiting","code":500}`
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
		shutdownClientFactory: func(string, string) nodeUtilsShutdownClient {
			return &fakeNodeUtilsShutdownClient{}
		},
	}

	handled, err := r.recoverOngoingManualUpgrade(ctx, node, current, desired)
	require.ErrorContains(t, err, "stop after create attempt")
	assert.True(t, handled)
	assert.Equal(t, int32(1), deletes.Load())
	assert.Equal(t, int32(1), creates.Load())
	assert.Equal(t, appsv1.UpgradeOnGoing, node.Status.Upgrades[0].Status)
}

func TestRecoverOngoingManualUpgradeHonorsImageOverrides(t *testing.T) {
	for _, tt := range []struct {
		name        string
		setOverride func(*appsv1.ChainNode)
	}{
		{name: "image override", setOverride: func(node *appsv1.ChainNode) { node.Spec.OverrideImage = ptr.To("repo/app:pinned") }},
		{name: "version override", setOverride: func(node *appsv1.ChainNode) { node.Spec.OverrideVersion = ptr.To("pinned") }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			scheme := nodeUtilsAuthTestScheme(t)
			node := nodeUtilsAuthTestNode()
			node.Spec.App.Image = "repo/app:v1"
			tt.setOverride(node)
			node.Status.Upgrades = []appsv1.Upgrade{{
				Height: 100,
				Source: appsv1.ManualUpgrade,
				Name:   "v2",
				Image:  "repo/app:v2",
				Status: appsv1.UpgradeOnGoing,
			}}
			upgradesConfig := &corev1.ConfigMap{
				ObjectMeta: metav1.ObjectMeta{Name: node.Name + "-upgrades", Namespace: node.Namespace},
				Data:       map[string]string{upgradesConfigFile: `{"upgrades":[]}`},
			}
			backing := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(node).WithObjects(node, upgradesConfig).Build()
			var podRequests atomic.Int32
			kubeHTTPClient := &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
				podRequests.Add(1)
				return &http.Response{
					StatusCode: http.StatusInternalServerError,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(`{"kind":"Status","apiVersion":"v1","status":"Failure","message":"unexpected pod mutation","code":500}`)),
				}, nil
			})}
			clientSet, err := kubernetes.NewForConfigAndClient(&rest.Config{Host: "https://kubernetes.invalid"}, kubeHTTPClient)
			require.NoError(t, err)
			r := &Reconciler{
				Client:    backing,
				APIReader: backing,
				ClientSet: clientSet,
				Scheme:    scheme,
				recorder:  record.NewFakeRecorder(10),
			}
			current := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: node.Name, Namespace: node.Namespace}}
			desired := current.DeepCopy()
			desired.Spec.Containers = []corev1.Container{{Name: node.Spec.App.App, Image: node.GetAppImage()}}

			handled, err := r.recoverOngoingManualUpgrade(ctx, node, current, desired)
			require.NoError(t, err)
			assert.True(t, handled)
			assert.Zero(t, podRequests.Load())
			assert.Equal(t, appsv1.UpgradeSkipped, node.Status.Upgrades[0].Status)
			assert.Equal(t, node.GetAppImage(), desired.Spec.Containers[0].Image)
			storedConfig := &corev1.ConfigMap{}
			require.NoError(t, backing.Get(ctx, client.ObjectKeyFromObject(upgradesConfig), storedConfig))
			assert.Contains(t, storedConfig.Data[upgradesConfigFile], `"status":"skipped"`)
		})
	}
}

func TestEnsurePodCompletesStartedStampedManualUpgradeWithoutNodeUtils(t *testing.T) {
	ctx := t.Context()
	scheme := nodeUtilsAuthTestScheme(t)
	node := nodeUtilsAuthTestNode()
	node.Spec.App.Image = "repo/app:v1"
	node.Spec.VPA = &appsv1.VerticalAutoscalingConfig{Enabled: true, ResetVpaAfterNodeUpgrade: true}
	node.Annotations = map[string]string{controllers.AnnotationVPAResources: `{"requests":{"cpu":"2"}}`}
	node.Status.Upgrades = []appsv1.Upgrade{{
		Height: 100,
		Source: appsv1.ManualUpgrade,
		Name:   "v2",
		Image:  "repo/app:v2",
		Status: appsv1.UpgradeOnGoing,
	}}
	credential := nodeUtilsShutdownCredential{name: nodeUtilsShutdownSecretNameForToken(testShutdownToken), uid: "secret-uid", token: testShutdownToken}
	bindNodeUtilsCredential(node, credential.name, credential.uid)
	secret := ownedNodeUtilsSecret(t, scheme, node, credential.token, credential.uid)
	config := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: node.Name, Namespace: node.Namespace}}
	upgradesConfig := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: node.Name + "-upgrades", Namespace: node.Namespace},
		Data:       map[string]string{upgradesConfigFile: `{"upgrades":[]}`},
	}
	specClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(config).Build()
	specReconciler := &Reconciler{
		Client: specClient,
		Scheme: scheme,
		opts:   &controllers.ControllerRunOptions{NodeUtilsImage: "node-utils:test"},
		statsClientFactory: func(string) nodeutils.StatsClient {
			return &mockStatsClient{}
		},
	}
	current, err := specReconciler.getPodSpec(ctx, node, "config-hash", credential.name)
	require.NoError(t, err)
	stampNodeUtilsShutdownCredential(current, credential)
	current.Status = corev1.PodStatus{
		Phase: corev1.PodRunning,
		ContainerStatuses: []corev1.ContainerStatus{{
			Name:    node.Spec.App.App,
			Ready:   true,
			Started: ptr.To(true),
			State:   corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}},
		InitContainerStatuses: []corev1.ContainerStatus{{
			Name:  nodeUtilsContainerName,
			Ready: false,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 1,
			}},
		}},
	}
	backing := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(node).WithObjects(node, current, secret, config, upgradesConfig).Build()
	var nodeUtilsCalls int
	r := &Reconciler{
		Client:    backing,
		APIReader: backing,
		Scheme:    scheme,
		recorder:  record.NewFakeRecorder(10),
		opts:      &controllers.ControllerRunOptions{NodeUtilsImage: "node-utils:test"},
		statsClientFactory: func(string) nodeutils.StatsClient {
			return &mockStatsClient{}
		},
		upgradeClientFactory: func(string) upgradeStatusClient {
			nodeUtilsCalls++
			return failingUpgradeStatusClient{err: errors.New("node-utils unavailable")}
		},
	}

	require.NoError(t, r.ensurePod(ctx, nil, node, "config-hash"))
	assert.Zero(t, nodeUtilsCalls)
	assert.Equal(t, "repo/app:v2", node.Status.AppImage)
	assert.Equal(t, "v2", node.Status.AppVersion)
	assert.Equal(t, appsv1.UpgradeCompleted, node.Status.Upgrades[0].Status)
	assert.NotContains(t, node.Annotations, controllers.AnnotationVPAResources)
	assert.NotEmpty(t, node.Annotations[controllers.AnnotationVPALastCPUScale])
	assert.NotEmpty(t, node.Annotations[controllers.AnnotationVPALastMemoryScale])
}

func TestEnsurePodCompletesTerminatedStampedManualUpgradeBeforeCrashRecovery(t *testing.T) {
	ctx := t.Context()
	scheme := nodeUtilsAuthTestScheme(t)
	node := nodeUtilsAuthTestNode()
	node.Spec.App.Image = "repo/app:v1"
	node.Status.Upgrades = []appsv1.Upgrade{{
		Height: 100,
		Source: appsv1.ManualUpgrade,
		Name:   "v2",
		Image:  "repo/app:v2",
		Status: appsv1.UpgradeOnGoing,
	}}
	credential := nodeUtilsShutdownCredential{name: nodeUtilsShutdownSecretNameForToken(testShutdownToken), uid: "secret-uid", token: testShutdownToken}
	bindNodeUtilsCredential(node, credential.name, credential.uid)
	secret := ownedNodeUtilsSecret(t, scheme, node, credential.token, credential.uid)
	config := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: node.Name, Namespace: node.Namespace}}
	upgradesConfig := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: node.Name + "-upgrades", Namespace: node.Namespace},
		Data:       map[string]string{upgradesConfigFile: `{"upgrades":[]}`},
	}
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
				ExitCode: 1,
			}},
		}},
		InitContainerStatuses: []corev1.ContainerStatus{{
			Name: nodeUtilsContainerName,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
				ExitCode: 1,
			}},
		}},
	}
	backing := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(node).WithObjects(node, current, secret, config, upgradesConfig).Build()
	var nodeUtilsCalls int
	r := &Reconciler{
		Client:    backing,
		APIReader: backing,
		Scheme:    scheme,
		recorder:  record.NewFakeRecorder(10),
		opts:      &controllers.ControllerRunOptions{NodeUtilsImage: "node-utils:test"},
		upgradeClientFactory: func(string) upgradeStatusClient {
			nodeUtilsCalls++
			return failingUpgradeStatusClient{err: errors.New("node-utils unavailable")}
		},
	}

	require.NoError(t, r.ensurePod(ctx, nil, node, "config-hash"))
	assert.Zero(t, nodeUtilsCalls)
	assert.Equal(t, "repo/app:v2", node.Status.AppImage)
	assert.Equal(t, "v2", node.Status.AppVersion)
	assert.Equal(t, appsv1.UpgradeCompleted, node.Status.Upgrades[0].Status)
}
