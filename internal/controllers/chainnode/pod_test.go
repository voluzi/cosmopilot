package chainnode

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	appsv1 "github.com/voluzi/cosmopilot/v3/api/v1"
	"github.com/voluzi/cosmopilot/v3/internal/chainutils"
	"github.com/voluzi/cosmopilot/v3/internal/controllers"
	"github.com/voluzi/cosmopilot/v3/internal/k8s"
)

func TestGeneratedPodComponentsContainNoTraceStoreArtifacts(t *testing.T) {
	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "node"},
		Spec: appsv1.ChainNodeSpec{
			App:    appsv1.AppSpec{App: "appd"},
			Config: &appsv1.Config{},
		},
	}
	r := &Reconciler{opts: &controllers.ControllerRunOptions{NodeUtilsImage: "node-utils:test"}}

	for _, volume := range r.buildBaseVolumes(chainNode) {
		if volume.Name == "trace" {
			t.Fatal("generated pod still contains trace volume")
		}
	}
	nodeUtils := r.buildNodeUtilsInitContainer(chainNode, "shutdown-token", k8s.NonRootUID, k8s.NonRootUID)
	for _, env := range nodeUtils.Env {
		if env.Name == "TRACE_STORE" || env.Name == "CREATE_FIFO" {
			t.Fatalf("node-utils still contains trace environment %s", env.Name)
		}
	}
	for _, mount := range nodeUtils.VolumeMounts {
		if mount.Name == "trace" || mount.MountPath == "/trace" {
			t.Fatalf("node-utils still contains trace mount %#v", mount)
		}
	}
	app := r.buildAppContainer(chainNode, nil, "/ready", corev1.ResourceRequirements{}, nil)
	for i, arg := range append(nodeUtils.Args, app.Args...) {
		if arg == "--trace-store" || strings.HasPrefix(arg, "--trace-store=") ||
			arg == "--create-fifo" || strings.HasPrefix(arg, "--create-fifo=") ||
			arg == "/trace/trace.fifo" {
			t.Fatalf("app arg %d still contains trace artifact %q", i, arg)
		}
	}
	for _, mount := range app.VolumeMounts {
		if mount.Name == "trace" || mount.MountPath == "/trace" {
			t.Fatalf("app still contains trace mount %#v", mount)
		}
	}
}

func TestAppHealthProbesUseCometBFTWhileReadinessUsesNodeUtils(t *testing.T) {
	chainNode := &appsv1.ChainNode{
		Spec: appsv1.ChainNodeSpec{
			App:    appsv1.AppSpec{App: "appd"},
			Config: &appsv1.Config{},
		},
	}
	r := &Reconciler{}

	app := r.buildAppContainer(chainNode, nil, "/ready", corev1.ResourceRequirements{}, nil)
	require.NotNil(t, app.StartupProbe)
	require.NotNil(t, app.StartupProbe.HTTPGet)
	assert.Equal(t, "/health", app.StartupProbe.HTTPGet.Path)
	assert.Equal(t, int32(chainutils.RpcPort), app.StartupProbe.HTTPGet.Port.IntVal)
	require.NotNil(t, app.LivenessProbe)
	require.NotNil(t, app.LivenessProbe.HTTPGet)
	assert.Equal(t, "/health", app.LivenessProbe.HTTPGet.Path)
	assert.Equal(t, int32(chainutils.RpcPort), app.LivenessProbe.HTTPGet.Port.IntVal)
	require.NotNil(t, app.ReadinessProbe)
	require.NotNil(t, app.ReadinessProbe.HTTPGet)
	assert.Equal(t, "/ready", app.ReadinessProbe.HTTPGet.Path)
	assert.Equal(t, int32(nodeUtilsPort), app.ReadinessProbe.HTTPGet.Port.IntVal)
}

func TestIsChainNodePodRunningIgnoresCrashedNodeUtils(t *testing.T) {
	chainNode := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: "node", Namespace: "default"}}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: chainNode.Name, Namespace: chainNode.Namespace},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:  "appd",
				Ready: true,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}},
			InitContainerStatuses: []corev1.ContainerStatus{{
				Name: nodeUtilsContainerName,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 1,
				}},
			}},
		},
	}
	scheme := gcpImportTestScheme(t)
	r := &Reconciler{Client: fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()}

	running, _, err := r.isChainNodePodRunning(t.Context(), chainNode)
	require.NoError(t, err)
	assert.True(t, running)
}

func TestNodeUtilsUsesEffectiveAppRunAsUserForSDKUpgradeInfo(t *testing.T) {
	tests := []struct {
		name               string
		config             *appsv1.Config
		wantUID            int64
		wantGID            int64
		wantRunAsNonRoot   bool
		checkIsolationFrom bool
	}{
		{
			name:             "restricted defaults use uid 1000",
			wantUID:          k8s.NonRootUID,
			wantGID:          k8s.NonRootUID,
			wantRunAsNonRoot: true,
		},
		{
			name: "app uid zero overrides pod uid",
			config: &appsv1.Config{
				SecurityContext: &corev1.SecurityContext{
					RunAsUser:                ptr.To[int64](0),
					RunAsGroup:               ptr.To[int64](0),
					RunAsNonRoot:             ptr.To(false),
					Privileged:               ptr.To(true),
					AllowPrivilegeEscalation: ptr.To(true),
					Capabilities: &corev1.Capabilities{
						Add: []corev1.Capability{"SYS_ADMIN"},
					},
				},
				PodSecurityContext: &corev1.PodSecurityContext{RunAsUser: ptr.To[int64](2000), RunAsGroup: ptr.To[int64](2000)},
			},
			wantUID:            0,
			wantGID:            0,
			wantRunAsNonRoot:   false,
			checkIsolationFrom: true,
		},
		{
			name: "app without uid inherits explicit pod uid",
			config: &appsv1.Config{
				SecurityContext:    &corev1.SecurityContext{AllowPrivilegeEscalation: ptr.To(false)},
				PodSecurityContext: &corev1.PodSecurityContext{RunAsUser: ptr.To[int64](2345), RunAsGroup: ptr.To[int64](2346)},
			},
			wantUID:          2345,
			wantGID:          2346,
			wantRunAsNonRoot: true,
		},
		{
			name: "node-utils matches app owner for sdk 0600 marker",
			config: &appsv1.Config{
				SecurityContext:    &corev1.SecurityContext{RunAsUser: ptr.To[int64](3456), RunAsGroup: ptr.To[int64](3457)},
				PodSecurityContext: &corev1.PodSecurityContext{RunAsUser: ptr.To[int64](2000), RunAsGroup: ptr.To[int64](2000)},
			},
			wantUID:          3456,
			wantGID:          3457,
			wantRunAsNonRoot: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod, err := renderPodForSecurityContextTest(t, tt.config)
			require.NoError(t, err)
			appSecurityContext := pod.Spec.Containers[0].SecurityContext
			require.NotNil(t, appSecurityContext)
			nodeUtils := requireNodeUtilsContainer(t, pod)
			require.NotNil(t, nodeUtils.SecurityContext)
			require.NotNil(t, nodeUtils.SecurityContext.RunAsUser)
			assert.Equal(t, tt.wantUID, *nodeUtils.SecurityContext.RunAsUser)
			require.NotNil(t, nodeUtils.SecurityContext.RunAsGroup)
			assert.Equal(t, tt.wantGID, *nodeUtils.SecurityContext.RunAsGroup)
			require.NotNil(t, nodeUtils.SecurityContext.RunAsNonRoot)
			assert.Equal(t, tt.wantRunAsNonRoot, *nodeUtils.SecurityContext.RunAsNonRoot)
			if appSecurityContext.RunAsUser != nil {
				assert.Equal(t, *appSecurityContext.RunAsUser, *nodeUtils.SecurityContext.RunAsUser)
			}
			if appSecurityContext.RunAsGroup != nil {
				assert.Equal(t, *appSecurityContext.RunAsGroup, *nodeUtils.SecurityContext.RunAsGroup)
			}
			if tt.checkIsolationFrom {
				assert.Nil(t, nodeUtils.SecurityContext.Privileged)
				require.NotNil(t, nodeUtils.SecurityContext.AllowPrivilegeEscalation)
				assert.False(t, *nodeUtils.SecurityContext.AllowPrivilegeEscalation)
				require.NotNil(t, nodeUtils.SecurityContext.Capabilities)
				assert.Empty(t, nodeUtils.SecurityContext.Capabilities.Add)
				assert.Equal(t, []corev1.Capability{"ALL"}, nodeUtils.SecurityContext.Capabilities.Drop)
				require.NotNil(t, nodeUtils.SecurityContext.SeccompProfile)
				assert.Equal(t, corev1.SeccompProfileTypeRuntimeDefault, nodeUtils.SecurityContext.SeccompProfile.Type)
			}
			for _, mount := range nodeUtils.VolumeMounts {
				if mount.Name == "data" {
					assert.True(t, mount.ReadOnly)
					return
				}
			}
			t.Fatal("node-utils data mount not found")
		})
	}
}

