package e2e

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1k8s "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
	chainnodecontroller "github.com/voluzi/cosmopilot/v2/internal/controllers/chainnode"
	managedcosmosigner "github.com/voluzi/cosmopilot/v2/internal/cosmosigner"
	"github.com/voluzi/cosmopilot/v2/test/e2e/apps"
)

var _ = Describe("ChainNodeSet Cosmosigner", func() {
	Context("Validator signing through a managed cosmosigner deployment", func() {
		apps.ForEachApp("should validate using cosmosigner (software backend) and produce blocks",
			WithNamespace(func(app apps.TestApp, ns *corev1.Namespace) {
				// The cosmosigner image is heavy infra; gate it behind the same flag as Vault so the
				// default e2e run stays lean and this opt-in path is exercised explicitly.
				if !Framework().Config().InstallVault {
					Skip("cosmosigner e2e is opt-in (enable with the Vault install flag)")
				}

				cns := app.BuildChainNodeSetWithCosmosigner(ns.Name, apps.CosmosignerConfig{Replicas: 1})
				Expect(Framework().Client().Create(Framework().Context(), cns)).To(Succeed())

				// The signer StatefulSet must be created and the chain must produce blocks, which only
				// happens if the validator node's remote signer is actually signing.
				WaitForChainNodeSetHeight(cns, 3)
				signerName := fmt.Sprintf("%s-signer", cns.GetName())
				validatorPodName := fmt.Sprintf("%s-validator", cns.GetName())
				validatorPod := waitForReadyCosmosignerTargetPod(
					ns.Name, validatorPodName, signerName, cns.Spec.App.App,
				)
				for cycle := 1; cycle <= 2; cycle++ {
					validatorPod = replaceCosmosignerTargetPodOnce(
						cns, validatorPod, signerName, cns.Spec.App.App, cycle,
					)
				}

				sts := &appsv1k8s.StatefulSet{}
				Eventually(func() error {
					return Framework().Client().Get(Framework().Context(),
						client.ObjectKey{Namespace: ns.Name, Name: signerName}, sts)
				}).Should(Succeed())
				Expect(*sts.Spec.Replicas).To(BeNumerically("==", 1))
			}),
		)

		apps.ForEachApp("should validate using cosmosigner (Vault backend, 3 replicas) and produce blocks",
			WithNamespace(func(app apps.TestApp, ns *corev1.Namespace) {
				if !Framework().Config().InstallVault {
					Skip("Vault is not installed, skipping cosmosigner Vault test")
				}

				tokenSecretName, caSecretName := CopyVaultSecretsToNamespace(ns.Name)
				cns := app.BuildChainNodeSetWithCosmosigner(ns.Name, apps.CosmosignerConfig{
					Replicas: 3,
					Vault: &apps.TmKMSConfig{
						VaultAddress:    GetVaultAddress(),
						KeyName:         fmt.Sprintf("%s-cosmosigner", app.ValidatorConfig.ChainID),
						TokenSecretName: tokenSecretName,
						CASecretName:    caSecretName,
					},
				})
				Expect(Framework().Client().Create(Framework().Context(), cns)).To(Succeed())

				WaitForChainNodeSetHeight(cns, 3)

				// Verify the key was imported into Vault exactly once (recorded in the signer's
				// per-signer status entry).
				RefreshChainNodeSet(cns)
				st := cns.GetCosmosignerStatus(fmt.Sprintf("%s-signer", cns.GetName()))
				Expect(st).NotTo(BeNil())
				Expect(st.KeyImported).NotTo(BeEmpty())
			}),
		)

		It("should preserve signing identity and Raft state when moving a top-level signer into its validator group", func() {
			requireCosmosignerE2E()
			app := apps.Nibiru()
			ns := CreateTestNamespace()
			cns := buildNamedValidatorCosmosignerSet(app, ns.Name, 1)
			Expect(Framework().Client().Create(Framework().Context(), cns)).To(Succeed())

			WaitForChainNodeSetHeight(cns, 3)
			oldLogicalName := fmt.Sprintf("%s-signer", cns.Name)
			oldStatus := waitForCosmosignerApplied(cns, oldLogicalName)
			oldResourceName := appsv1.CosmosignerStatusResourceName(oldStatus)
			oldPVCs := signerPVCUIDs(ns.Name, oldResourceName, 1)
			oldHeight, err := observedChainNodeSetHeight(cns)
			Expect(err).NotTo(HaveOccurred())

			moveTopLevelCosmosignerIntoGroup(cns, "validators")

			newLogicalName := fmt.Sprintf("%s-validators-signer", cns.Name)
			newStatus := waitForCosmosignerApplied(cns, newLogicalName)
			Expect(newStatus.PublicKey).To(Equal(oldStatus.PublicKey))
			Expect(newStatus.ResourceName).To(Equal(oldResourceName))
			Expect(signerPVCUIDs(ns.Name, oldResourceName, 1)).To(Equal(oldPVCs))
			Eventually(func() bool {
				current := &appsv1.ChainNodeSet{}
				if err := Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(cns), current); err != nil {
					return false
				}
				return current.GetCosmosignerStatus(oldLogicalName) == nil
			}).Should(BeTrue())
			WaitForChainNodeSetHeight(cns, oldHeight)
		})

		It("should not recreate a migrated signer while the old signer pod is terminating", func() {
			requireCosmosignerE2E()
			app := apps.Nibiru()
			ns := CreateTestNamespace()
			cns := buildNamedValidatorCosmosignerSet(app, ns.Name, 1)
			Expect(Framework().Client().Create(Framework().Context(), cns)).To(Succeed())

			WaitForChainNodeSetHeight(cns, 3)
			oldLogicalName := fmt.Sprintf("%s-signer", cns.Name)
			oldStatus := waitForCosmosignerApplied(cns, oldLogicalName)
			oldResourceName := appsv1.CosmosignerStatusResourceName(oldStatus)
			oldPodName := oldResourceName + "-0"
			setPodTestFinalizer(ns.Name, oldPodName, true)
			DeferCleanup(func() { setPodTestFinalizer(ns.Name, oldPodName, false) })

			oldPod := &corev1.Pod{}
			Expect(Framework().Client().Get(Framework().Context(), client.ObjectKey{Namespace: ns.Name, Name: oldPodName}, oldPod)).To(Succeed())
			oldPodUID := oldPod.UID
			oldHeight, err := observedChainNodeSetHeight(cns)
			Expect(err).NotTo(HaveOccurred())

			moveTopLevelCosmosignerIntoGroup(cns, "validators")
			waitForTerminatingSignerPod(ns.Name, oldPodName, string(oldPodUID), false)

			newLogicalName := fmt.Sprintf("%s-validators-signer", cns.Name)
			Consistently(func() bool {
				pods, err := listSignerPods(ns.Name, oldResourceName)
				if err != nil || len(pods) != 1 {
					return false
				}
				current := &appsv1.ChainNodeSet{}
				if err := Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(cns), current); err != nil {
					return false
				}
				status := current.GetCosmosignerStatus(newLogicalName)
				return pods[0].UID == oldPodUID && pods[0].DeletionTimestamp != nil && status != nil && status.Migration != nil
			}, 10*time.Second, time.Second).Should(BeTrue())

			setPodTestFinalizer(ns.Name, oldPodName, false)
			newStatus := waitForCosmosignerApplied(cns, newLogicalName)
			Expect(newStatus.PublicKey).To(Equal(oldStatus.PublicKey))
			Expect(newStatus.ResourceName).To(Equal(oldResourceName))
			WaitForChainNodeSetHeight(cns, oldHeight)
		})

		It("should fail over a TLS-secured Raft leader and stop signing without quorum", func() {
			requireCosmosignerE2E()
			app := apps.Nibiru()
			ns := CreateTestNamespace()
			cns := buildNamedValidatorCosmosignerSet(app, ns.Name, 3)
			resourceName := fmt.Sprintf("%s-signer", cns.Name)
			validatorName := fmt.Sprintf("%s-validators-0", cns.Name)
			tlsSecretName := createRaftTLSSecret(ns.Name, resourceName)
			cns.Spec.Cosmosigner.RaftTLSSecret = ptr.To(tlsSecretName)
			Expect(Framework().Client().Create(Framework().Context(), cns)).To(Succeed())

			Eventually(func() (int64, error) {
				return observedChainNodeHeight(ns.Name, validatorName)
			}).Should(BeNumerically(">", 3), "the initial TLS-secured Raft cluster should produce blocks")
			pods := waitForReadySignerPods(ns.Name, resourceName, 3)
			leaderName := waitForSignerLeader(ns.Name, resourceName, "")
			var leaderUID string
			for i := range pods {
				if pods[i].Name == leaderName {
					leaderUID = string(pods[i].UID)
				}
			}
			Expect(leaderUID).NotTo(BeEmpty())
			heightBeforeFailover, err := observedChainNodeHeight(ns.Name, validatorName)
			Expect(err).NotTo(HaveOccurred())

			leaderPod := &corev1.Pod{}
			Expect(Framework().Client().Get(Framework().Context(), client.ObjectKey{Namespace: ns.Name, Name: leaderName}, leaderPod)).To(Succeed())
			Expect(Framework().Client().Delete(Framework().Context(), leaderPod)).To(Succeed())
			newLeaderName := waitForSignerLeader(ns.Name, resourceName, leaderName)
			Expect(newLeaderName).NotTo(Equal(leaderName))
			Eventually(func() (int64, error) {
				return observedChainNodeHeight(ns.Name, validatorName)
			}).Should(BeNumerically(">", heightBeforeFailover), "a surviving Raft replica should resume signing after leader deletion")
			pods = waitForReadySignerPods(ns.Name, resourceName, 3)

			heldNames := []string{newLeaderName}
			for i := range pods {
				if pods[i].Name != newLeaderName {
					heldNames = append(heldNames, pods[i].Name)
					break
				}
			}
			Expect(heldNames).To(HaveLen(2))
			heldUIDs := make(map[string]string, len(heldNames))
			for _, name := range heldNames {
				setPodTestFinalizer(ns.Name, name, true)
				DeferCleanup(func() { setPodTestFinalizer(ns.Name, name, false) })
				pod := &corev1.Pod{}
				Expect(Framework().Client().Get(Framework().Context(), client.ObjectKey{Namespace: ns.Name, Name: name}, pod)).To(Succeed())
				heldUIDs[name] = string(pod.UID)
				Expect(Framework().Client().Delete(Framework().Context(), pod)).To(Succeed())
			}
			for _, name := range heldNames {
				waitForTerminatingSignerPod(ns.Name, name, heldUIDs[name], true)
			}

			stableHeight := waitForStableChainNodeHeight(ns.Name, validatorName, 10*time.Second)
			Consistently(func() (int64, error) {
				return observedChainNodeHeight(ns.Name, validatorName)
			}, 10*time.Second, time.Second).Should(Equal(stableHeight))

			setPodTestFinalizer(ns.Name, heldNames[0], false)
			waitForReplacementSignerPod(ns.Name, heldNames[0], heldUIDs[heldNames[0]])
			Eventually(func() (int64, error) {
				return observedChainNodeHeight(ns.Name, validatorName)
			}).Should(BeNumerically(">", stableHeight), "restoring a second voter should restore Raft quorum and signing")

			setPodTestFinalizer(ns.Name, heldNames[1], false)
			waitForReadySignerPods(ns.Name, resourceName, 3)
		})
	})
})

