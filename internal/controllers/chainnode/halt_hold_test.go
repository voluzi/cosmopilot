package chainnode

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/pkg/nodeutils"
)

func TestTerminalPodRecoveryRequiresBoundHaltEvidence(t *testing.T) {
	for _, tt := range []struct {
		name           string
		cachedHeight   int64
		podHaltHeight  int64
		evidenceTarget int64
		forced         bool
		conflictingEnv bool
		appExitCode    int32
		appReason      string
		appLogs        string
		wantAction     terminalPodRecoveryAction
		wantHold       string
	}{
		{
			name:           "clean native halt recovers despite cached H minus two",
			cachedHeight:   98,
			podHaltHeight:  100,
			evidenceTarget: 100,
			appLogs:        sdkHaltJSON(100, 0),
			wantAction:     terminalPodHold,
			wantHold:       "100",
		},
		{
			name:           "OOM at H minus one is recreated",
			cachedHeight:   99,
			podHaltHeight:  100,
			evidenceTarget: 100,
			appExitCode:    137,
			appReason:      "OOMKilled",
			appLogs:        sdkHaltJSON(100, 0),
			wantAction:     terminalPodRestart,
		},
		{
			name:           "nonzero error at H minus one is recreated",
			cachedHeight:   99,
			podHaltHeight:  100,
			evidenceTarget: 100,
			appExitCode:    1,
			appReason:      "Error",
			appLogs:        sdkHaltJSON(100, 0),
			wantAction:     terminalPodRestart,
		},
		{
			name:           "forced shutdown is recreated",
			cachedHeight:   99,
			podHaltHeight:  100,
			evidenceTarget: 100,
			forced:         true,
			appLogs:        sdkHaltJSON(100, 0),
			wantAction:     terminalPodRestart,
		},
		{
			name:           "different pod halt target is recreated",
			cachedHeight:   99,
			podHaltHeight:  99,
			evidenceTarget: 99,
			appLogs:        sdkHaltJSON(100, 0),
			wantAction:     terminalPodRestart,
		},
		{
			name:           "ambiguous pod halt target is recreated",
			cachedHeight:   99,
			podHaltHeight:  100,
			evidenceTarget: 100,
			conflictingEnv: true,
			appLogs:        sdkHaltJSON(100, 0),
			wantAction:     terminalPodRestart,
		},
		{
			name:           "halt-time exit is recreated",
			cachedHeight:   98,
			podHaltHeight:  100,
			evidenceTarget: 100,
			appLogs:        sdkHaltJSON(100, 1700000000),
			wantAction:     terminalPodRestart,
		},
		{
			name:           "direct clean exit without halt log is recreated",
			cachedHeight:   98,
			podHaltHeight:  100,
			evidenceTarget: 100,
			appLogs:        "2026-09-21T12:00:30Z I[2026-09-21|12:00:30.000] caught signal signal=terminated\n",
			wantAction:     terminalPodRestart,
		},
		{
			name:           "wrong logged height is recreated",
			cachedHeight:   98,
			podHaltHeight:  100,
			evidenceTarget: 100,
			appLogs:        sdkHaltJSON(99, 0),
			wantAction:     terminalPodRestart,
		},
		{
			name:           "oversized log tail is recreated",
			cachedHeight:   98,
			podHaltHeight:  100,
			evidenceTarget: 100,
			appLogs:        strings.Repeat("x", appTerminationLogMaxBytes+1),
			wantAction:     terminalPodRestart,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			haltHeight := int64(100)
			node := &appsv1.ChainNode{
				ObjectMeta: metav1.ObjectMeta{Name: "node", Namespace: "default"},
				Spec: appsv1.ChainNodeSpec{
					App:    appsv1.AppSpec{App: "appd"},
					Config: &appsv1.Config{HaltHeight: &haltHeight},
				},
				Status: appsv1.ChainNodeStatus{LatestHeight: tt.cachedHeight},
			}
			pod := terminalEvidencePod(t, tt.podHaltHeight, tt.evidenceTarget, tt.cachedHeight, tt.forced, tt.appExitCode, tt.appReason)
			if tt.conflictingEnv {
				pod.Spec.InitContainers[0].Env = append(pod.Spec.InitContainers[0].Env,
					corev1.EnvVar{Name: "HALT_HEIGHT", Value: "101"})
			}

			reader := func(_ context.Context, _ *corev1.Pod, container string, identity appTerminationIdentity) ([]byte, error) {
				assert.Equal(t, "appd", container)
				assert.Equal(t, types.UID("pod-uid"), identity.PodUID)
				assert.Equal(t, "containerd://app", identity.ContainerID)
				return []byte(tt.appLogs), nil
			}
			assert.Equal(t, tt.wantAction, terminalPodRecoveryFor(t.Context(), node, pod, reader))

			scheme := runtime.NewScheme()
			require.NoError(t, appsv1.AddToScheme(scheme))
			backing := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()
			r := &Reconciler{Client: backing, terminatedAppLogReader: reader}
			require.NoError(t, r.reconcileHaltHeightHold(t.Context(), node, pod))

			stored := &appsv1.ChainNode{}
			require.NoError(t, backing.Get(t.Context(), client.ObjectKeyFromObject(node), stored))
			assert.Equal(t, tt.wantHold, stored.Annotations[appsv1.AnnotationHaltHeightHold])
		})
	}
}