func TestGetPodSpecRejectsImageDefinedUserForNodeUtilsMarkerAccess(t *testing.T) {
	config := &appsv1.Config{
		SecurityContext:    &corev1.SecurityContext{},
		PodSecurityContext: &corev1.PodSecurityContext{},
	}

	pod, err := renderPodForSecurityContextTest(t, config)
	require.Nil(t, pod)
	require.ErrorContains(t, err, "explicit numeric runAsUser")
}

func TestGetPodSpecRejectsImageDefinedGroupForNodeUtilsDataTraversal(t *testing.T) {
	config := &appsv1.Config{
		SecurityContext:    &corev1.SecurityContext{RunAsUser: ptr.To[int64](1001)},
		PodSecurityContext: &corev1.PodSecurityContext{RunAsUser: ptr.To[int64](1001)},
	}

	pod, err := renderPodForSecurityContextTest(t, config)
	require.Nil(t, pod)
	require.ErrorContains(t, err, "explicit numeric runAsGroup")
}

func renderPodForSecurityContextTest(t *testing.T, config *appsv1.Config) (*corev1.Pod, error) {
	t.Helper()
	genesisConfigMap := "genesis"
	node := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "node", Namespace: "default"},
		Spec: appsv1.ChainNodeSpec{
			App:     appsv1.AppSpec{Image: "repo/app", App: "appd"},
			Config:  config,
			Genesis: &appsv1.GenesisConfig{ConfigMap: &genesisConfigMap},
		},
	}
	scheme := gcpImportTestScheme(t)
	client := fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: node.Name, Namespace: node.Namespace},
	}).Build()
	r := &Reconciler{
		Client: client,
		Scheme: scheme,
		opts:   &controllers.ControllerRunOptions{NodeUtilsImage: "node-utils:test"},
	}
	return r.getPodSpec(t.Context(), node, "config-hash", "shutdown-secret")
}

func requireNodeUtilsContainer(t *testing.T, pod *corev1.Pod) *corev1.Container {
	t.Helper()
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == nodeUtilsContainerName {
			return &pod.Spec.InitContainers[i]
		}
	}
	t.Fatal("node-utils container not found")
	return nil
}

func TestPodSpecHash(t *testing.T) {
	tests := []struct {
		name      string
		pod       *corev1.Pod
		wantError bool
		sameHash  *corev1.Pod // if set, hash should match this pod
	}{
		{
			name: "basic pod",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "test", Image: "nginx:latest"},
					},
				},
			},
			wantError: false,
		},
		{
			name: "identical pods produce same hash",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "app", Image: "myapp:v1"},
					},
				},
			},
			sameHash: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "app", Image: "myapp:v1"},
					},
				},
			},
			wantError: false,
		},
		{
			name: "pod with volumes",
			pod: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "app", Image: "myapp:v1"},
					},
					Volumes: []corev1.Volume{
						{Name: "data", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
					},
				},
			},
			wantError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hash, err := podSpecHash(tt.pod)
			if (err != nil) != tt.wantError {
				t.Errorf("podSpecHash() error = %v, wantError %v", err, tt.wantError)
				return
			}
			if !tt.wantError && hash == "" {
				t.Error("podSpecHash() returned empty hash")
			}

			if tt.sameHash != nil {
				sameHash, err := podSpecHash(tt.sameHash)
				if err != nil {
					t.Fatalf("podSpecHash() failed for comparison pod: %v", err)
				}
				if hash != sameHash {
					t.Errorf("podSpecHash() expected same hash for identical pods, got %s vs %s", hash, sameHash)
				}
			}
		})
	}
}

func TestPodSpecHash_DifferentSpecsProduceDifferentHashes(t *testing.T) {
	pod1 := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Image: "myapp:v1"},
			},
		},
	}

	pod2 := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "app", Image: "myapp:v2"}, // Different image
			},
		},
	}

	hash1, err1 := podSpecHash(pod1)
	hash2, err2 := podSpecHash(pod2)

	if err1 != nil || err2 != nil {
		t.Fatalf("podSpecHash() failed: %v, %v", err1, err2)
	}

	if hash1 == hash2 {
		t.Error("podSpecHash() expected different hashes for different pod specs")
	}
}

