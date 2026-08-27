package e2e

import (
	"bufio"
	"context"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1 "github.com/voluzi/cosmopilot/v3/api/v1"
	"github.com/voluzi/cosmopilot/v3/test/framework"
)

// RandString generates a random string of the specified length
func RandString(n int) string {
	const letterBytes = "abcdefghijklmnopqrstuvwxyz"
	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	b := make([]byte, n)
	for i := range b {
		b[i] = letterBytes[r.Intn(len(letterBytes))]
	}
	return string(b)
}

// WaitForChainNodeRunning waits for a ChainNode to reach the Running phase
func WaitForChainNodeRunning(chainNode *appsv1.ChainNode) {
	Eventually(func() appsv1.ChainNodePhase {
		current := appsv1.ChainNode{}
		if err := Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(chainNode), &current); err != nil {
			return ""
		}
		return current.Status.Phase
	}).Should(Equal(appsv1.PhaseChainNodeRunning))
}

// WaitForPodReady waits for a pod to have all containers ready
func WaitForPodReady(namespace, name string) {
	Eventually(func() bool {
		pod := &corev1.Pod{}
		if err := Framework().Client().Get(Framework().Context(), client.ObjectKey{Namespace: namespace, Name: name}, pod); err != nil {
			return false
		}
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
				return true
			}
		}
		return false
	}).Should(BeTrue())
}

// WaitForChainNodeHeight waits for a ChainNode to reach the specified height
func WaitForChainNodeHeight(chainNode *appsv1.ChainNode, minHeight int64) {
	Eventually(func() int64 {
		current := appsv1.ChainNode{}
		if err := Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(chainNode), &current); err != nil {
			return 0
		}
		return current.Status.LatestHeight
	}).Should(BeNumerically(">", minHeight))
}

// WaitForChainNodeSetRunning waits for a ChainNodeSet to reach the running phase
func WaitForChainNodeSetRunning(chainNodeSet *appsv1.ChainNodeSet) {
	Eventually(func() appsv1.ChainNodeSetPhase {
		current := appsv1.ChainNodeSet{}
		if err := Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(chainNodeSet), &current); err != nil {
			return ""
		}
		return current.Status.Phase
	}).Should(Equal(appsv1.PhaseChainNodeSetRunning))
}

// WaitForChainNodeSetHeight waits for a ChainNodeSet to reach the specified height
func WaitForChainNodeSetHeight(chainNodeSet *appsv1.ChainNodeSet, minHeight int64) {
	Eventually(func() int64 {
		current := appsv1.ChainNodeSet{}
		if err := Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(chainNodeSet), &current); err != nil {
			return 0
		}
		return current.Status.LatestHeight
	}).Should(BeNumerically(">", minHeight))
}

// RefreshChainNode fetches the latest state of a ChainNode
func RefreshChainNode(chainNode *appsv1.ChainNode) {
	err := Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(chainNode), chainNode)
	Expect(err).NotTo(HaveOccurred())
}

// RefreshChainNodeSet fetches the latest state of a ChainNodeSet
func RefreshChainNodeSet(chainNodeSet *appsv1.ChainNodeSet) {
	err := Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(chainNodeSet), chainNodeSet)
	Expect(err).NotTo(HaveOccurred())
}

// CountChainNodes returns the number of ChainNodes in the namespace
func CountChainNodes(namespace string) int {
	chainNodeList := &appsv1.ChainNodeList{}
	err := Framework().Client().List(Framework().Context(), chainNodeList, &client.ListOptions{Namespace: namespace})
	Expect(err).NotTo(HaveOccurred())
	return len(chainNodeList.Items)
}

// WaitForChainNodeCount waits for the number of ChainNodes in the namespace to equal the expected count
func WaitForChainNodeCount(namespace string, expectedCount int) {
	Eventually(func() int {
		chainNodeList := &appsv1.ChainNodeList{}
		if err := Framework().Client().List(Framework().Context(), chainNodeList, &client.ListOptions{Namespace: namespace}); err != nil {
			return -1
		}
		return len(chainNodeList.Items)
	}).Should(Equal(expectedCount))
}