func requireCosmosignerE2E() {
	if !Framework().Config().InstallVault {
		Skip("cosmosigner e2e is opt-in (enable with the Vault install flag)")
	}
}

func buildNamedValidatorCosmosignerSet(app apps.TestApp, namespace string, replicas int32) *appsv1.ChainNodeSet {
	cns := app.BuildChainNodeSet(namespace, 0)
	cns.Name = fmt.Sprintf("e2e-nibiru-cosmosigner-%s", RandString(6))
	cns.GenerateName = ""
	validator := cns.Spec.Validator
	cns.Spec.Validator = nil
	cns.Spec.Nodes = []appsv1.NodeGroupSpec{{
		Name:      "validators",
		Instances: ptr.To(1),
		Validator: validator,
	}}
	cns.Spec.Cosmosigner = &appsv1.Cosmosigner{
		NodeGroups: []string{"validators"},
		Replicas:   ptr.To(replicas),
		Backend: appsv1.CosmosignerBackend{
			Software: &appsv1.CosmosignerSoftwareBackend{},
		},
	}
	return cns
}

func moveTopLevelCosmosignerIntoGroup(cns *appsv1.ChainNodeSet, groupName string) {
	Eventually(func() error {
		current := &appsv1.ChainNodeSet{}
		if err := Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(cns), current); err != nil {
			return err
		}
		if current.Spec.Cosmosigner == nil {
			for i := range current.Spec.Nodes {
				if current.Spec.Nodes[i].Name == groupName && current.Spec.Nodes[i].Cosmosigner != nil {
					return nil
				}
			}
			return fmt.Errorf("top-level cosmosigner is absent without a replacement in group %q", groupName)
		}
		replacement := current.Spec.Cosmosigner.DeepCopy()
		replacement.NodeGroups = nil
		for i := range current.Spec.Nodes {
			if current.Spec.Nodes[i].Name == groupName {
				current.Spec.Nodes[i].Cosmosigner = replacement
				current.Spec.Cosmosigner = nil
				return Framework().Client().Update(Framework().Context(), current)
			}
		}
		return fmt.Errorf("node group %q not found", groupName)
	}).Should(Succeed())
}