func TestIsPodTerminating(t *testing.T) {
	now := metav1.Now()

	tests := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{
			name: "pod is terminating",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					DeletionTimestamp: &now,
				},
			},
			want: true,
		},
		{
			name: "pod is not terminating",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					DeletionTimestamp: nil,
				},
			},
			want: false,
		},
		{
			name: "new pod",
			pod:  &corev1.Pod{},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isPodTerminating(tt.pod); got != tt.want {
				t.Errorf("isPodTerminating() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPodSpecChanged(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		name     string
		existing *corev1.Pod
		new      *corev1.Pod
		want     bool
	}{
		{
			name: "no hash annotation - considers changed",
			existing: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{},
				},
			},
			new: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "app", Image: "myapp:v1"}},
				},
			},
			want: true,
		},
		{
			name: "invalid hash annotation - considers changed",
			existing: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"cosmopilot.voluzi.com/pod-spec-hash": "invalid",
					},
				},
			},
			new: &corev1.Pod{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "app", Image: "myapp:v1"}},
				},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := podSpecChanged(ctx, tt.existing, tt.new); got != tt.want {
				t.Errorf("podSpecChanged() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestGetPodSpecDeferredSidecarHashIsIndependentOfGroupHealth(t *testing.T) {
	deferredV1 := appsv1.SidecarSpec{Name: "indexer", Image: ptr.To("indexer:v1"), DeferUntilHealthy: ptr.To(true)}
	nonDeferred := appsv1.SidecarSpec{Name: "metrics", Image: ptr.To("metrics:v1")}
	healthy := deferredSidecarPodSpec(t, 1, deferredV1, nonDeferred)
	unhealthy := deferredSidecarPodSpec(t, 0, deferredV1, nonDeferred)

	assert.True(t, hasContainer(healthy.Spec.InitContainers, deferredV1.Name))
	assert.False(t, hasContainer(unhealthy.Spec.InitContainers, deferredV1.Name))
	assert.True(t, hasContainer(healthy.Spec.InitContainers, nonDeferred.Name))
	assert.True(t, hasContainer(unhealthy.Spec.InitContainers, nonDeferred.Name))
	assert.Equal(t, deferredV1.Name, healthy.Annotations[controllers.AnnotationMaterializedDeferredSidecars])
	assert.Equal(t, "", unhealthy.Annotations[controllers.AnnotationMaterializedDeferredSidecars])
	assert.Equal(t, healthy.Annotations[controllers.AnnotationPodSpecHash], unhealthy.Annotations[controllers.AnnotationPodSpecHash])
	assert.False(t, podSpecChanged(t.Context(), healthy, unhealthy))
	assert.False(t, podSpecChanged(t.Context(), unhealthy, healthy))

	deferredV2 := deferredV1
	deferredV2.Image = ptr.To("indexer:v2")
	unhealthyWithConfigChange := deferredSidecarPodSpec(t, 0, deferredV2, nonDeferred)
	assert.NotEqual(t, unhealthy.Annotations[controllers.AnnotationPodSpecHash], unhealthyWithConfigChange.Annotations[controllers.AnnotationPodSpecHash])
}

func TestGetPodSpecDeferredSidecarFingerprintStableWithMultipleConfigKeys(t *testing.T) {
	mountConfig := "/config"
	deferred := appsv1.SidecarSpec{
		Name:              "indexer",
		Image:             ptr.To("indexer:v1"),
		DeferUntilHealthy: ptr.To(true),
		MountConfig:       &mountConfig,
	}
	configData := map[string]string{
		"app.toml":       "app",
		"client.toml":    "client",
		"config.toml":    "config",
		"consensus.toml": "consensus",
		"genesis.json":   "genesis",
		"mempool.toml":   "mempool",
	}
	fingerprints := map[string]struct{}{}
	for range 30 {
		pod := deferredSidecarPodSpecWithConfigData(t, 0, 1, configData, deferred)
		fingerprints[pod.Annotations[controllers.AnnotationDeferredSidecarFingerprints]] = struct{}{}
	}
	assert.Len(t, fingerprints, 1)
}

func TestGetPodSpecOmittedDeferredSidecarPreservesManagedInitContainerWithSameName(t *testing.T) {
	base := deferredSidecarPodSpec(t, 0)
	deferred := appsv1.SidecarSpec{Name: nodeUtilsContainerName, Image: ptr.To("custom-node-utils:v1"), DeferUntilHealthy: ptr.To(true)}
	desired := deferredSidecarPodSpec(t, 0, deferred)

	var matching []corev1.Container
	for _, container := range desired.Spec.InitContainers {
		if container.Name == nodeUtilsContainerName {
			matching = append(matching, container)
		}
	}
	require.Len(t, matching, 1)
	assert.Equal(t, "node-utils:test", matching[0].Image)
	assert.Equal(t,
		base.Annotations[controllers.AnnotationPodSpecHashWithoutDeferredSidecars],
		desired.Annotations[controllers.AnnotationPodSpecHashWithoutDeferredSidecars],
	)
}

func TestPodSpecChangedAcceptsLegacyDeferredSidecarHash(t *testing.T) {
	deferred := appsv1.SidecarSpec{Name: "indexer", Image: ptr.To("indexer:v1"), DeferUntilHealthy: ptr.To(true)}
	healthy := deferredSidecarPodSpec(t, 1, deferred)
	legacyOmitted := deferredSidecarPodSpec(t, 0, deferred)

	legacyHash, err := podSpecHash(legacyOmitted)
	require.NoError(t, err)
	legacyOmitted.Annotations[controllers.AnnotationPodSpecHash] = legacyHash
	delete(legacyOmitted.Annotations, controllers.AnnotationPodSpecHashWithoutDeferredSidecars)
	require.NotEqual(t, healthy.Annotations[controllers.AnnotationPodSpecHash], legacyHash)
	assert.False(t, podSpecChanged(t.Context(), legacyOmitted, healthy))
}

func TestPodSpecChangedDetectsFirstDeferredSidecarOnAnnotatedPod(t *testing.T) {
	current := deferredSidecarPodSpec(t, 1)
	deferred := appsv1.SidecarSpec{Name: "indexer", Image: ptr.To("indexer:v1"), DeferUntilHealthy: ptr.To(true)}
	desired := deferredSidecarPodSpec(t, 1, deferred)

	require.Contains(t, current.Annotations, controllers.AnnotationPodSpecHashWithoutDeferredSidecars)
	require.NotEqual(t, current.Annotations[controllers.AnnotationPodSpecHash], desired.Annotations[controllers.AnnotationPodSpecHash])
	require.Equal(t, current.Annotations[controllers.AnnotationPodSpecHash], desired.Annotations[controllers.AnnotationPodSpecHashWithoutDeferredSidecars])
	assert.True(t, podSpecChanged(t.Context(), current, desired))
}

func TestPodSpecChangedDetectsDeferredSidecarBecomingRequired(t *testing.T) {
	required := appsv1.SidecarSpec{Name: "indexer", Image: ptr.To("indexer:v1"), DeferUntilHealthy: ptr.To(true)}
	stillDeferred := appsv1.SidecarSpec{Name: "audit", Image: ptr.To("audit:v1"), DeferUntilHealthy: ptr.To(true)}
	current := deferredSidecarPodSpecAtGeneration(t, 0, 1, required, stillDeferred)
	required.DeferUntilHealthy = ptr.To(false)
	desired := deferredSidecarPodSpecAtGeneration(t, 0, 2, required, stillDeferred)

	assert.False(t, hasContainer(current.Spec.InitContainers, required.Name))
	assert.False(t, hasContainer(current.Spec.InitContainers, stillDeferred.Name))
	assert.True(t, hasContainer(desired.Spec.InitContainers, required.Name))
	assert.False(t, hasContainer(desired.Spec.InitContainers, stillDeferred.Name))
	require.Equal(t, current.Annotations[controllers.AnnotationPodSpecHash], desired.Annotations[controllers.AnnotationPodSpecHash])
	assert.Equal(t, "audit,indexer", current.Annotations[controllers.AnnotationDeferredSidecars])
	assert.Equal(t, stillDeferred.Name, desired.Annotations[controllers.AnnotationDeferredSidecars])
	assert.True(t, podSpecChanged(t.Context(), current, desired))
}

func TestPodSpecChangedDetectsOmittedDeferredSidecarWithManagedNameBecomingRequired(t *testing.T) {
	deferred := appsv1.SidecarSpec{Name: nodeUtilsContainerName, Image: ptr.To("custom-node-utils:v1"), DeferUntilHealthy: ptr.To(true)}
	current := deferredSidecarPodSpecAtGeneration(t, 0, 1, deferred)
	required := deferred
	required.DeferUntilHealthy = ptr.To(false)
	desired := deferredSidecarPodSpecAtGeneration(t, 0, 2, required)

	var currentMatching []corev1.Container
	for _, container := range current.Spec.InitContainers {
		if container.Name == nodeUtilsContainerName {
			currentMatching = append(currentMatching, container)
		}
	}
	require.Len(t, currentMatching, 1)
	assert.Equal(t, "node-utils:test", currentMatching[0].Image)
	require.Equal(t, current.Annotations[controllers.AnnotationPodSpecHash], desired.Annotations[controllers.AnnotationPodSpecHash])
	require.Empty(t, current.Annotations[controllers.AnnotationMaterializedDeferredSidecars])
	assert.True(t, podSpecChanged(t.Context(), current, desired))
}

func TestPodSpecChangedDeferredConfigChanges(t *testing.T) {
	tests := []struct {
		name              string
		currentHealthy    int32
		desiredHealthy    int32
		changeNonDeferred bool
		removeDeferred    bool
		wantChanged       bool
	}{
		{name: "omitted deferred-only change stays current", currentHealthy: 0, desiredHealthy: 0, wantChanged: false},
		{name: "healthy deferred-only change requires rollout", currentHealthy: 1, desiredHealthy: 1, wantChanged: true},
		{name: "materialized deferred-only change remains drift when group degrades", currentHealthy: 1, desiredHealthy: 0, wantChanged: true},
		{name: "mixed change while unhealthy requires rollout", currentHealthy: 0, desiredHealthy: 0, changeNonDeferred: true, wantChanged: true},
		{name: "removing a running deferred sidecar requires rollout", currentHealthy: 1, desiredHealthy: 0, removeDeferred: true, wantChanged: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldDeferred := appsv1.SidecarSpec{Name: "indexer", Image: ptr.To("indexer:v1"), Args: []string{"--old"}, DeferUntilHealthy: ptr.To(true)}
			newDeferred := appsv1.SidecarSpec{Name: "indexer", Image: ptr.To("indexer:v2"), Args: []string{"--new"}, DeferUntilHealthy: ptr.To(true)}
			currentSidecars := []appsv1.SidecarSpec{oldDeferred}
			desiredSidecars := []appsv1.SidecarSpec{newDeferred}
			if tt.removeDeferred {
				stillDeferred := appsv1.SidecarSpec{Name: "audit", Image: ptr.To("audit:v1"), DeferUntilHealthy: ptr.To(true)}
				currentSidecars = []appsv1.SidecarSpec{oldDeferred, stillDeferred}
				desiredSidecars = []appsv1.SidecarSpec{stillDeferred}
			}
			if tt.changeNonDeferred {
				currentSidecars = append(currentSidecars, appsv1.SidecarSpec{Name: "metrics", Image: ptr.To("metrics:v1")})
				desiredSidecars = append(desiredSidecars, appsv1.SidecarSpec{Name: "metrics", Image: ptr.To("metrics:v2")})
			}

			current := deferredSidecarPodSpecAtGeneration(t, tt.currentHealthy, 1, currentSidecars...)
			desired := deferredSidecarPodSpecAtGeneration(t, tt.desiredHealthy, 2, desiredSidecars...)
			require.NotEqual(t, current.Annotations[controllers.AnnotationPodSpecHash], desired.Annotations[controllers.AnnotationPodSpecHash])
			if tt.changeNonDeferred {
				require.NotEqual(t, current.Annotations[controllers.AnnotationPodSpecHashWithoutDeferredSidecars], desired.Annotations[controllers.AnnotationPodSpecHashWithoutDeferredSidecars])
			} else {
				require.Equal(t, current.Annotations[controllers.AnnotationPodSpecHashWithoutDeferredSidecars], desired.Annotations[controllers.AnnotationPodSpecHashWithoutDeferredSidecars])
			}
			assert.Equal(t, tt.wantChanged, podSpecChanged(t.Context(), current, desired))
			if !tt.wantChanged {
				require.True(t, syncPodSpecAnnotations(current, desired))
				assert.Equal(t, desired.Annotations[controllers.AnnotationPodSpecHash], current.Annotations[controllers.AnnotationPodSpecHash])
			}
		})
	}
}