// WaitForChainNodesHeight waits for all ChainNodes in a ChainNodeSet to reach the minimum height
func WaitForChainNodesHeight(chainNodeSet *appsv1.ChainNodeSet, minHeight int64) {
	// Refresh the ChainNodeSet to get the latest status
	RefreshChainNodeSet(chainNodeSet)

	// Collect all node names
	var nodeNames []string

	// Add validator if present
	if chainNodeSet.Spec.Validator != nil {
		nodeNames = append(nodeNames, chainNodeSet.Name+"-validator")
	}

	// Add all nodes from status
	for _, node := range chainNodeSet.Status.Nodes {
		nodeNames = append(nodeNames, node.Name)
	}

	// Wait for each node to reach the minimum height. The description is a func so it is rendered
	// from the node's final state rather than its state when the wait started; without it a stalled
	// chain reports a bare height and not even which of the nodes stopped.
	for _, name := range nodeNames {
		Eventually(func() int64 {
			current := appsv1.ChainNode{}
			if err := Framework().Client().Get(Framework().Context(), client.ObjectKey{Namespace: chainNodeSet.Namespace, Name: name}, &current); err != nil {
				return 0
			}
			return current.Status.LatestHeight
		}).Should(BeNumerically(">", minHeight), func() string {
			return fmt.Sprintf("node %s never advanced past height %d\n%s",
				name, minHeight, DescribeChainNode(chainNodeSet.Namespace, name))
		})
	}
}

// GetVaultAddress returns the in-cluster Vault address
func GetVaultAddress() string {
	return framework.VaultAddress
}

// CopyVaultSecretsToNamespace copies Vault token and CA certificate secrets to the test namespace.
// Returns the names of the token secret and CA secret in the target namespace.
func CopyVaultSecretsToNamespace(namespace string) (tokenSecretName, caSecretName string) {
	ctx := context.Background()

	// Copy token secret
	tokenSecret, err := Framework().KubeClient().CoreV1().Secrets(framework.VaultNamespace).Get(
		ctx, framework.VaultTokenSecretName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())

	tokenSecretName = "vault-token"
	newTokenSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tokenSecretName,
			Namespace: namespace,
		},
		Data: map[string][]byte{
			"token": tokenSecret.Data["token"],
		},
	}
	_, err = Framework().KubeClient().CoreV1().Secrets(namespace).Create(ctx, newTokenSecret, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred())

	// Copy CA certificate secret
	caSecret, err := Framework().KubeClient().CoreV1().Secrets(framework.VaultNamespace).Get(
		ctx, framework.VaultCASecretName, metav1.GetOptions{})
	Expect(err).NotTo(HaveOccurred())

	caSecretName = "vault-ca"
	newCASecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      caSecretName,
			Namespace: namespace,
		},
		Data: map[string][]byte{
			"ca.crt": caSecret.Data["ca.crt"],
		},
	}
	_, err = Framework().KubeClient().CoreV1().Secrets(namespace).Create(ctx, newCASecret, metav1.CreateOptions{})
	Expect(err).NotTo(HaveOccurred())

	return tokenSecretName, caSecretName
}

// WaitForTmkmsContainerRunning waits for the TMKMS container to be ready in the ChainNode pod
func WaitForTmkmsContainerRunning(chainNode *appsv1.ChainNode) {
	Eventually(func() bool {
		pod := &corev1.Pod{}
		if err := Framework().Client().Get(Framework().Context(), client.ObjectKey{
			Namespace: chainNode.Namespace,
			Name:      chainNode.Name,
		}, pod); err != nil {
			return false
		}

		// Check if tmkms container exists and is ready
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.Name == "tmkms" {
				return cs.Ready
			}
		}
		return false
	}).Should(BeTrue())
}