func waitForCosmosignerApplied(cns *appsv1.ChainNodeSet, logicalName string) *appsv1.CosmosignerStatus {
	var result *appsv1.CosmosignerStatus
	Eventually(func(g Gomega) {
		current := &appsv1.ChainNodeSet{}
		g.Expect(Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(cns), current)).To(Succeed())
		status := current.GetCosmosignerStatus(logicalName)
		g.Expect(status).NotTo(BeNil())
		if status == nil {
			return
		}
		g.Expect(status.AppliedDigest).NotTo(BeEmpty())
		g.Expect(status.PublicKey).NotTo(BeEmpty())
		g.Expect(status.Migration).To(BeNil())
		result = status.DeepCopy()
	}).Should(Succeed())
	return result
}

func signerPVCUIDs(namespace, resourceName string, replicas int32) map[string]string {
	uids := make(map[string]string, replicas)
	for ordinal := int32(0); ordinal < replicas; ordinal++ {
		name := fmt.Sprintf("data-%s-%d", resourceName, ordinal)
		pvc := &corev1.PersistentVolumeClaim{}
		Eventually(func() error {
			return Framework().Client().Get(Framework().Context(), client.ObjectKey{Namespace: namespace, Name: name}, pvc)
		}).Should(Succeed())
		uids[name] = string(pvc.UID)
	}
	return uids
}