func TestPodSpecChangedIgnoresRemovalOfSoleOmittedDeferredSidecar(t *testing.T) {
	deferred := appsv1.SidecarSpec{Name: "indexer", Image: ptr.To("indexer:v1"), DeferUntilHealthy: ptr.To(true)}
	current := deferredSidecarPodSpecAtGeneration(t, 0, 1, deferred)
	desired := deferredSidecarPodSpecAtGeneration(t, 0, 2)

	require.False(t, hasContainer(current.Spec.InitContainers, deferred.Name))
	require.NotEqual(t, current.Annotations[controllers.AnnotationPodSpecHash], desired.Annotations[controllers.AnnotationPodSpecHash])
	require.Equal(t,
		current.Annotations[controllers.AnnotationPodSpecHashWithoutDeferredSidecars],
		desired.Annotations[controllers.AnnotationPodSpecHashWithoutDeferredSidecars],
	)
	assert.False(t, podSpecChanged(t.Context(), current, desired))
}

func TestPodSpecChangedIgnoresChangesToDifferentOmittedDeferredSidecar(t *testing.T) {
	running := appsv1.SidecarSpec{Name: "indexer", Image: ptr.To("indexer:v1"), DeferUntilHealthy: ptr.To(true)}
	oldOmitted := appsv1.SidecarSpec{Name: "audit", Image: ptr.To("audit:v1"), DeferUntilHealthy: ptr.To(true)}
	newOmitted := appsv1.SidecarSpec{Name: "audit", Image: ptr.To("audit:v2"), DeferUntilHealthy: ptr.To(true)}

	tests := []struct {
		name    string
		current *corev1.Pod
		desired *corev1.Pod
	}{
		{
			name:    "new omitted sidecar",
			current: deferredSidecarPodSpecAtGeneration(t, 1, 1, running),
			desired: deferredSidecarPodSpecAtGeneration(t, 0, 2, running, newOmitted),
		},
		{
			name: "changed omitted sidecar",
			current: func() *corev1.Pod {
				pod := deferredSidecarPodSpecAtGeneration(t, 1, 1, running, oldOmitted)
				pod.Spec.InitContainers = withoutContainerNamed(pod.Spec.InitContainers, oldOmitted.Name)
				pod.Annotations[controllers.AnnotationMaterializedDeferredSidecars] = running.Name
				return pod
			}(),
			desired: deferredSidecarPodSpecAtGeneration(t, 0, 2, running, newOmitted),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.True(t, hasContainer(tt.current.Spec.InitContainers, running.Name))
			require.False(t, hasContainer(tt.current.Spec.InitContainers, newOmitted.Name))
			require.False(t, hasContainer(tt.desired.Spec.InitContainers, running.Name))
			require.False(t, hasContainer(tt.desired.Spec.InitContainers, newOmitted.Name))
			require.Equal(t,
				tt.current.Annotations[controllers.AnnotationPodSpecHashWithoutDeferredSidecars],
				tt.desired.Annotations[controllers.AnnotationPodSpecHashWithoutDeferredSidecars],
			)
			require.False(t, podSpecChanged(t.Context(), tt.current, tt.desired))
			materialized := tt.current.Annotations[controllers.AnnotationMaterializedDeferredSidecars]
			require.True(t, syncPodSpecAnnotations(tt.current, tt.desired))
			assert.Equal(t, materialized, tt.current.Annotations[controllers.AnnotationMaterializedDeferredSidecars])
		})
	}
}

