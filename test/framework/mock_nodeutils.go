package framework

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	cosmopilotv1 "github.com/voluzi/cosmopilot/v3/api/v1"
)

const (
	// NodeUtilsContainer is the name of the node-utils sidecar container
	NodeUtilsContainer = "node-utils"

	// NodeUtilsBinary is the path to the node-utils binary in the container
	NodeUtilsBinary = "/nodeutils"
)

// PodExecer is an interface for executing commands in pods.
// Both Framework and *KindFramework implement this interface.
type PodExecer interface {
	PodExec(namespace, podName, container string, command ...string) (string, error)
}

// MockNodeUtilsHelper provides methods to control node-utils mock mode via kubectl exec.
// This is useful for E2E testing VPA functionality where we need to simulate
// different CPU/memory usage levels.
type MockNodeUtilsHelper struct {
	execer    PodExecer
	namespace string
	podName   string
}

// NewMockNodeUtilsHelper creates a helper for controlling mock mode on a specific pod.
func NewMockNodeUtilsHelper(execer PodExecer, namespace, podName string) *MockNodeUtilsHelper {
	return &MockNodeUtilsHelper{
		execer:    execer,
		namespace: namespace,
		podName:   podName,
	}
}

// NewMockNodeUtilsHelperFromChainNode creates a helper from a ChainNode.
// It retrieves the pod name from the ChainNode status.
func NewMockNodeUtilsHelperFromChainNode(f *KindFramework, cn *cosmopilotv1.ChainNode) (*MockNodeUtilsHelper, error) {
	// Get the pod for this ChainNode
	pod, err := f.KubeClient().CoreV1().Pods(cn.Namespace).Get(
		f.Context(),
		cn.Name+"-0", // ChainNode pod naming convention
		metav1.GetOptions{},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to get pod for ChainNode %s: %w", cn.Name, err)
	}

	return &MockNodeUtilsHelper{
		execer:    f,
		namespace: cn.Namespace,
		podName:   pod.Name,
	}, nil
}

// NewMockNodeUtilsHelperFromPod creates a helper from a Pod.
func NewMockNodeUtilsHelperFromPod(execer PodExecer, pod *corev1.Pod) *MockNodeUtilsHelper {
	return &MockNodeUtilsHelper{
		execer:    execer,
		namespace: pod.Namespace,
		podName:   pod.Name,
	}
}

// containerStater is the half of a framework needed to tell a mock server that went away from one
// that answered wrongly. Every framework that can exec also carries a clientset, but PodExecer does
// not require one, so this is asserted rather than demanded.
type containerStater interface {
	KubeClient() *kubernetes.Clientset
}

// nodeUtilsIncarnation identifies the running instance of the node-utils container. It changes when
// the pod is replaced or the container restarts, and is empty when it cannot be determined at all.
func (h *MockNodeUtilsHelper) nodeUtilsIncarnation() string {
	stater, ok := h.execer.(containerStater)
	if !ok {
		return ""
	}
	pod, err := stater.KubeClient().CoreV1().Pods(h.namespace).Get(context.Background(), h.podName, metav1.GetOptions{})
	if err != nil {
		return ""
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name != NodeUtilsContainer {
			continue
		}
		started := ""
		if status.State.Running != nil {
			started = status.State.Running.StartedAt.String()
		}
		return fmt.Sprintf("%s/%d/%s", pod.UID, status.RestartCount, started)
	}
	return ""
}

// verifySet runs a read-back check, tolerating the pod restart that the write itself may have
// provoked.
//
// The values these specs write are picked to push vertical autoscaling over a threshold, and
// cosmopilot acts on one by replacing the pod — taking node-utils, and with it the mock server the
// read-back talks to, down. A write that causes exactly the behaviour the spec is testing therefore
// races its own verification, in two ways: the read finds nothing listening on port 8000, or it
// reaches a fresh process whose in-memory mock has reset to the default and so disagrees. Both show
// up as the container changing incarnation, and neither means the write failed.
//
// Only the read-back is forgiving. A failed write is still an error, because a spec that carries on
// believing it set a value it did not would fail later and less legibly. And when the incarnation
// cannot be read at all, before and after are both empty and the check stays strict.
func (h *MockNodeUtilsHelper) verifySet(before string, check func() error) error {
	err := check()
	if err == nil {
		return nil
	}
	if after := h.nodeUtilsIncarnation(); after != before {
		fmt.Printf("[MockNodeUtilsHelper] node-utils restarted under the read-back (%q -> %q), treating the write as applied: %v\n", before, after, err)
		return nil
	}
	return err
}