func listSignerPods(namespace, resourceName string) ([]corev1.Pod, error) {
	list := &corev1.PodList{}
	if err := Framework().Client().List(
		Framework().Context(),
		list,
		client.InNamespace(namespace),
		client.MatchingLabels(managedcosmosigner.InstanceLabels(resourceName)),
	); err != nil {
		return nil, err
	}
	sort.Slice(list.Items, func(i, j int) bool { return list.Items[i].Name < list.Items[j].Name })
	return list.Items, nil
}

func waitForReadySignerPods(namespace, resourceName string, replicas int) []corev1.Pod {
	var result []corev1.Pod
	Eventually(func(g Gomega) {
		pods, err := listSignerPods(namespace, resourceName)
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(pods).To(HaveLen(replicas))
		for i := range pods {
			g.Expect(pods[i].DeletionTimestamp).To(BeNil())
			g.Expect(podReady(&pods[i])).To(BeTrue())
		}
		result = append([]corev1.Pod(nil), pods...)
	}).Should(Succeed())
	return result
}

func podReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func waitForReadyCosmosignerTargetPod(namespace, name, signerName, appContainer string) *corev1.Pod {
	var result *corev1.Pod
	Eventually(func(g Gomega) {
		pod := &corev1.Pod{}
		g.Expect(Framework().Client().Get(
			Framework().Context(), client.ObjectKey{Namespace: namespace, Name: name}, pod,
		)).To(Succeed())
		g.Expect(pod.Labels[controllers.LabelCosmosignerTarget]).To(Equal(signerName))
		g.Expect(pod.Status.Phase).To(Equal(corev1.PodRunning))
		g.Expect(podReady(pod)).To(BeTrue())
		g.Expect(cosmosignerDiscoveryGateSucceeded(pod)).To(BeTrue())
		restartCount, previousLogs, found := appContainerRestartDetails(namespace, pod, appContainer)
		g.Expect(found).To(BeTrue(), "app container %q status is missing", appContainer)
		g.Expect(restartCount).To(BeZero(), "initial app container restarted; previous logs:\n%s", previousLogs)
		result = pod.DeepCopy()
	}, 2*time.Minute, 500*time.Millisecond).Should(Succeed())
	return result
}