func TestPodSpecChangedHandlesDeferredSidecarReordering(t *testing.T) {
	a := appsv1.SidecarSpec{Name: "a", Image: ptr.To("a:v1"), DeferUntilHealthy: ptr.To(true)}
	b := appsv1.SidecarSpec{Name: "b", Image: ptr.To("b:v1"), DeferUntilHealthy: ptr.To(true)}
	c := appsv1.SidecarSpec{Name: "c", Image: ptr.To("c:v1"), DeferUntilHealthy: ptr.To(true)}
	metrics := appsv1.SidecarSpec{Name: "metrics", Image: ptr.To("metrics:v1")}

	tests := []struct {
		name    string
		current *corev1.Pod
		desired *corev1.Pod
		want    bool
	}{
		{
			name:    "all omitted",
			current: deferredSidecarPodSpecAtGeneration(t, 0, 1, a, b, c),
			desired: deferredSidecarPodSpecAtGeneration(t, 0, 2, a, c, b),
		},
		{
			name: "unrelated running sidecar",
			current: func() *corev1.Pod {
				pod := deferredSidecarPodSpecAtGeneration(t, 1, 1, a, b, c)
				pod.Spec.InitContainers = withoutContainerNamed(pod.Spec.InitContainers, b.Name)
				pod.Spec.InitContainers = withoutContainerNamed(pod.Spec.InitContainers, c.Name)
				pod.Annotations[controllers.AnnotationMaterializedDeferredSidecars] = a.Name
				return pod
			}(),
			desired: deferredSidecarPodSpecAtGeneration(t, 0, 2, a, c, b),
		},
		{
			name:    "materialized reordered sidecars",
			current: deferredSidecarPodSpecAtGeneration(t, 1, 1, b, c),
			desired: deferredSidecarPodSpecAtGeneration(t, 1, 2, c, b),
			want:    true,
		},
		{
			name:    "materialized deferred sidecar crosses non-deferred sidecar",
			current: deferredSidecarPodSpecAtGeneration(t, 1, 1, a, metrics),
			desired: deferredSidecarPodSpecAtGeneration(t, 1, 2, metrics, a),
			want:    true,
		},
		{
			name:    "omitted deferred sidecar crosses non-deferred sidecar",
			current: deferredSidecarPodSpecAtGeneration(t, 0, 1, a, metrics),
			desired: deferredSidecarPodSpecAtGeneration(t, 0, 2, metrics, a),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.NotEqual(t, tt.current.Annotations[controllers.AnnotationPodSpecHash], tt.desired.Annotations[controllers.AnnotationPodSpecHash])
			require.Equal(t,
				tt.current.Annotations[controllers.AnnotationPodSpecHashWithoutDeferredSidecars],
				tt.desired.Annotations[controllers.AnnotationPodSpecHashWithoutDeferredSidecars],
			)
			assert.Equal(t, tt.want, podSpecChanged(t.Context(), tt.current, tt.desired))
		})
	}
}

func TestPodSpecChangedDetectsMaterializedDeferredSidecarChanges(t *testing.T) {
	oldMaterialized := appsv1.SidecarSpec{
		Name:              "indexer",
		Image:             ptr.To("indexer:v1"),
		DeferUntilHealthy: ptr.To(true),
	}
	changedMaterialized := oldMaterialized
	changedMaterialized.Image = ptr.To("indexer:v2")
	stillDeferred := appsv1.SidecarSpec{Name: "audit", Image: ptr.To("audit:v1"), DeferUntilHealthy: ptr.To(true)}

	tests := []struct {
		name            string
		desiredSidecars []appsv1.SidecarSpec
	}{
		{name: "changed materialized sidecar", desiredSidecars: []appsv1.SidecarSpec{changedMaterialized, stillDeferred}},
		{name: "removed materialized sidecar", desiredSidecars: []appsv1.SidecarSpec{stillDeferred}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			current := deferredSidecarPodSpecAtGeneration(t, 1, 1, oldMaterialized, stillDeferred)
			desired := deferredSidecarPodSpecAtGeneration(t, 0, 2, tt.desiredSidecars...)

			require.Equal(t,
				current.Annotations[controllers.AnnotationPodSpecHashWithoutDeferredSidecars],
				desired.Annotations[controllers.AnnotationPodSpecHashWithoutDeferredSidecars],
			)
			assert.True(t, podSpecChanged(t.Context(), current, desired))
		})
	}
}

