package integration

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	policyv1 "k8s.io/api/policy/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/voluzi/cosmopilot/v5/internal/cosmosigner"
)

var _ = Describe("Cosmosigner PodDisruptionBudget", func() {
	It("persists the signer eviction budget and unhealthy eviction policy", func() {
		ns := CreateTestNamespace()
		pdb := (cosmosigner.Params{Name: "validator-signer", Namespace: ns.Name}).PodDisruptionBudget()
		Expect(Framework().Client().Create(Framework().Context(), pdb)).To(Succeed())
		live := &policyv1.PodDisruptionBudget{}
		Expect(Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(pdb), live)).To(Succeed())
		Expect(live.Spec.MaxUnavailable.IntValue()).To(Equal(1))
		Expect(live.Spec.UnhealthyPodEvictionPolicy).NotTo(BeNil())
		Expect(*live.Spec.UnhealthyPodEvictionPolicy).To(Equal(policyv1.AlwaysAllow))
	})
})