// DescribeChainNode renders the parts of a ChainNode's status that explain why it stopped making
// progress: an upgrade that reports completed covers both a clean restart and one the controller gave
// up on, so the upgrade entries and conditions are what separate the two.
func DescribeChainNode(namespace, name string) string {
	node := appsv1.ChainNode{}
	if err := Framework().Client().Get(Framework().Context(),
		client.ObjectKey{Namespace: namespace, Name: name}, &node); err != nil {
		return fmt.Sprintf("  <could not read ChainNode %s: %v>\n", name, err)
	}

	var b strings.Builder
	fmt.Fprintf(&b, "  ChainNode %s: phase=%s height=%d image=%s\n",
		node.Name, node.Status.Phase, node.Status.LatestHeight, node.Status.AppImage)
	for _, upgrade := range node.Status.Upgrades {
		fmt.Fprintf(&b, "    upgrade height=%d status=%s source=%s image=%s\n",
			upgrade.Height, upgrade.Status, upgrade.Source, upgrade.Image)
	}
	for _, condition := range node.Status.Conditions {
		fmt.Fprintf(&b, "    condition %s=%s reason=%s: %s\n",
			condition.Type, condition.Status, condition.Reason, condition.Message)
	}
	return b.String()
}

// DumpNamespaceDiagnostics writes the cluster-side state of a failing spec to the Ginkgo output.
// Nothing else does: the controller runs inside the kind cluster and its logs are never collected,
// and CI tears the cluster down on the way out, so a timeout that is not described here cannot be
// investigated after the fact. Events carry the most — the controller records upgrade and restart
// failures against the objects in this namespace.
const (
	// Where the framework's Helm release puts the controller, and how to pick it out.
	controllerNamespace = "cosmopilot-system"
	controllerSelector  = "app.kubernetes.io/name=cosmopilot"
	controllerContainer = "manager"

	// The controller serves every Ginkgo process at once, so its stream is mostly other specs'
	// work. Read a generous tail, print only the lines naming the namespace that failed.
	controllerLogTailLines = 4000
	controllerLogMaxLines  = 150

	// Capping the number of lines does not cap the bytes: a structured line can carry a whole
	// resource dump, and the scanner is deliberately sized to survive one. Printing them whole
	// would let a single failed spec emit tens of megabytes, and several failures in a shard would
	// then push the earliest — usually the most interesting — diagnostics out of the CI log this
	// exists to be read in. The message and its first fields lead the line, so cutting the tail
	// costs little.
	controllerLogMaxLineBytes = 2000
)

// DumpControllerDiagnostics prints the controller's own account of a failing namespace.
//
// The namespace dump says what cosmopilot did. When it did nothing at all — no pods, no events, a
// ChainNode whose status was never written — that dump goes quiet exactly when the answer matters
// most, and cannot distinguish a controller that was down from one that saw the object and refused
// it. Restart counts settle the first question and the log lines settle the second.
func DumpControllerDiagnostics(namespace string) {
	ctx := Framework().Context()

	pods, err := Framework().KubeClient().CoreV1().Pods(controllerNamespace).List(ctx, metav1.ListOptions{
		LabelSelector: controllerSelector,
	})
	if err != nil {
		GinkgoWriter.Printf("  <could not list controller pods: %v>\n", err)
		return
	}
	if len(pods.Items) == 0 {
		GinkgoWriter.Printf("  <no controller pod matched %s in %s>\n", controllerSelector, controllerNamespace)
		return
	}

	for _, pod := range pods.Items {
		GinkgoWriter.Printf("  Controller %s: phase=%s\n", pod.Name, pod.Status.Phase)
		for _, status := range pod.Status.ContainerStatuses {
			GinkgoWriter.Printf("    container %s ready=%t restarts=%d state=%s\n",
				status.Name, status.Ready, status.RestartCount, describeContainerState(status.State))
		}
		dumpControllerLog(ctx, pod.Name, namespace)
	}
}