func replaceCosmosignerTargetPodOnce(cns *appsv1.ChainNodeSet, current *corev1.Pod, signerName, appContainer string, cycle int) *corev1.Pod {
	By(fmt.Sprintf("restarting Cosmosigner target pod, cycle %d", cycle))
	namespace := cns.GetNamespace()
	ctx, cancel := context.WithTimeout(Framework().Context(), 2*time.Minute)
	defer cancel()

	podWatch, err := Framework().KubeClient().CoreV1().Pods(namespace).Watch(ctx, metav1.ListOptions{
		FieldSelector:   fields.OneTermEqualSelector("metadata.name", current.Name).String(),
		ResourceVersion: current.ResourceVersion,
	})
	Expect(err).NotTo(HaveOccurred())
	defer podWatch.Stop()

	oldUID := string(current.UID)
	Expect(Framework().Client().Delete(Framework().Context(), current.DeepCopy())).To(Succeed())

	replacementUIDs := map[string]struct{}{}
	var replacement *corev1.Pod
	for replacement == nil {
		select {
		case <-ctx.Done():
			Fail(fmt.Sprintf("timed out waiting for Cosmosigner target pod replacement in cycle %d: %v", cycle, ctx.Err()))
		case event, ok := <-podWatch.ResultChan():
			if !ok {
				Fail(fmt.Sprintf("Cosmosigner target pod watch closed before cycle %d replacement became ready", cycle))
			}
			if event.Type == watch.Error {
				Fail(fmt.Sprintf("Cosmosigner target pod watch failed in cycle %d: %v", cycle, apierrors.FromObject(event.Object)))
			}
			pod, ok := event.Object.(*corev1.Pod)
			if !ok || string(pod.UID) == oldUID {
				continue
			}
			replacementUIDs[string(pod.UID)] = struct{}{}
			Expect(replacementUIDs).To(HaveLen(1), "cycle %d created more than one replacement Pod UID", cycle)
			if pod.DeletionTimestamp == nil && pod.Status.Phase == corev1.PodRunning && podReady(pod) && cosmosignerDiscoveryGateSucceeded(pod) {
				replacement = pod.DeepCopy()
			}
		}
	}

	Expect(replacement.Labels[controllers.LabelCosmosignerTarget]).To(Equal(signerName))
	Expect(string(replacement.UID)).NotTo(Equal(oldUID))
	restartCount, previousLogs, found := appContainerRestartDetails(namespace, replacement, appContainer)
	Expect(found).To(BeTrue(), "cycle %d app container %q status is missing", cycle, appContainer)
	Expect(restartCount).To(BeZero(), "cycle %d app container restarted; previous logs:\n%s", cycle, previousLogs)
	heightAtReplacement, err := observedChainNodeSetHeight(cns)
	Expect(err).NotTo(HaveOccurred())
	Eventually(func() (int64, error) {
		return observedChainNodeSetHeight(cns)
	}, time.Minute, time.Second).Should(BeNumerically(">", heightAtReplacement),
		"cycle %d chain height must advance after the replacement becomes ready", cycle)

	Consistently(func(g Gomega) {
		pod := &corev1.Pod{}
		g.Expect(Framework().Client().Get(
			Framework().Context(), client.ObjectKey{Namespace: namespace, Name: replacement.Name}, pod,
		)).To(Succeed())
		g.Expect(string(pod.UID)).To(Equal(string(replacement.UID)), "cycle %d must not create a second replacement", cycle)
		g.Expect(pod.Status.Phase).To(Equal(corev1.PodRunning))
		g.Expect(podReady(pod)).To(BeTrue())
		g.Expect(cosmosignerDiscoveryGateSucceeded(pod)).To(BeTrue())
		restartCount, previousLogs, found := appContainerRestartDetails(namespace, pod, appContainer)
		g.Expect(found).To(BeTrue(), "cycle %d app container %q status is missing", cycle, appContainer)
		g.Expect(restartCount).To(BeZero(), "cycle %d app container restarted; previous logs:\n%s", cycle, previousLogs)
	}, 35*time.Second, 500*time.Millisecond).Should(Succeed())
	assertNoCosmosignerDiscoveryPubKeyFailure(namespace, replacement.Name, appContainer, cycle)

	return replacement
}