func TestTerminalPodRecoveryUsesOnlyMatchingPendingUpgradeEvidence(t *testing.T) {
	node := &appsv1.ChainNode{
		Spec: appsv1.ChainNodeSpec{App: appsv1.AppSpec{App: "appd"}, Config: &appsv1.Config{}},
		Status: appsv1.ChainNodeStatus{Upgrades: []appsv1.Upgrade{{
			Height: 100,
			Source: appsv1.OnChainUpgrade,
			Status: appsv1.UpgradeScheduled,
		}}},
	}
	pod := terminalEvidencePod(t, 0, 0, 98, false, 1, "Error")
	evidence, ok := nodeUtilsTerminationEvidence(pod)
	require.True(t, ok)
	evidence.RequiredUpgrade = &nodeutils.RequiredUpgrade{Height: 100, Source: nodeutils.OnChainUpgrade}
	body, err := json.Marshal(evidence)
	require.NoError(t, err)
	pod.Status.InitContainerStatuses[0].State.Terminated.Message = string(body)

	reader := func(context.Context, *corev1.Pod, string, appTerminationIdentity) ([]byte, error) {
		return nil, errors.New("upgrade recovery must not require halt logs")
	}
	assert.Equal(t, terminalPodUpgrade, terminalPodRecoveryFor(t.Context(), node, pod, reader))
	node.Status.Upgrades[0].Status = appsv1.UpgradeOnGoing
	assert.Equal(t, terminalPodUpgrade, terminalPodRecoveryFor(t.Context(), node, pod, reader))
	node.Status.Upgrades[0].Status = appsv1.UpgradeCompleted
	assert.Equal(t, terminalPodRestart, terminalPodRecoveryFor(t.Context(), node, pod, reader))
	node.Status.Upgrades[0].Status = appsv1.UpgradeScheduled

	evidence.RequiredUpgrade.Source = nodeutils.ManualUpgrade
	body, err = json.Marshal(evidence)
	require.NoError(t, err)
	pod.Status.InitContainerStatuses[0].State.Terminated.Message = string(body)
	assert.Equal(t, terminalPodRestart, terminalPodRecoveryFor(t.Context(), node, pod, reader))
}

func TestTerminalPodRecoveryWaitsForCurrentSidecarEvidence(t *testing.T) {
	node := &appsv1.ChainNode{Spec: appsv1.ChainNodeSpec{App: appsv1.AppSpec{App: "appd"}, Config: &appsv1.Config{}}}
	pod := terminalEvidencePod(t, 0, 0, 98, false, 137, "OOMKilled")
	pod.Status.InitContainerStatuses[0].State.Terminated = nil
	pod.Status.InitContainerStatuses[0].State.Running = &corev1.ContainerStateRunning{}

	assert.Equal(t, terminalPodWaitForEvidence, terminalPodRecoveryFor(t.Context(), node, pod, nil))
}

func TestTerminalPodRecoveryRetriesLogTransportFailureWithoutDeleting(t *testing.T) {
	haltHeight := int64(100)
	node := &appsv1.ChainNode{Spec: appsv1.ChainNodeSpec{
		App: appsv1.AppSpec{App: "appd"}, Config: &appsv1.Config{HaltHeight: &haltHeight},
	}}
	pod := terminalEvidencePod(t, 100, 100, 98, false, 0, "Completed")
	reader := func(context.Context, *corev1.Pod, string, appTerminationIdentity) ([]byte, error) {
		return nil, errors.New("apiserver unavailable")
	}

	assert.Equal(t, terminalPodRetry, terminalPodRecoveryFor(t.Context(), node, pod, reader))
}

func TestAppTerminationIdentityRejectsPodOrContainerReuse(t *testing.T) {
	pod := terminalEvidencePod(t, 100, 100, 98, false, 0, "Completed")
	identity, ok := currentAppTerminationIdentity(pod, "appd")
	require.True(t, ok)
	assert.True(t, sameAppTerminationIdentity(identity, pod, "appd"))

	changedPod := pod.DeepCopy()
	changedPod.UID = "replacement-pod"
	assert.False(t, sameAppTerminationIdentity(identity, changedPod, "appd"))

	changedContainer := pod.DeepCopy()
	changedContainer.Status.ContainerStatuses[0].State.Terminated.ContainerID = "containerd://replacement"
	assert.False(t, sameAppTerminationIdentity(identity, changedContainer, "appd"))

	changedTimes := pod.DeepCopy()
	changedTimes.Status.ContainerStatuses[0].State.Terminated.FinishedAt = metav1.NewTime(
		changedTimes.Status.ContainerStatuses[0].State.Terminated.FinishedAt.Add(time.Second),
	)
	assert.False(t, sameAppTerminationIdentity(identity, changedTimes, "appd"))
}

