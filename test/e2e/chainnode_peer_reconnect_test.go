package e2e

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/voluzi/cosmopilot/v2/internal/controllers"
	"github.com/voluzi/cosmopilot/v2/test/e2e/apps"
)

var _ = Describe("ChainNode Peer Reconnect", func() {
	Context("Peer pod reschedule triggers peer restart", func() {
		apps.ForEachApp("should restart peer pods when a peer pod is rescheduled",
			WithNamespace(func(app apps.TestApp, ns *corev1.Namespace) {
				ctx := Framework().Context()

				// Create two ChainNodes that will auto-discover each other as peers.
				// We use a ChainNodeSet with 1 fullnode to get a validator + fullnode pair.
				chainNodeSet := app.BuildChainNodeSet(ns.Name, 1)
				err := Framework().Client().Create(ctx, chainNodeSet)
				Expect(err).NotTo(HaveOccurred())

				// Wait for ChainNodeSet to reach running phase
				WaitForChainNodeSetRunning(chainNodeSet)

				// Verify ChainNodes were created (validator + 1 fullnode = 2)
				Expect(CountChainNodes(ns.Name)).To(Equal(2))

				// Wait for nodes to reach height > 2 to confirm they're producing blocks
				WaitForChainNodeSetHeight(chainNodeSet, 2)

				// Get the fullnode pod name and its current UID
				RefreshChainNodeSet(chainNodeSet)
				fullnodeName := fmt.Sprintf("%s-fullnodes-0", chainNodeSet.Name)
				validatorName := fmt.Sprintf("%s-validator", chainNodeSet.Name)

				fullnodePod := &corev1.Pod{}
				err = Framework().Client().Get(ctx, types.NamespacedName{
					Namespace: ns.Name,
					Name:      fullnodeName,
				}, fullnodePod)
				Expect(err).NotTo(HaveOccurred())

				// Record the validator pod's UID before the fullnode reschedule
				validatorPod := &corev1.Pod{}
				err = Framework().Client().Get(ctx, types.NamespacedName{
					Namespace: ns.Name,
					Name:      validatorName,
				}, validatorPod)
				Expect(err).NotTo(HaveOccurred())
				validatorUIDBeforeReschedule := validatorPod.UID

				// Record the peer-pods-hash annotation on the validator's ConfigMap before reschedule
				validatorCM := &corev1.ConfigMap{}
				err = Framework().Client().Get(ctx, types.NamespacedName{
					Namespace: ns.Name,
					Name:      validatorName,
				}, validatorCM)
				Expect(err).NotTo(HaveOccurred())
				hashBefore := validatorCM.Annotations[controllers.AnnotationPeerPodsHash]

				// Delete the fullnode pod to simulate a reschedule
				By("Deleting fullnode pod to simulate reschedule")
				err = Framework().Client().Delete(ctx, fullnodePod)
				Expect(err).NotTo(HaveOccurred())

				// Wait for the fullnode pod to come back with a new UID
				By("Waiting for fullnode pod to be recreated with new UID")
				Eventually(func() bool {
					newPod := &corev1.Pod{}
					if err := Framework().Client().Get(ctx, client.ObjectKeyFromObject(fullnodePod), newPod); err != nil {
						return false
					}
					// Pod must exist with a different UID (rescheduled)
					return newPod.UID != fullnodePod.UID
				}).Should(BeTrue())

				// Wait for the validator's ConfigMap peer-pods-hash to change
				By("Waiting for validator ConfigMap peer-pods-hash annotation to change")
				Eventually(func() string {
					cm := &corev1.ConfigMap{}
					if err := Framework().Client().Get(ctx, types.NamespacedName{
						Namespace: ns.Name,
						Name:      validatorName,
					}, cm); err != nil {
						return hashBefore
					}
					return cm.Annotations[controllers.AnnotationPeerPodsHash]
				}).ShouldNot(Equal(hashBefore))

				// Wait for the validator pod to be recreated (new UID due to changed peer-pods-hash)
				By("Waiting for validator pod to be recreated due to peer-pods-hash change")
				Eventually(func() bool {
					newValidatorPod := &corev1.Pod{}
					if err := Framework().Client().Get(ctx, types.NamespacedName{
						Namespace: ns.Name,
						Name:      validatorName,
					}, newValidatorPod); err != nil {
						return false
					}
					return newValidatorPod.UID != validatorUIDBeforeReschedule
				}).Should(BeTrue())

				// Verify both nodes reconnect and continue producing blocks
				By("Verifying both nodes continue producing blocks after reconnect")
				WaitForChainNodeSetHeight(chainNodeSet, 5)
			}),
		)
	})
})