func cosmosignerDiscoveryGateSucceeded(pod *corev1.Pod) bool {
	for i := range pod.Status.InitContainerStatuses {
		status := &pod.Status.InitContainerStatuses[i]
		if status.Name == chainnodecontroller.CosmosignerDiscoveryWaitContainerName {
			return status.State.Terminated != nil && status.State.Terminated.ExitCode == 0
		}
	}
	return false
}

func appContainerRestartDetails(namespace string, pod *corev1.Pod, appContainer string) (int32, string, bool) {
	for i := range pod.Status.ContainerStatuses {
		status := &pod.Status.ContainerStatuses[i]
		if status.Name != appContainer {
			continue
		}
		if status.RestartCount == 0 {
			return 0, "", true
		}
		logs, err := Framework().KubeClient().CoreV1().Pods(namespace).GetLogs(pod.Name, &corev1.PodLogOptions{
			Container: appContainer,
			Previous:  true,
		}).DoRaw(Framework().Context())
		if err != nil {
			return status.RestartCount, fmt.Sprintf("failed to read previous logs: %v", err), true
		}
		return status.RestartCount, string(logs), true
	}
	return 0, "", false
}

func assertNoCosmosignerDiscoveryPubKeyFailure(namespace, podName, appContainer string, cycle int) {
	logs, err := Framework().KubeClient().CoreV1().Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{
		Container: appContainer,
	}).DoRaw(Framework().Context())
	Expect(err).NotTo(HaveOccurred())
	lower := strings.ToLower(string(logs))
	for _, signature := range []string{
		"can't get pubkey",
		"can't-get-pubkey",
		"cannot get pubkey",
		"cannot-get-pubkey",
		"exhausted all attempts to get pubkey",
		"failed to get private validator pubkey",
	} {
		Expect(lower).NotTo(ContainSubstring(signature), "cycle %d app logs contain a remote-signer public-key startup failure", cycle)
	}
	for _, line := range strings.Split(lower, "\n") {
		publicKeyLine := strings.Contains(line, "pubkey") || strings.Contains(line, "public key") || strings.Contains(line, "public-key")
		timeoutLine := strings.Contains(line, "timeout") || strings.Contains(line, "timed out") || strings.Contains(line, "deadline exceeded")
		Expect(publicKeyLine && timeoutLine).To(BeFalse(), "cycle %d app logs contain a public-key timeout line: %q", cycle, line)
	}
}

const e2eHoldFinalizer = "e2e.cosmopilot.voluzi.com/hold"

func setPodTestFinalizer(namespace, name string, present bool) {
	Eventually(func() error {
		pod := &corev1.Pod{}
		err := Framework().Client().Get(Framework().Context(), client.ObjectKey{Namespace: namespace, Name: name}, pod)
		if apierrors.IsNotFound(err) && !present {
			return nil
		}
		if err != nil {
			return err
		}
		hasFinalizer := false
		for _, finalizer := range pod.Finalizers {
			if finalizer == e2eHoldFinalizer {
				hasFinalizer = true
			}
		}
		if hasFinalizer == present {
			return nil
		}
		if present {
			pod.Finalizers = append(pod.Finalizers, e2eHoldFinalizer)
		} else {
			filtered := pod.Finalizers[:0]
			for _, finalizer := range pod.Finalizers {
				if finalizer != e2eHoldFinalizer {
					filtered = append(filtered, finalizer)
				}
			}
			pod.Finalizers = filtered
		}
		return Framework().Client().Update(Framework().Context(), pod)
	}).Should(Succeed())
}

