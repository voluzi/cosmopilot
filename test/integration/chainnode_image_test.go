package integration

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1 "github.com/voluzi/cosmopilot/v3/api/v1"
)

// Image resolution and pinning were previously covered only by unit tests against in-memory objects.
// These specs exercise the same behaviour through the real controllers and webhooks: what the
// ChainNodeSet propagates onto its generated ChainNodes, and what admission accepts or rejects.
var _ = Describe("Image resolution", func() {
	newNodeSet := func(ns *corev1.Namespace, mutate func(*appsv1.ChainNodeSet)) *appsv1.ChainNodeSet {
		nodeSet := &appsv1.ChainNodeSet{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: ChainNodeSetPrefix,
				Namespace:    ns.Name,
			},
			Spec: appsv1.ChainNodeSetSpec{
				App:     DefaultChainNodeSetTestApp,
				Nodes:   []appsv1.NodeGroupSpec{{Name: "fullnodes", Instances: ptr.To(1)}},
				Genesis: NewGenesisConfigWithChainID("test-chain"),
			},
		}
		mutate(nodeSet)
		return nodeSet
	}

	Context("Upgrade images", func() {
		// The regression this guards: an upgrade naming a different registry used to contribute only
		// its tag, so the node was sent to `<spec.app.image>:<tag>` — an image that was never
		// published. The full reference must survive onto the generated ChainNode.
		It("propagates a cross-registry upgrade image verbatim",
			WithNamespace(func(ns *corev1.Namespace) {
				nodeSet := newNodeSet(ns, func(s *appsv1.ChainNodeSet) {
					s.Spec.App.Upgrades = []appsv1.UpgradeSpec{
						{Height: 100, Image: "registry.example.com:5000/team/nibiru:v9.9.9"},
					}
				})
				Expect(Framework().Client().Create(Framework().Context(), nodeSet)).To(Succeed())
				WaitForChainNodeCount(ns.Name, 1)

				node := GetChainNodes(ns.Name)[0]
				Expect(node.Spec.App.Upgrades).To(HaveLen(1))
				Expect(node.Spec.App.Upgrades[0].Image).
					To(Equal("registry.example.com:5000/team/nibiru:v9.9.9"))

				// A registry port must not be mistaken for a tag.
				Expect(node.Spec.App.Upgrades[0].GetVersion()).To(Equal("v9.9.9"))
			}),
		)

		It("resolves the running image from the reached upgrade, not from spec.app.image",
			WithNamespace(func(ns *corev1.Namespace) {
				nodeSet := newNodeSet(ns, func(s *appsv1.ChainNodeSet) {
					s.Spec.App.Upgrades = []appsv1.UpgradeSpec{
						{Height: 10, Image: "registry.example.com/team/nibiru:v2"},
					}
				})
				Expect(Framework().Client().Create(Framework().Context(), nodeSet)).To(Succeed())
				WaitForChainNodeCount(ns.Name, 1)

				node := GetChainNodes(ns.Name)[0]
				node.Status.LatestHeight = 50
				node.Status.Upgrades = []appsv1.Upgrade{
					{Height: 10, Image: "registry.example.com/team/nibiru:v2", Status: appsv1.UpgradeCompleted},
				}
				Expect(node.GetAppImage()).To(Equal("registry.example.com/team/nibiru:v2"))
				Expect(node.GetAppVersion()).To(Equal("v2"))
			}),
		)
	})

	Context("Overrides", func() {
		It("propagates a group overrideImage to the generated ChainNode",
			WithNamespace(func(ns *corev1.Namespace) {
				nodeSet := newNodeSet(ns, func(s *appsv1.ChainNodeSet) {
					s.Spec.Nodes[0].OverrideImage = ptr.To("registry.example.com/team/nibiru:pinned")
				})
				Expect(Framework().Client().Create(Framework().Context(), nodeSet)).To(Succeed())
				WaitForChainNodeCount(ns.Name, 1)

				Eventually(func() *string {
					return GetChainNodes(ns.Name)[0].Spec.OverrideImage
				}).Should(HaveValue(Equal("registry.example.com/team/nibiru:pinned")))

				node := GetChainNodes(ns.Name)[0]
				Expect(node.GetAppImage()).To(Equal("registry.example.com/team/nibiru:pinned"))
				Expect(node.HasImageOverride()).To(BeTrue())
			}),
		)

		It("propagates a group overrideVersion against the configured repository",
			WithNamespace(func(ns *corev1.Namespace) {
				nodeSet := newNodeSet(ns, func(s *appsv1.ChainNodeSet) {
					s.Spec.Nodes[0].OverrideVersion = ptr.To("9.9.9")
				})
				Expect(Framework().Client().Create(Framework().Context(), nodeSet)).To(Succeed())
				WaitForChainNodeCount(ns.Name, 1)

				Eventually(func() *string {
					return GetChainNodes(ns.Name)[0].Spec.OverrideVersion
				}).Should(HaveValue(Equal("9.9.9")))

				node := GetChainNodes(ns.Name)[0]
				Expect(node.GetAppImage()).To(Equal(DefaultChainNodeSetTestApp.Image + ":9.9.9"))
			}),
		)

		// Preserving the two override fields independently used to leave a switched group carrying
		// both, which the ChainNode webhook then rejects as mutually exclusive — deadlocking the
		// ChainNodeSet, since it could never write its child again.
		It("allows switching a group from overrideImage to overrideVersion",
			WithNamespace(func(ns *corev1.Namespace) {
				nodeSet := newNodeSet(ns, func(s *appsv1.ChainNodeSet) {
					s.Spec.Nodes[0].OverrideImage = ptr.To("registry.example.com/team/nibiru:pinned")
				})
				Expect(Framework().Client().Create(Framework().Context(), nodeSet)).To(Succeed())
				WaitForChainNodeCount(ns.Name, 1)
				Eventually(func() *string {
					return GetChainNodes(ns.Name)[0].Spec.OverrideImage
				}).Should(HaveValue(Equal("registry.example.com/team/nibiru:pinned")))

				By("switching the group to overrideVersion")
				Eventually(func() error {
					current := &appsv1.ChainNodeSet{}
					if err := Framework().Client().Get(Framework().Context(),
						client.ObjectKeyFromObject(nodeSet), current); err != nil {
						return err
					}
					current.Spec.Nodes[0].OverrideImage = nil
					current.Spec.Nodes[0].OverrideVersion = ptr.To("9.9.9")
					return Framework().Client().Update(Framework().Context(), current)
				}).Should(Succeed())

				// The child must end up with only the version override. Carrying the stale image
				// override alongside it would be rejected by its own webhook.
				Eventually(func() bool {
					node := GetChainNodes(ns.Name)[0]
					return node.Spec.OverrideImage == nil &&
						node.Spec.OverrideVersion != nil && *node.Spec.OverrideVersion == "9.9.9"
				}).Should(BeTrue(), "child should carry only overrideVersion after the switch")
			}),
		)
	})

	Context("Admission", func() {
		It("rejects both overrides set on a node group",
			WithNamespace(func(ns *corev1.Namespace) {
				nodeSet := newNodeSet(ns, func(s *appsv1.ChainNodeSet) {
					s.Spec.Nodes[0].OverrideVersion = ptr.To("9.9.9")
					s.Spec.Nodes[0].OverrideImage = ptr.To("registry.example.com/team/nibiru:pinned")
				})
				err := Framework().Client().Create(Framework().Context(), nodeSet)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("mutually exclusive"))
			}),
		)

		It("rejects an overrideImage without a tag or digest",
			WithNamespace(func(ns *corev1.Namespace) {
				nodeSet := newNodeSet(ns, func(s *appsv1.ChainNodeSet) {
					s.Spec.Nodes[0].OverrideImage = ptr.To("registry.example.com/team/nibiru")
				})
				err := Framework().Client().Create(Framework().Context(), nodeSet)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("tag or digest"))
			}),
		)

		// A validator group is reconciled from .validator.<field>, so those are the overrides that
		// reach the generated ChainNode and the ones admission must check.
		It("rejects both overrides set inside a validator group",
			WithNamespace(func(ns *corev1.Namespace) {
				nodeSet := newNodeSet(ns, func(s *appsv1.ChainNodeSet) {
					s.Spec.Nodes[0].Validator = &appsv1.NodeSetValidatorConfig{
						OverrideVersion: ptr.To("9.9.9"),
						OverrideImage:   ptr.To("registry.example.com/team/nibiru:pinned"),
					}
				})
				err := Framework().Client().Create(Framework().Context(), nodeSet)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("mutually exclusive"))
			}),
		)

		It("rejects both overrides set on a standalone ChainNode",
			WithNamespace(func(ns *corev1.Namespace) {
				node := &appsv1.ChainNode{
					ObjectMeta: metav1.ObjectMeta{
						GenerateName: ChainNodePrefix,
						Namespace:    ns.Name,
					},
					Spec: appsv1.ChainNodeSpec{
						App:             DefaultChainNodeTestApp,
						Genesis:         NewGenesisConfigWithChainID("test-chain"),
						OverrideVersion: ptr.To("9.9.9"),
						OverrideImage:   ptr.To("registry.example.com/team/nibiru:pinned"),
					},
				}
				err := Framework().Client().Create(Framework().Context(), node)
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("mutually exclusive"))
			}),
		)
	})
})