// dumpControllerLog prints the controller log lines that mention the namespace.
func dumpControllerLog(ctx context.Context, podName, namespace string) {
	tail := int64(controllerLogTailLines)
	stream, err := Framework().KubeClient().CoreV1().Pods(controllerNamespace).
		GetLogs(podName, &corev1.PodLogOptions{Container: controllerContainer, TailLines: &tail}).
		Stream(ctx)
	if err != nil {
		GinkgoWriter.Printf("    <could not read controller log: %v>\n", err)
		return
	}
	defer stream.Close()

	// Structured log lines carry whole resource dumps, so the default 64KiB scanner limit is not
	// enough: without a bigger buffer the scan stops at the first long line and the rest is lost.
	scanner := bufio.NewScanner(stream)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	printed := 0
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.Contains(line, namespace) {
			continue
		}
		if printed == controllerLogMaxLines {
			GinkgoWriter.Printf("    <further controller log lines omitted>\n")
			return
		}
		GinkgoWriter.Printf("    %s\n", truncateLogLine(line))
		printed++
	}
	if err := scanner.Err(); err != nil {
		GinkgoWriter.Printf("    <controller log truncated: %v>\n", err)
	}
	if printed == 0 {
		GinkgoWriter.Printf("    <no controller log line mentions %s>\n", namespace)
	}
}

// truncateLogLine shortens an over-long log line for printing, saying how much it dropped so the
// reader can tell a cut line from a short one. The cut is walked back to a rune boundary, since
// splitting a multi-byte character mid-way would render as a replacement character.
func truncateLogLine(line string) string {
	if len(line) <= controllerLogMaxLineBytes {
		return line
	}
	cut := controllerLogMaxLineBytes
	for cut > 0 && !utf8.RuneStart(line[cut]) {
		cut--
	}
	return fmt.Sprintf("%s… <%d more bytes>", line[:cut], len(line)-cut)
}

func DumpNamespaceDiagnostics(namespace string) {
	ctx := Framework().Context()

	GinkgoWriter.Printf("\n===== diagnostics for namespace %s =====\n", namespace)
	defer GinkgoWriter.Printf("===== end diagnostics for namespace %s =====\n\n", namespace)

	nodes := appsv1.ChainNodeList{}
	if err := Framework().Client().List(ctx, &nodes, client.InNamespace(namespace)); err != nil {
		GinkgoWriter.Printf("  <could not list ChainNodes: %v>\n", err)
	}
	for _, node := range nodes.Items {
		GinkgoWriter.Print(DescribeChainNode(namespace, node.Name))
	}

	pods, err := Framework().KubeClient().CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		GinkgoWriter.Printf("  <could not list pods: %v>\n", err)
	} else {
		for _, pod := range pods.Items {
			GinkgoWriter.Printf("  Pod %s: phase=%s\n", pod.Name, pod.Status.Phase)
			for _, status := range pod.Status.ContainerStatuses {
				GinkgoWriter.Printf("    container %s ready=%t restarts=%d state=%s\n",
					status.Name, status.Ready, status.RestartCount, describeContainerState(status.State))
			}
		}
	}

	events, err := Framework().KubeClient().CoreV1().Events(namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		GinkgoWriter.Printf("  <could not list events: %v>\n", err)
	} else {
		sort.Slice(events.Items, func(i, j int) bool {
			return events.Items[i].LastTimestamp.Before(&events.Items[j].LastTimestamp)
		})
		for _, event := range events.Items {
			GinkgoWriter.Printf("  Event %s %s/%s %s: %s\n",
				event.Type, event.InvolvedObject.Kind, event.InvolvedObject.Name, event.Reason, event.Message)
		}
	}

	DumpControllerDiagnostics(namespace)
}

// describeContainerState renders whichever of the three container states is set. A restart that never
// came back shows up here as a waiting reason, which is the detail worth having.
func describeContainerState(state corev1.ContainerState) string {
	switch {
	case state.Waiting != nil:
		return fmt.Sprintf("waiting(%s: %s)", state.Waiting.Reason, state.Waiting.Message)
	case state.Terminated != nil:
		return fmt.Sprintf("terminated(%s: exit %d)", state.Terminated.Reason, state.Terminated.ExitCode)
	case state.Running != nil:
		return "running"
	default:
		return "unknown"
	}
}