func TestPodSpecChangedDetectsFirstDeferredSidecarOnLegacyNewGeneration(t *testing.T) {
	legacy := deferredSidecarPodSpecAtGeneration(t, 1, 1)
	delete(legacy.Annotations, controllers.AnnotationPodSpecHashWithoutDeferredSidecars)
	deferred := appsv1.SidecarSpec{Name: "indexer", Image: ptr.To("indexer:v1"), DeferUntilHealthy: ptr.To(true)}
	desired := deferredSidecarPodSpecAtGeneration(t, 1, 2, deferred)

	require.Equal(t, legacy.Annotations[controllers.AnnotationPodSpecHash], desired.Annotations[controllers.AnnotationPodSpecHashWithoutDeferredSidecars])
	assert.True(t, podSpecChanged(t.Context(), legacy, desired))
}

func TestSyncPodSpecAnnotationsMigratesAcceptedLegacyDeferredSidecar(t *testing.T) {
	deferred := appsv1.SidecarSpec{Name: "indexer", Image: ptr.To("indexer:v1"), DeferUntilHealthy: ptr.To(true)}
	desired := deferredSidecarPodSpecAtGeneration(t, 0, 1, deferred)
	require.Contains(t, desired.Annotations, controllers.AnnotationDeferredSidecarFingerprints)
	legacy := desired.DeepCopy()
	legacyHash, err := podSpecHash(legacy)
	require.NoError(t, err)
	legacy.Annotations[controllers.AnnotationPodSpecHash] = legacyHash
	delete(legacy.Annotations, controllers.AnnotationPodSpecHashWithoutDeferredSidecars)
	delete(legacy.Annotations, controllers.AnnotationDeferredSidecars)
	delete(legacy.Annotations, controllers.AnnotationConfiguredSidecarOrder)
	delete(legacy.Annotations, controllers.AnnotationDeferredSidecarFingerprints)
	delete(legacy.Annotations, controllers.AnnotationMaterializedDeferredSidecars)
	require.False(t, podSpecChanged(t.Context(), legacy, desired))
	require.True(t, syncPodSpecAnnotations(legacy, desired))

	for _, annotation := range []string{
		controllers.AnnotationPodSpecHash,
		controllers.AnnotationPodSpecHashWithoutDeferredSidecars,
		controllers.AnnotationDeferredSidecars,
		controllers.AnnotationConfiguredSidecarOrder,
		controllers.AnnotationDeferredSidecarFingerprints,
	} {
		assert.Equal(t, desired.Annotations[annotation], legacy.Annotations[annotation])
	}
	assert.Equal(t, "", legacy.Annotations[controllers.AnnotationMaterializedDeferredSidecars])
	later := desired.DeepCopy()
	later.Annotations[controllers.AnnotationChainNodeGeneration] = "2"
	assert.False(t, podSpecChanged(t.Context(), legacy, later))
	changedDeferred := deferred
	changedDeferred.Image = ptr.To("indexer:v2")
	changed := deferredSidecarPodSpecAtGeneration(t, 0, 2, changedDeferred)
	assert.False(t, podSpecChanged(t.Context(), legacy, changed))
}

func TestSyncPodSpecAnnotationsBackfillsRunningLegacyDeferredSidecars(t *testing.T) {
	running := appsv1.SidecarSpec{Name: "indexer", Image: ptr.To("indexer:v1"), DeferUntilHealthy: ptr.To(true)}
	desired := deferredSidecarPodSpecAtGeneration(t, 1, 1, running)
	legacy := desired.DeepCopy()
	legacyHash, err := podSpecHash(legacy)
	require.NoError(t, err)
	legacy.Annotations[controllers.AnnotationPodSpecHash] = legacyHash
	delete(legacy.Annotations, controllers.AnnotationPodSpecHashWithoutDeferredSidecars)
	delete(legacy.Annotations, controllers.AnnotationDeferredSidecars)
	delete(legacy.Annotations, controllers.AnnotationConfiguredSidecarOrder)
	delete(legacy.Annotations, controllers.AnnotationDeferredSidecarFingerprints)
	delete(legacy.Annotations, controllers.AnnotationMaterializedDeferredSidecars)

	require.False(t, podSpecChanged(t.Context(), legacy, desired))
	require.True(t, syncPodSpecAnnotations(legacy, desired))
	assert.Equal(t, running.Name, legacy.Annotations[controllers.AnnotationMaterializedDeferredSidecars])
	required := running
	required.DeferUntilHealthy = ptr.To(false)
	withoutDeferral := deferredSidecarPodSpecAtGeneration(t, 0, 2, required)
	assert.False(t, podSpecChanged(t.Context(), legacy, withoutDeferral))

	newOmitted := appsv1.SidecarSpec{Name: "audit", Image: ptr.To("audit:v1"), DeferUntilHealthy: ptr.To(true)}
	added := deferredSidecarPodSpecAtGeneration(t, 0, 2, running, newOmitted)
	require.False(t, podSpecChanged(t.Context(), legacy, added))
	require.True(t, syncPodSpecAnnotations(legacy, added))
	newOmitted.Image = ptr.To("audit:v2")
	changed := deferredSidecarPodSpecAtGeneration(t, 0, 3, running, newOmitted)
	assert.False(t, podSpecChanged(t.Context(), legacy, changed))
}

func TestSyncPodSpecAnnotationsDoesNotBackfillAmbiguousLegacyDeferredSidecar(t *testing.T) {
	deferred := appsv1.SidecarSpec{
		Name:              nodeUtilsContainerName,
		Image:             ptr.To("custom-node-utils:v1"),
		DeferUntilHealthy: ptr.To(true),
	}
	desired := deferredSidecarPodSpecAtGeneration(t, 0, 1, deferred)
	legacy := desired.DeepCopy()
	legacyHash, err := podSpecHash(legacy)
	require.NoError(t, err)
	legacy.Annotations[controllers.AnnotationPodSpecHash] = legacyHash
	delete(legacy.Annotations, controllers.AnnotationPodSpecHashWithoutDeferredSidecars)
	delete(legacy.Annotations, controllers.AnnotationDeferredSidecars)
	delete(legacy.Annotations, controllers.AnnotationConfiguredSidecarOrder)
	delete(legacy.Annotations, controllers.AnnotationDeferredSidecarFingerprints)
	delete(legacy.Annotations, controllers.AnnotationMaterializedDeferredSidecars)

	require.False(t, podSpecChanged(t.Context(), legacy, desired))
	require.True(t, syncPodSpecAnnotations(legacy, desired))
	assert.NotContains(t, legacy.Annotations, controllers.AnnotationMaterializedDeferredSidecars)

	required := deferred
	required.DeferUntilHealthy = ptr.To(false)
	withoutDeferral := deferredSidecarPodSpecAtGeneration(t, 0, 2, required)
	assert.True(t, podSpecChanged(t.Context(), legacy, withoutDeferral))
}

