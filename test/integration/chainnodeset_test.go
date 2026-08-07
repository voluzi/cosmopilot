package integration

import (
	"strings"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1 "github.com/voluzi/cosmopilot/v3/api/v1"
	"github.com/voluzi/cosmopilot/v3/internal/controllers"
)

var _ = Describe("ChainNodeSet", func() {
	Context("ChainNode Creation", func() {
		It("should create ChainNodes for node groups",
			WithNamespace(func(ns *corev1.Namespace) {
				chainNodeSet := &appsv1.ChainNodeSet{
					ObjectMeta: metav1.ObjectMeta{
						GenerateName: ChainNodeSetPrefix,
						Namespace:    ns.Name,
					},
					Spec: appsv1.ChainNodeSetSpec{
						App:     DefaultChainNodeSetTestApp,
						Nodes:   []appsv1.NodeGroupSpec{{Name: "fullnodes", Instances: ptr.To(2)}},
						Genesis: NewGenesisConfigWithChainID("test-chain"),
					},
				}

				err := Framework().Client().Create(Framework().Context(), chainNodeSet)
				Expect(err).NotTo(HaveOccurred())

				// Wait for ChainNodes to be created (2 fullnodes)
				WaitForChainNodeCount(ns.Name, 2)

				// Verify ChainNode naming convention
				chainNodes := GetChainNodes(ns.Name)
				names := make([]string, len(chainNodes))
				for i, cn := range chainNodes {
					names[i] = cn.Name
				}
				Expect(names).To(ContainElements(
					chainNodeSet.Name+"-fullnodes-0",
					chainNodeSet.Name+"-fullnodes-1",
				))
			}),
		)

		It("should create ChainNodes for multiple node groups",
			WithNamespace(func(ns *corev1.Namespace) {
				chainNodeSet := &appsv1.ChainNodeSet{
					ObjectMeta: metav1.ObjectMeta{
						GenerateName: ChainNodeSetPrefix,
						Namespace:    ns.Name,
					},
					Spec: appsv1.ChainNodeSetSpec{
						App: DefaultChainNodeSetTestApp,
						Nodes: []appsv1.NodeGroupSpec{
							{Name: "fullnodes", Instances: ptr.To(2)},
							{Name: "archive", Instances: ptr.To(1)},
						},
						Genesis: NewGenesisConfigWithChainID("test-chain"),
					},
				}

				err := Framework().Client().Create(Framework().Context(), chainNodeSet)
				Expect(err).NotTo(HaveOccurred())

				// Wait for ChainNodes to be created (2 fullnodes + 1 archive = 3)
				WaitForChainNodeCount(ns.Name, 3)
			}),
		)

		It("should label ChainNodes with group name",
			WithNamespace(func(ns *corev1.Namespace) {
				chainNodeSet := &appsv1.ChainNodeSet{
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

				err := Framework().Client().Create(Framework().Context(), chainNodeSet)
				Expect(err).NotTo(HaveOccurred())

				// Wait for ChainNode to be created
				WaitForChainNodeCount(ns.Name, 1)

				// Verify labels
				chainNodes := GetChainNodes(ns.Name)
				cn := chainNodes[0]
				Expect(cn.Labels[controllers.LabelChainNodeSet]).To(Equal(chainNodeSet.Name))
				Expect(cn.Labels[controllers.LabelChainNodeSetGroup]).To(Equal("fullnodes"))
			}),
		)

		It("should propagate persistence config to ChainNodes",
			WithNamespace(func(ns *corev1.Namespace) {
				chainNodeSet := &appsv1.ChainNodeSet{
					ObjectMeta: metav1.ObjectMeta{
						GenerateName: ChainNodeSetPrefix,
						Namespace:    ns.Name,
					},
					Spec: appsv1.ChainNodeSetSpec{
						App: DefaultChainNodeSetTestApp,
						Nodes: []appsv1.NodeGroupSpec{{
							Name:      "fullnodes",
							Instances: ptr.To(1),
							Persistence: &appsv1.Persistence{
								Size: ptr.To("500Gi"),
							},
						}},
						Genesis: NewGenesisConfigWithChainID("test-chain"),
					},
				}

				err := Framework().Client().Create(Framework().Context(), chainNodeSet)
				Expect(err).NotTo(HaveOccurred())

				// Wait for ChainNode to be created
				WaitForChainNodeCount(ns.Name, 1)

				// Verify persistence was propagated
				chainNodes := GetChainNodes(ns.Name)
				cn := chainNodes[0]
				Expect(cn.Spec.Persistence).NotTo(BeNil())
				Expect(*cn.Spec.Persistence.Size).To(Equal("500Gi"))
			}),
		)
	})

	Context("Scaling", func() {
		It("should scale up ChainNodes when instances increase",
			WithNamespace(func(ns *corev1.Namespace) {
				chainNodeSet := &appsv1.ChainNodeSet{
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

				err := Framework().Client().Create(Framework().Context(), chainNodeSet)
				Expect(err).NotTo(HaveOccurred())

				// Wait for initial ChainNode
				WaitForChainNodeCount(ns.Name, 1)

				// Scale up to 3
				Eventually(func() error {
					current := &appsv1.ChainNodeSet{}
					if err := Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(chainNodeSet), current); err != nil {
						return err
					}
					current.Spec.Nodes[0].Instances = ptr.To(3)
					return Framework().Client().Update(Framework().Context(), current)
				}).Should(Succeed())

				// Wait for scale up
				WaitForChainNodeCount(ns.Name, 3)
			}),
		)

		It("should scale down ChainNodes when instances decrease",
			WithNamespace(func(ns *corev1.Namespace) {
				chainNodeSet := &appsv1.ChainNodeSet{
					ObjectMeta: metav1.ObjectMeta{
						GenerateName: ChainNodeSetPrefix,
						Namespace:    ns.Name,
					},
					Spec: appsv1.ChainNodeSetSpec{
						App:     DefaultChainNodeSetTestApp,
						Nodes:   []appsv1.NodeGroupSpec{{Name: "fullnodes", Instances: ptr.To(3)}},
						Genesis: NewGenesisConfigWithChainID("test-chain"),
					},
				}

				err := Framework().Client().Create(Framework().Context(), chainNodeSet)
				Expect(err).NotTo(HaveOccurred())

				// Wait for initial ChainNodes
				WaitForChainNodeCount(ns.Name, 3)

				// Scale down to 1
				Eventually(func() error {
					current := &appsv1.ChainNodeSet{}
					if err := Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(chainNodeSet), current); err != nil {
						return err
					}
					current.Spec.Nodes[0].Instances = ptr.To(1)
					return Framework().Client().Update(Framework().Context(), current)
				}).Should(Succeed())

				// Wait for scale down
				WaitForChainNodeCount(ns.Name, 1)
			}),
		)
	})

	Context("Status", func() {
		It("should set ChainID in status",
			WithNamespace(func(ns *corev1.Namespace) {
				chainNodeSet := &appsv1.ChainNodeSet{
					ObjectMeta: metav1.ObjectMeta{
						GenerateName: ChainNodeSetPrefix,
						Namespace:    ns.Name,
					},
					Spec: appsv1.ChainNodeSetSpec{
						App:     DefaultChainNodeSetTestApp,
						Nodes:   []appsv1.NodeGroupSpec{{Name: "fullnodes", Instances: ptr.To(1)}},
						Genesis: NewGenesisConfigWithChainID("my-test-chain"),
					},
				}

				err := Framework().Client().Create(Framework().Context(), chainNodeSet)
				Expect(err).NotTo(HaveOccurred())

				// Wait for ChainID to be set in status
				Eventually(func() string {
					current := &appsv1.ChainNodeSet{}
					if err := Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(chainNodeSet), current); err != nil {
						return ""
					}
					return current.Status.ChainID
				}).Should(Equal("my-test-chain"))
			}),
		)

		It("should set instance count in status",
			WithNamespace(func(ns *corev1.Namespace) {
				chainNodeSet := &appsv1.ChainNodeSet{
					ObjectMeta: metav1.ObjectMeta{
						GenerateName: ChainNodeSetPrefix,
						Namespace:    ns.Name,
					},
					Spec: appsv1.ChainNodeSetSpec{
						App: DefaultChainNodeSetTestApp,
						Nodes: []appsv1.NodeGroupSpec{
							{Name: "fullnodes", Instances: ptr.To(2)},
							{Name: "archive", Instances: ptr.To(1)},
						},
						Genesis: NewGenesisConfigWithChainID("test-chain"),
					},
				}

				err := Framework().Client().Create(Framework().Context(), chainNodeSet)
				Expect(err).NotTo(HaveOccurred())

				// Wait for instance count in status (2 fullnodes + 1 archive = 3)
				Eventually(func() int {
					current := &appsv1.ChainNodeSet{}
					if err := Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(chainNodeSet), current); err != nil {
						return -1
					}
					return current.Status.Instances
				}).Should(Equal(3))
			}),
		)

		It("should track ready instances in status",
			WithNamespace(func(ns *corev1.Namespace) {
				chainNodeSet := &appsv1.ChainNodeSet{
					ObjectMeta: metav1.ObjectMeta{
						GenerateName: ChainNodeSetPrefix,
						Namespace:    ns.Name,
					},
					Spec: appsv1.ChainNodeSetSpec{
						App:     DefaultChainNodeSetTestApp,
						Nodes:   []appsv1.NodeGroupSpec{{Name: "fullnodes", Instances: ptr.To(2)}},
						Genesis: NewGenesisConfigWithChainID("test-chain"),
					},
				}

				err := Framework().Client().Create(Framework().Context(), chainNodeSet)
				Expect(err).NotTo(HaveOccurred())

				WaitForChainNodeCount(ns.Name, 2)

				// No pod ever runs in envtest, so the ChainNode controller keeps driving its children
				// back to InitializingData. Re-assert the desired phases on every poll so the parent
				// reconcile observes them, instead of racing a single one-shot patch. Writes that lose
				// to a concurrent controller update are ignored — the next poll re-applies them.
				readyInstancesWithPhases := func(phases map[string]appsv1.ChainNodePhase) func() int {
					return func() int {
						for _, node := range GetChainNodes(ns.Name) {
							phase, ok := phases[node.Name[strings.LastIndex(node.Name, "-")+1:]]
							if !ok {
								continue
							}
							node.Status.Phase = phase
							_ = Framework().Client().Status().Update(Framework().Context(), &node)
						}
						current := &appsv1.ChainNodeSet{}
						if err := Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(chainNodeSet), current); err != nil {
							return -1
						}
						return current.Status.ReadyInstances
					}
				}

				Eventually(readyInstancesWithPhases(map[string]appsv1.ChainNodePhase{
					"0": appsv1.PhaseChainNodeRunning,
					"1": appsv1.PhaseChainNodeRunning,
				}), time.Minute).Should(Equal(2))

				// A single degraded child must be visible on the parent.
				Eventually(readyInstancesWithPhases(map[string]appsv1.ChainNodePhase{
					"0": appsv1.PhaseChainNodeError,
					"1": appsv1.PhaseChainNodeRunning,
				}), time.Minute).Should(Equal(1))
			}),
		)
	})

	Context("Owner References", func() {
		It("should set owner references on ChainNodes for garbage collection",
			WithNamespace(func(ns *corev1.Namespace) {
				chainNodeSet := &appsv1.ChainNodeSet{
					ObjectMeta: metav1.ObjectMeta{
						GenerateName: ChainNodeSetPrefix,
						Namespace:    ns.Name,
					},
					Spec: appsv1.ChainNodeSetSpec{
						App:     DefaultChainNodeSetTestApp,
						Nodes:   []appsv1.NodeGroupSpec{{Name: "fullnodes", Instances: ptr.To(2)}},
						Genesis: NewGenesisConfigWithChainID("test-chain"),
					},
				}

				err := Framework().Client().Create(Framework().Context(), chainNodeSet)
				Expect(err).NotTo(HaveOccurred())

				// Wait for ChainNodes to be created
				WaitForChainNodeCount(ns.Name, 2)

				// Get the ChainNodeSet to have its UID
				err = Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(chainNodeSet), chainNodeSet)
				Expect(err).NotTo(HaveOccurred())

				// Verify owner references are set correctly on all ChainNodes
				chainNodes := GetChainNodes(ns.Name)
				for _, cn := range chainNodes {
					Expect(cn.OwnerReferences).To(HaveLen(1))
					Expect(cn.OwnerReferences[0].Name).To(Equal(chainNodeSet.Name))
					Expect(cn.OwnerReferences[0].UID).To(Equal(chainNodeSet.UID))
					Expect(cn.OwnerReferences[0].Kind).To(Equal("ChainNodeSet"))
					Expect(*cn.OwnerReferences[0].Controller).To(BeTrue())
				}
			}),
		)
	})
})
