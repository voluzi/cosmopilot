package integration

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	appsv1 "github.com/voluzi/cosmopilot/v3/api/v1"
	"github.com/voluzi/cosmopilot/v3/internal/resourcecleanup"
)

var _ = Describe("Deletion policy", func() {
	DescribeTable("applies explicit retention and deletion after quiescing controlled pods",
		func(policy *appsv1.DeletionPolicy, expectDeleted bool) {
			ns := CreateTestNamespace()
			node := &appsv1.ChainNode{
				ObjectMeta: metav1.ObjectMeta{GenerateName: ChainNodePrefix, Namespace: ns.Name},
				Spec: appsv1.ChainNodeSpec{
					App:            DefaultChainNodeTestApp,
					Genesis:        &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
					DeletionPolicy: policy,
				},
			}
			Expect(Framework().Client().Create(Framework().Context(), node)).To(Succeed())

			Eventually(func() error {
				return Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(node), &corev1.PersistentVolumeClaim{})
			}).Should(Succeed())
			Eventually(func() error {
				return Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(node), &corev1.Secret{})
			}).Should(Succeed())
			Eventually(func() bool {
				current := &appsv1.ChainNode{}
				if err := Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(node), current); err != nil {
					return false
				}
				return containsString(current.Finalizers, resourcecleanup.Finalizer)
			}).Should(BeTrue())

			current := &appsv1.ChainNode{}
			Expect(Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(node), current)).To(Succeed())
			Expect(Framework().Client().Delete(Framework().Context(), current)).To(Succeed())
			if expectDeleted {
				// Envtest enables PVC protection admission without running the controller that releases
				// the protection finalizer after all consuming pods are gone.
				protectionObserved := false
				Eventually(func() bool {
					pvc := &corev1.PersistentVolumeClaim{}
					if err := Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(node), pvc); err != nil {
						return protectionObserved && apierrors.IsNotFound(err)
					}
					if pvc.GetDeletionTimestamp().IsZero() || !containsString(pvc.Finalizers, pvcProtectionFinalizer) {
						return false
					}
					protectionObserved = true

					pods := &corev1.PodList{}
					if err := Framework().Client().List(Framework().Context(), pods, client.InNamespace(ns.Name)); err != nil {
						return false
					}
					for i := range pods.Items {
						if metav1.IsControlledBy(&pods.Items[i], current) {
							return false
						}
					}

					controllerutil.RemoveFinalizer(pvc, pvcProtectionFinalizer)
					err := Framework().Client().Update(Framework().Context(), pvc)
					return err == nil || apierrors.IsNotFound(err)
				}).Should(BeTrue())
			}
			Eventually(func() bool {
				err := Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(current), &appsv1.ChainNode{})
				return apierrors.IsNotFound(err)
			}).Should(BeTrue())

			for _, object := range []client.Object{
				&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: node.Name, Namespace: ns.Name}},
				&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: node.Name, Namespace: ns.Name}},
			} {
				err := Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(object), object)
				if expectDeleted {
					Expect(apierrors.IsNotFound(err)).To(BeTrue())
					continue
				}
				Expect(err).NotTo(HaveOccurred())
				Expect(metav1.GetControllerOf(object)).To(BeNil())
			}
		},
		Entry("defaults to Retain", nil, false),
		Entry("deletes only with explicit Delete", &appsv1.DeletionPolicy{
			DataVolumes:   ptr.To(appsv1.DeletionPolicyDelete),
			GeneratedKeys: ptr.To(appsv1.DeletionPolicyDelete),
		}, true),
	)
})

const pvcProtectionFinalizer = "kubernetes.io/pvc-protection"

func containsString(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