func TestReconcileHaltHeightHoldClearsStaleIntent(t *testing.T) {
	for _, tt := range []struct {
		name       string
		haltHeight *int64
	}{
		{name: "target changed", haltHeight: ptr.To[int64](101)},
		{name: "target removed"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			node := &appsv1.ChainNode{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "node",
					Namespace:   "default",
					Annotations: map[string]string{appsv1.AnnotationHaltHeightHold: "100"},
				},
				Spec: appsv1.ChainNodeSpec{
					App:    appsv1.AppSpec{App: "appd"},
					Config: &appsv1.Config{HaltHeight: tt.haltHeight},
				},
			}
			pod := terminalEvidencePod(t, 100, 100, 99, false, 0, "Completed")
			scheme := runtime.NewScheme()
			require.NoError(t, appsv1.AddToScheme(scheme))
			backing := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()
			r := &Reconciler{
				Client: backing,
				terminatedAppLogReader: func(context.Context, *corev1.Pod, string, appTerminationIdentity) ([]byte, error) {
					return []byte(sdkHaltJSON(100, 0)), nil
				},
			}

			require.NoError(t, r.reconcileHaltHeightHold(t.Context(), node, pod))

			stored := &appsv1.ChainNode{}
			require.NoError(t, backing.Get(t.Context(), client.ObjectKeyFromObject(node), stored))
			assert.NotContains(t, stored.Annotations, appsv1.AnnotationHaltHeightHold)
			mustStop, _ := stored.MustStop()
			assert.False(t, mustStop)
		})
	}
}

func TestShouldMigrateLegacyHaltHeightHold(t *testing.T) {
	for _, tt := range []struct {
		name         string
		phase        appsv1.ChainNodePhase
		latest       int64
		haltHeight   *int64
		existingHold string
		want         bool
	}{
		{name: "legacy stopped at target", phase: appsv1.PhaseChainNodeStopped, latest: 100, haltHeight: ptr.To[int64](100), want: true},
		{name: "running phase", phase: appsv1.PhaseChainNodeRunning, latest: 100, haltHeight: ptr.To[int64](100)},
		{name: "restarting phase", phase: appsv1.PhaseChainNodeRestarting, latest: 100, haltHeight: ptr.To[int64](100)},
		{name: "height mismatch", phase: appsv1.PhaseChainNodeStopped, latest: 99, haltHeight: ptr.To[int64](100)},
		{name: "removed target", phase: appsv1.PhaseChainNodeStopped, latest: 100},
		{name: "zero target", phase: appsv1.PhaseChainNodeStopped, haltHeight: ptr.To[int64](0)},
		{name: "matching hold already exists", phase: appsv1.PhaseChainNodeStopped, latest: 100, haltHeight: ptr.To[int64](100), existingHold: "100"},
		{name: "stale hold is not migrated", phase: appsv1.PhaseChainNodeStopped, latest: 100, haltHeight: ptr.To[int64](100), existingHold: "99"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			node := &appsv1.ChainNode{
				Spec:   appsv1.ChainNodeSpec{Config: &appsv1.Config{HaltHeight: tt.haltHeight}},
				Status: appsv1.ChainNodeStatus{Phase: tt.phase, LatestHeight: tt.latest},
			}
			if tt.existingHold != "" {
				node.Annotations = map[string]string{appsv1.AnnotationHaltHeightHold: tt.existingHold}
			}
			assert.Equal(t, tt.want, shouldMigrateLegacyHaltHeightHold(node))
		})
	}
}

func sdkHaltJSON(height, haltTime int64) string {
	return `2026-09-21T12:00:30Z {"level":"info","height":` + strconv.FormatInt(height, 10) +
		`,"time":` + strconv.FormatInt(haltTime, 10) +
		`,"_msg":"halting node per configuration"}` + "\n"
}

func terminalEvidencePod(
	t *testing.T,
	podHaltHeight int64,
	evidenceTarget int64,
	latestHeight int64,
	forced bool,
	appExitCode int32,
	appReason string,
) *corev1.Pod {
	t.Helper()
	evidence := nodeutils.TerminationEvidence{
		UpgradeStatus:  nodeutils.UpgradeStatus{LatestHeight: &latestHeight},
		HaltHeight:     evidenceTarget,
		ForcedShutdown: forced,
	}
	body, err := json.Marshal(evidence)
	require.NoError(t, err)
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{UID: "pod-uid"},
		Spec: corev1.PodSpec{InitContainers: []corev1.Container{{
			Name: nodeUtilsContainerName,
			Env:  []corev1.EnvVar{{Name: "HALT_HEIGHT", Value: strconv.FormatInt(podHaltHeight, 10)}},
		}}},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{{
				Name:        "appd",
				ContainerID: "containerd://app",
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode:    appExitCode,
					Reason:      appReason,
					ContainerID: "containerd://app",
					StartedAt:   metav1.NewTime(time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC)),
					FinishedAt:  metav1.NewTime(time.Date(2026, 9, 21, 12, 1, 0, 0, time.UTC)),
				}},
			}},
			InitContainerStatuses: []corev1.ContainerStatus{{
				Name: nodeUtilsContainerName,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{
					ExitCode: 0,
					Message:  string(body),
				}},
			}},
		},
	}
}
