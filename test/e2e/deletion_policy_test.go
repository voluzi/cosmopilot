package e2e

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/resourcecleanup"
	"github.com/voluzi/cosmopilot/v2/test/e2e/apps"
)

var _ = Describe("Deletion policy", func() {
	It("deletes only attributed node data and generated keys after the workload stops", WithNs(func(ns *corev1.Namespace) {
		app := apps.Nibiru()
		node := app.BuildChainNode(ns.Name)
		node.Spec.DeletionPolicy = &appsv1.DeletionPolicy{
			DataVolumes:      ptr.To(appsv1.DeletionPolicyDelete),
			GeneratedKeys:    ptr.To(appsv1.DeletionPolicyDelete),
			CosmosignerState: ptr.To(appsv1.DeletionPolicyDelete),
		}
		Expect(Framework().Client().Create(Framework().Context(), node)).To(Succeed())
		WaitForChainNodeHeight(node, 1)
		RefreshChainNode(node)

		Eventually(func() int {
			return attributedPVCCount(ns.Name, node.UID)
		}).Should(BeNumerically(">", 0))
		Eventually(func() int {
			return attributedSecretCount(ns.Name, node.UID)
		}).Should(BeNumerically(">", 0))
		reservations := &appsv1.ConsensusKeyReservationList{}
		Eventually(func() int {
			if err := Framework().Client().List(Framework().Context(), reservations); err != nil {
				return -1
			}
			count := 0
			for i := range reservations.Items {
				if reservations.Items[i].Spec.OwnerUID == node.UID {
					count++
				}
			}
			return count
		}).Should(BeNumerically(">", 0))
		DeferCleanup(func() {
			for i := range reservations.Items {
				if reservations.Items[i].Spec.OwnerUID == node.UID {
					_ = Framework().Client().Delete(Framework().Context(), &reservations.Items[i])
				}
			}
		})

		Expect(Framework().Client().Delete(Framework().Context(), node)).To(Succeed())
		Eventually(func() bool {
			err := Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(node), &appsv1.ChainNode{})
			return apierrors.IsNotFound(err)
		}).Should(BeTrue())
		Eventually(func() bool {
			err := Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(node), &corev1.Pod{})
			return apierrors.IsNotFound(err)
		}).Should(BeTrue())
		Eventually(func() int {
			return attributedPVCCount(ns.Name, node.UID)
		}).Should(Equal(0))
		Eventually(func() int {
			return attributedSecretCount(ns.Name, node.UID)
		}).Should(Equal(0))
		for i := range reservations.Items {
			if reservations.Items[i].Spec.OwnerUID != node.UID {
				continue
			}
			current := &appsv1.ConsensusKeyReservation{}
			Expect(Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(&reservations.Items[i]), current)).To(Succeed())
		}
	}))
})

func attributedPVCCount(namespace string, uid types.UID) int {
	list := &corev1.PersistentVolumeClaimList{}
	if err := Framework().Client().List(Framework().Context(), list, client.InNamespace(namespace)); err != nil {
		return -1
	}
	count := 0
	for i := range list.Items {
		if list.Items[i].Annotations[resourcecleanup.AnnotationRootOwnerUID] == string(uid) {
			count++
		}
	}
	return count
}

func attributedSecretCount(namespace string, uid types.UID) int {
	list := &corev1.SecretList{}
	if err := Framework().Client().List(Framework().Context(), list, client.InNamespace(namespace)); err != nil {
		return -1
	}
	count := 0
	for i := range list.Items {
		if list.Items[i].Annotations[resourcecleanup.AnnotationRootOwnerUID] == string(uid) {
			count++
		}
	}
	return count
}