// SetCPUMillicores sets the mock CPU usage in millicores.
// Example: SetCPUMillicores(500) sets CPU to 500m (0.5 cores).
func (h *MockNodeUtilsHelper) SetCPUMillicores(millicores int64) error {
	incarnation := h.nodeUtilsIncarnation()

	output, err := h.execer.PodExec(
		h.namespace,
		h.podName,
		NodeUtilsContainer,
		NodeUtilsBinary, "mock", "set-cpu", strconv.FormatInt(millicores, 10),
	)
	if err != nil {
		return fmt.Errorf("SetCPUMillicores exec failed: %w (output: %s)", err, output)
	}
	// Log output for debugging
	if output != "" {
		fmt.Printf("[MockNodeUtilsHelper] SetCPUMillicores(%d): %s\n", millicores, strings.TrimSpace(output))
	}

	// Verify the value was actually set
	return h.verifySet(incarnation, func() error {
		actualCores, _, err := h.GetStats()
		if err != nil {
			return fmt.Errorf("SetCPUMillicores verification failed: could not read stats: %w", err)
		}

		expectedCores := float64(millicores) / 1000.0
		if actualCores != expectedCores {
			return fmt.Errorf("SetCPUMillicores verification failed: expected %.3f cores but got %.3f cores", expectedCores, actualCores)
		}
		return nil
	})
}

// SetMemoryMiB sets the mock memory usage in MiB.
// Example: SetMemoryMiB(512) sets memory to 512MiB.
func (h *MockNodeUtilsHelper) SetMemoryMiB(mib int64) error {
	incarnation := h.nodeUtilsIncarnation()

	output, err := h.execer.PodExec(
		h.namespace,
		h.podName,
		NodeUtilsContainer,
		NodeUtilsBinary, "mock", "set-memory", strconv.FormatInt(mib, 10),
	)
	if err != nil {
		return fmt.Errorf("SetMemoryMiB exec failed: %w (output: %s)", err, output)
	}
	// Log output for debugging
	if output != "" {
		fmt.Printf("[MockNodeUtilsHelper] SetMemoryMiB(%d): %s\n", mib, strings.TrimSpace(output))
	}

	// Verify the value was actually set
	return h.verifySet(incarnation, func() error {
		_, actualBytes, err := h.GetStats()
		if err != nil {
			return fmt.Errorf("SetMemoryMiB verification failed: could not read stats: %w", err)
		}

		expectedBytes := uint64(mib * 1024 * 1024)
		if actualBytes != expectedBytes {
			return fmt.Errorf("SetMemoryMiB verification failed: expected %d bytes but got %d bytes", expectedBytes, actualBytes)
		}
		return nil
	})
}

// GetStats returns the current mock stats as a map.
func (h *MockNodeUtilsHelper) GetStats() (cpuCores float64, memoryBytes uint64, err error) {
	output, err := h.execer.PodExec(
		h.namespace,
		h.podName,
		NodeUtilsContainer,
		NodeUtilsBinary, "mock", "get",
	)
	if err != nil {
		return 0, 0, err
	}

	// Parse JSON response: {"cpuCores":0.1,"memoryBytes":536870912}
	output = strings.TrimSpace(output)

	// Simple parsing - extract values using string operations
	// This avoids needing to import encoding/json
	if idx := strings.Index(output, `"cpuCores":`); idx >= 0 {
		start := idx + len(`"cpuCores":`)
		end := strings.IndexAny(output[start:], ",}")
		if end > 0 {
			cpuCores, _ = strconv.ParseFloat(output[start:start+end], 64)
		}
	}

	if idx := strings.Index(output, `"memoryBytes":`); idx >= 0 {
		start := idx + len(`"memoryBytes":`)
		end := strings.IndexAny(output[start:], ",}")
		if end > 0 {
			memoryBytes, _ = strconv.ParseUint(output[start:start+end], 10, 64)
		}
	}

	return cpuCores, memoryBytes, nil
}

// SetUsagePercentOfRequest sets mock usage as a percentage of the current request.
// This is useful for testing VPA scale-up/scale-down thresholds.
// Example: SetUsagePercentOfRequest(80, 1000, 512) sets CPU to 80% of 1000m (800m)
// and memory to 80% of 512MiB.
func (h *MockNodeUtilsHelper) SetUsagePercentOfRequest(percent int, cpuRequestMillicores, memoryRequestMiB int64) error {
	cpuMillicores := cpuRequestMillicores * int64(percent) / 100
	if err := h.SetCPUMillicores(cpuMillicores); err != nil {
		return fmt.Errorf("failed to set CPU: %w", err)
	}

	memoryMiB := memoryRequestMiB * int64(percent) / 100
	if err := h.SetMemoryMiB(memoryMiB); err != nil {
		return fmt.Errorf("failed to set memory: %w", err)
	}

	return nil
}

// SimulateHighUsage sets mock usage to trigger scale-up (85% of request).
func (h *MockNodeUtilsHelper) SimulateHighUsage(cpuRequestMillicores, memoryRequestMiB int64) error {
	return h.SetUsagePercentOfRequest(85, cpuRequestMillicores, memoryRequestMiB)
}

// SimulateLowUsage sets mock usage to trigger scale-down (30% of request).
func (h *MockNodeUtilsHelper) SimulateLowUsage(cpuRequestMillicores, memoryRequestMiB int64) error {
	return h.SetUsagePercentOfRequest(30, cpuRequestMillicores, memoryRequestMiB)
}

// SimulateNormalUsage sets mock usage to a stable level (60% of request).
func (h *MockNodeUtilsHelper) SimulateNormalUsage(cpuRequestMillicores, memoryRequestMiB int64) error {
	return h.SetUsagePercentOfRequest(60, cpuRequestMillicores, memoryRequestMiB)
}