func TestSyncPodSpecAnnotationsLeavesHealthyLegacyCollisionMaterializationUnknown(t *testing.T) {
	deferred := appsv1.SidecarSpec{
		Name:              nodeUtilsContainerName,
		Image:             ptr.To("custom-node-utils:v1"),
		DeferUntilHealthy: ptr.To(true),
	}
	legacy := deferredSidecarPodSpecAtGeneration(t, 0, 1, deferred)
	legacyHash, err := podSpecHash(legacy)
	require.NoError(t, err)
	legacy.Annotations[controllers.AnnotationPodSpecHash] = legacyHash
	delete(legacy.Annotations, controllers.AnnotationPodSpecHashWithoutDeferredSidecars)
	delete(legacy.Annotations, controllers.AnnotationDeferredSidecars)
	delete(legacy.Annotations, controllers.AnnotationConfiguredSidecarOrder)
	delete(legacy.Annotations, controllers.AnnotationDeferredSidecarFingerprints)
	delete(legacy.Annotations, controllers.AnnotationMaterializedDeferredSidecars)
	desired := deferredSidecarPodSpecAtGeneration(t, 1, 1, deferred)

	require.Len(t, containersNamed(legacy.Spec.InitContainers, nodeUtilsContainerName), 1)
	require.Len(t, containersNamed(desired.Spec.InitContainers, nodeUtilsContainerName), 2)
	require.False(t, podSpecChanged(t.Context(), legacy, desired))
	require.True(t, syncPodSpecAnnotations(legacy, desired))
	assert.NotContains(t, legacy.Annotations, controllers.AnnotationMaterializedDeferredSidecars)

	changed := deferred
	changed.Image = ptr.To("custom-node-utils:v2")
	required := deferred
	required.DeferUntilHealthy = ptr.To(false)
	for _, tt := range []struct {
		name string
		next *corev1.Pod
	}{
		{name: "changed", next: deferredSidecarPodSpecAtGeneration(t, 1, 2, changed)},
		{name: "removed", next: deferredSidecarPodSpecAtGeneration(t, 1, 2)},
		{name: "deferral removed", next: deferredSidecarPodSpecAtGeneration(t, 1, 2, required)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			assert.True(t, podSpecChanged(t.Context(), legacy, tt.next))
		})
	}
}

func TestSyncPodSpecAnnotationsTracksSidecarMaterializedBeforeDeferral(t *testing.T) {
	required := appsv1.SidecarSpec{Name: "indexer", Image: ptr.To("indexer:v1")}
	deferred := required
	deferred.DeferUntilHealthy = ptr.To(true)
	changed := deferred
	changed.Image = ptr.To("indexer:v2")

	tests := []struct {
		name string
		next *corev1.Pod
	}{
		{name: "changed deferred sidecar", next: deferredSidecarPodSpecAtGeneration(t, 0, 3, changed)},
		{name: "removed deferred sidecar", next: deferredSidecarPodSpecAtGeneration(t, 0, 3)},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			current := deferredSidecarPodSpecAtGeneration(t, 0, 1, required)
			desired := deferredSidecarPodSpecAtGeneration(t, 0, 2, deferred)

			require.True(t, hasContainer(current.Spec.InitContainers, required.Name))
			require.False(t, hasContainer(desired.Spec.InitContainers, deferred.Name))
			require.Equal(t,
				current.Annotations[controllers.AnnotationPodSpecHash],
				desired.Annotations[controllers.AnnotationPodSpecHash],
			)
			require.False(t, podSpecChanged(t.Context(), current, desired))
			require.True(t, syncPodSpecAnnotations(current, desired))
			assert.Equal(t, deferred.Name, current.Annotations[controllers.AnnotationMaterializedDeferredSidecars])
			assert.True(t, podSpecChanged(t.Context(), current, tt.next))
		})
	}
}

func TestSyncPodSpecAnnotationsAddsEmptyDeferredSidecarsMarker(t *testing.T) {
	existing := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
		controllers.AnnotationPodSpecHash:                        "canonical",
		controllers.AnnotationPodSpecHashWithoutDeferredSidecars: "compatible",
	}}}
	desired := existing.DeepCopy()
	desired.Annotations[controllers.AnnotationDeferredSidecars] = ""
	desired.Annotations[controllers.AnnotationDeferredSidecarFingerprints] = "{}"
	desired.Annotations[controllers.AnnotationMaterializedDeferredSidecars] = ""

	assert.True(t, syncPodSpecAnnotations(existing, desired))
	assert.Contains(t, existing.Annotations, controllers.AnnotationDeferredSidecars)
	assert.Equal(t, "{}", existing.Annotations[controllers.AnnotationDeferredSidecarFingerprints])
	assert.NotContains(t, existing.Annotations, controllers.AnnotationMaterializedDeferredSidecars)
}

func deferredSidecarPodSpec(t *testing.T, currentHealthy int32, sidecars ...appsv1.SidecarSpec) *corev1.Pod {
	t.Helper()
	return deferredSidecarPodSpecAtGeneration(t, currentHealthy, 1, sidecars...)
}

func deferredSidecarPodSpecAtGeneration(t *testing.T, currentHealthy int32, generation int64, sidecars ...appsv1.SidecarSpec) *corev1.Pod {
	t.Helper()
	return deferredSidecarPodSpecWithConfigData(t, currentHealthy, generation, nil, sidecars...)
}

func deferredSidecarPodSpecWithConfigData(t *testing.T, currentHealthy int32, generation int64, configData map[string]string, sidecars ...appsv1.SidecarSpec) *corev1.Pod {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, policyv1.AddToScheme(scheme))

	chainNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "archive-0",
			Namespace:  "default",
			UID:        "node-uid",
			Generation: generation,
			Labels:     map[string]string{controllers.LabelChainNodeSetGroup: "archive"},
		},
		Spec: appsv1.ChainNodeSpec{
			App:    appsv1.AppSpec{Image: "app", Version: ptr.To("v1"), App: "appd"},
			Config: &appsv1.Config{Sidecars: sidecars},
		},
		Status: appsv1.ChainNodeStatus{ChainID: "chain", NodeID: "node-id"},
	}
	config := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: chainNode.Name, Namespace: chainNode.Namespace},
		Data:       configData,
	}
	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "archive", Namespace: chainNode.Namespace},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector: &metav1.LabelSelector{MatchLabels: map[string]string{controllers.LabelChainNodeSetGroup: "archive"}},
		},
		Status: policyv1.PodDisruptionBudgetStatus{CurrentHealthy: currentHealthy, DesiredHealthy: 1},
	}
	reconciler := &Reconciler{
		Client: fakeclient.NewClientBuilder().WithScheme(scheme).WithObjects(config, pdb).Build(),
		Scheme: scheme,
		opts:   &controllers.ControllerRunOptions{NodeUtilsImage: "node-utils:test"},
	}

	pod, err := reconciler.getPodSpec(t.Context(), chainNode, "config-hash", "shutdown-secret")
	require.NoError(t, err)
	return pod
}

