package chainnode

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
)

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

func TestNodeUtilsIsInFailedState(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{
			name: "node-utils running",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name: "node-utils",
							State: corev1.ContainerState{
								Running: &corev1.ContainerStateRunning{
									StartedAt: metav1.Time{Time: time.Now()},
								},
							},
						},
					},
				},
			},
			want: false,
		},
		{
			name: "no node-utils container",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					ContainerStatuses: []corev1.ContainerStatus{
						{
							Name: "app",
							State: corev1.ContainerState{
								Running: &corev1.ContainerStateRunning{},
							},
						},
					},
				},
			},
			want: false,
		},
		{
			name: "failed pod with discovery gate failure",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodFailed,
					InitContainerStatuses: []corev1.ContainerStatus{
						{
							Name: CosmosignerDiscoveryWaitContainerName,
							State: corev1.ContainerState{
								Terminated: &corev1.ContainerStateTerminated{ExitCode: 1},
							},
						},
					},
				},
			},
			want: false,
		},
		{
			name: "discovery failure with node-utils sidecar shutdown",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodFailed,
					InitContainerStatuses: []corev1.ContainerStatus{
						{
							Name:  nodeUtilsContainerName,
							State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
						},
						{
							Name:  CosmosignerDiscoveryWaitContainerName,
							State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}},
						},
					},
				},
			},
			want: false,
		},
		{
			name: "terminated node-utils init container",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Phase: corev1.PodFailed,
					InitContainerStatuses: []corev1.ContainerStatus{
						{
							Name: nodeUtilsContainerName,
							State: corev1.ContainerState{
								Terminated: &corev1.ContainerStateTerminated{ExitCode: 1},
							},
						},
					},
				},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nodeUtilsIsInFailedState(tt.pod); got != tt.want {
				t.Errorf("nodeUtilsIsInFailedState() = %v, want %v", got, tt.want)
			}
		})
	}
}
