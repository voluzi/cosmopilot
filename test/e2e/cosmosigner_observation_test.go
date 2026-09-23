package e2e

import (
	"io"
	"net/http"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

func TestLiveValidatorHeight(t *testing.T) {
	for _, tt := range []struct {
		name, payload string
		want          int64
		wantErr       bool
	}{
		{"valid", `{"jsonrpc":"2.0","result":{"sync_info":{"latest_block_height":"42"}}}`, 42, false},
		{"missing", `{"result":{"sync_info":{}}}`, 0, true},
		{"malformed", `{"result":{"sync_info":{"latest_block_height":"oops"}}}`, 0, true},
		{"negative", `{"result":{"sync_info":{"latest_block_height":"-1"}}}`, 0, true},
		{"zero", `{"result":{"sync_info":{"latest_block_height":"0"}}}`, 0, true},
		{"invalid JSON", `{`, 0, true},
		{"RPC error", `{"error":{"code":-32603,"message":"unavailable"}}`, 0, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			transport := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != "/api/v1/namespaces/ns/pods/http:validator:26657/proxy/status" {
					t.Errorf("proxy path = %q", r.URL.Path)
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(tt.payload)), Header: make(http.Header)}, nil
			})
			kube, err := kubernetes.NewForConfig(&rest.Config{Host: "http://localhost", WrapTransport: func(http.RoundTripper) http.RoundTripper { return transport }})
			if err != nil {
				t.Fatal(err)
			}
			got, err := liveValidatorHeight(t.Context(), kube, "ns", "validator")
			if got != tt.want || (err != nil) != tt.wantErr {
				t.Fatalf("height = %d, err = %v; want %d, error %v", got, err, tt.want, tt.wantErr)
			}
		})
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestHeldSignerContainerTerminated(t *testing.T) {
	deleting := metav1.Now()
	for _, tt := range []struct {
		name string
		pod  corev1.Pod
		want bool
	}{
		{"terminated", corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: types.UID("held"), DeletionTimestamp: &deleting}, Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "cosmosigner", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}}}}}, true},
		{"wrong UID", corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: types.UID("other"), DeletionTimestamp: &deleting}, Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "cosmosigner", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}}}}}, false},
		{"not deleting", corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: types.UID("held")}, Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "cosmosigner", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}}}}}, false},
		{"missing status", corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: types.UID("held"), DeletionTimestamp: &deleting}}, false},
		{"wrong container", corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: types.UID("held"), DeletionTimestamp: &deleting}, Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "sidecar", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}}}}}, false},
		{"unready running", corev1.Pod{ObjectMeta: metav1.ObjectMeta{UID: types.UID("held"), DeletionTimestamp: &deleting}, Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "cosmosigner", Ready: false, State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := heldSignerContainerTerminated(&tt.pod, "held"); got != tt.want {
				t.Errorf("heldSignerContainerTerminated = %v, want %v", got, tt.want)
			}
		})
	}
}