func hasContainer(containers []corev1.Container, name string) bool {
	for _, container := range containers {
		if container.Name == name {
			return true
		}
	}
	return false
}

func containersNamed(containers []corev1.Container, name string) []corev1.Container {
	var matching []corev1.Container
	for _, container := range containers {
		if container.Name == name {
			matching = append(matching, container)
		}
	}
	return matching
}

func withoutContainerNamed(containers []corev1.Container, name string) []corev1.Container {
	filtered := make([]corev1.Container, 0, len(containers))
	for _, container := range containers {
		if container.Name != name {
			filtered = append(filtered, container)
		}
	}
	return filtered
}

func TestOrderVolumes(t *testing.T) {
	tests := []struct {
		name     string
		podSpec  *corev1.PodSpec
		expected []string // Expected order of volume names
	}{
		{
			name: "volumes get sorted",
			podSpec: &corev1.PodSpec{
				Volumes: []corev1.Volume{
					{Name: "zebra"},
					{Name: "apple"},
					{Name: "banana"},
				},
			},
			expected: []string{"apple", "banana", "zebra"},
		},
		{
			name: "already sorted volumes stay sorted",
			podSpec: &corev1.PodSpec{
				Volumes: []corev1.Volume{
					{Name: "a"},
					{Name: "b"},
					{Name: "c"},
				},
			},
			expected: []string{"a", "b", "c"},
		},
		{
			name: "empty volumes",
			podSpec: &corev1.PodSpec{
				Volumes: []corev1.Volume{},
			},
			expected: []string{},
		},
		{
			name: "single volume",
			podSpec: &corev1.PodSpec{
				Volumes: []corev1.Volume{
					{Name: "only"},
				},
			},
			expected: []string{"only"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			orderVolumes(tt.podSpec)
			if len(tt.podSpec.Volumes) != len(tt.expected) {
				t.Errorf("orderVolumes() resulted in %d volumes, want %d", len(tt.podSpec.Volumes), len(tt.expected))
				return
			}
			for i, vol := range tt.podSpec.Volumes {
				if vol.Name != tt.expected[i] {
					t.Errorf("orderVolumes() volume[%d] = %s, want %s", i, vol.Name, tt.expected[i])
				}
			}
		})
	}
}

func TestIsImagePullFailure(t *testing.T) {
	tests := []struct {
		name  string
		state *corev1.ContainerStateWaiting
		want  bool
	}{
		{
			name: "ImagePullBackOff",
			state: &corev1.ContainerStateWaiting{
				Reason: "ImagePullBackOff",
			},
			want: true,
		},
		{
			name: "ErrImagePull",
			state: &corev1.ContainerStateWaiting{
				Reason: "ErrImagePull",
			},
			want: true,
		},
		{
			name: "Other reason",
			state: &corev1.ContainerStateWaiting{
				Reason: "CrashLoopBackOff",
			},
			want: false,
		},
		{
			name:  "nil state",
			state: nil,
			want:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isImagePullFailure(tt.state); got != tt.want {
				t.Errorf("isImagePullFailure() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFailedPodRequiresEarlyRecreation(t *testing.T) {
	chainNode := &appsv1.ChainNode{}
	failedApp := corev1.ContainerStatus{
		Name:  "app",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}},
	}
	runningNodeUtils := corev1.ContainerStatus{
		Name:  nodeUtilsContainerName,
		Ready: false, // /must_upgrade returns 426 while the sidecar remains available.
		State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
	}
	stoppedNodeUtils := corev1.ContainerStatus{
		Name:  nodeUtilsContainerName,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
	}

	upgradeCandidate := &corev1.Pod{Status: corev1.PodStatus{
		ContainerStatuses:     []corev1.ContainerStatus{failedApp},
		InitContainerStatuses: []corev1.ContainerStatus{runningNodeUtils},
	}}
	if failedPodRequiresEarlyRecreation(chainNode, upgradeCandidate) {
		t.Fatal("failed app with live node-utils must reach the upgrade probe before recreation")
	}

	ordinaryFailure := upgradeCandidate.DeepCopy()
	ordinaryFailure.Status.InitContainerStatuses = []corev1.ContainerStatus{stoppedNodeUtils}
	if !failedPodRequiresEarlyRecreation(chainNode, ordinaryFailure) {
		t.Fatal("failed app with stopped node-utils must be recreated before HTTP probes")
	}
}

func TestLogFailedContainerOnlyFetchesLogsForTerminatedContainer(t *testing.T) {
	var requests atomic.Int32
	httpClient := &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		requests.Add(1)
		if got := r.URL.Query().Get("container"); got != "chaind" {
			t.Errorf("container query = %q, want chaind", got)
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("application failed")),
		}, nil
	})}

	clientSet, err := kubernetes.NewForConfigAndClient(&rest.Config{Host: "https://kubernetes.invalid"}, httpClient)
	if err != nil {
		t.Fatal(err)
	}
	r := &Reconciler{ClientSet: clientSet}

	tests := []struct {
		name         string
		pod          *corev1.Pod
		wantRequests int32
	}{
		{
			name: "pure discovery gate failure skips app logs",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "node", Namespace: "default"},
				Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{{
					Name: CosmosignerDiscoveryWaitContainerName,
					State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
						ExitCode: 1,
					}},
				}}},
			},
		},
		{
			name: "terminated application preserves logs",
			pod: &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "node", Namespace: "default"},
				Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
					Name: "chaind",
					State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
						ExitCode: 1,
					}},
				}}},
			},
			wantRequests: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := requests.Load()
			r.logFailedContainer(context.Background(), logr.Discard(), tt.pod, "chaind")
			if got := requests.Load() - before; got != tt.wantRequests {
				t.Fatalf("log requests = %d, want %d", got, tt.wantRequests)
			}
		})
	}
}

func TestNodeUtilsIsRunning(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{
			name: "restartable init sidecar is ready and running",
			pod: &corev1.Pod{Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{{
				Name:  nodeUtilsContainerName,
				Ready: true,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}}}},
			want: true,
		},
		{
			name: "restartable init sidecar is unready for a pending upgrade but still running",
			pod: &corev1.Pod{Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{{
				Name:  nodeUtilsContainerName,
				Ready: false,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}}}},
			want: true,
		},
		{
			name: "restartable init sidecar has stopped",
			pod: &corev1.Pod{Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{{
				Name:  nodeUtilsContainerName,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
			}}}},
			want: false,
		},
		{
			name: "regular container status does not count",
			pod: &corev1.Pod{Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
				Name:  nodeUtilsContainerName,
				Ready: true,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
			}}}},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nodeUtilsIsRunning(tt.pod); got != tt.want {
				t.Errorf("nodeUtilsIsRunning() = %v, want %v", got, tt.want)
			}
		})
	}
}