func waitForTerminatingSignerPod(namespace, name, uid string, requireNotReady bool) {
	Eventually(func() bool {
		pod := &corev1.Pod{}
		if err := Framework().Client().Get(Framework().Context(), client.ObjectKey{Namespace: namespace, Name: name}, pod); err != nil {
			return false
		}
		if string(pod.UID) != uid || pod.DeletionTimestamp == nil {
			return false
		}
		return !requireNotReady || !podReady(pod)
	}).Should(BeTrue())
}

func waitForReplacementSignerPod(namespace, name, oldUID string) {
	Eventually(func() bool {
		pod := &corev1.Pod{}
		if err := Framework().Client().Get(Framework().Context(), client.ObjectKey{Namespace: namespace, Name: name}, pod); err != nil {
			return false
		}
		return string(pod.UID) != oldUID && pod.DeletionTimestamp == nil && podReady(pod)
	}).Should(BeTrue())
}

func observedChainNodeSetHeight(cns *appsv1.ChainNodeSet) (int64, error) {
	current := &appsv1.ChainNodeSet{}
	if err := Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(cns), current); err != nil {
		return 0, err
	}
	return current.Status.LatestHeight, nil
}

func observedChainNodeHeight(namespace, name string) (int64, error) {
	current := &appsv1.ChainNode{}
	if err := Framework().Client().Get(Framework().Context(), client.ObjectKey{Namespace: namespace, Name: name}, current); err != nil {
		return 0, err
	}
	return current.Status.LatestHeight, nil
}

func waitForStableChainNodeHeight(namespace, name string, stableFor time.Duration) int64 {
	var height int64
	var unchangedSince time.Time
	Eventually(func() (bool, error) {
		current, err := observedChainNodeHeight(namespace, name)
		if err != nil {
			return false, err
		}
		if unchangedSince.IsZero() || current != height {
			height = current
			unchangedSince = time.Now()
			return false, nil
		}
		return time.Since(unchangedSince) >= stableFor, nil
	}, 45*time.Second, time.Second).Should(BeTrue())
	return height
}

func waitForSignerLeader(namespace, resourceName, excludedPod string) string {
	var leader string
	Eventually(func() string {
		pods, err := listSignerPods(namespace, resourceName)
		if err != nil {
			return ""
		}
		for i := range pods {
			if pods[i].Name == excludedPod || pods[i].DeletionTimestamp != nil {
				continue
			}
			logs, err := Framework().KubeClient().CoreV1().Pods(namespace).GetLogs(pods[i].Name, &corev1.PodLogOptions{
				Container: "cosmosigner",
			}).DoRaw(Framework().Context())
			if err == nil && strings.Contains(string(logs), "serving remote signer") {
				leader = pods[i].Name
				return leader
			}
		}
		return ""
	}, 2*time.Minute, 2*time.Second).ShouldNot(BeEmpty())
	return leader
}

func createRaftTLSSecret(namespace, resourceName string) string {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	Expect(err).NotTo(HaveOccurred())
	now := time.Now()
	ca := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "cosmosigner e2e raft ca"},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(2 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	Expect(err).NotTo(HaveOccurred())

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	Expect(err).NotTo(HaveOccurred())
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: resourceName},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(2 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
			x509.ExtKeyUsageClientAuth,
		},
		DNSNames: []string{
			fmt.Sprintf("*.%s.%s.svc", resourceName, namespace),
			fmt.Sprintf("%s.%s.svc", resourceName, namespace),
		},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, ca, &leafKey.PublicKey, caKey)
	Expect(err).NotTo(HaveOccurred())
	leafKeyDER, err := x509.MarshalECPrivateKey(leafKey)
	Expect(err).NotTo(HaveOccurred())

	name := "cosmosigner-raft-tls"
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Type:       corev1.SecretTypeTLS,
		Data: map[string][]byte{
			corev1.TLSCertKey:       pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}),
			corev1.TLSPrivateKeyKey: pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: leafKeyDER}),
			"ca.crt":                pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		},
	}
	Expect(Framework().Client().Create(Framework().Context(), secret)).To(Succeed())
	return name
}
