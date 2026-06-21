package e2e

import (
	"encoding/json"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/chainutils"
	"github.com/voluzi/cosmopilot/v2/test/e2e/apps"
)

var _ = Describe("ChainNodeSet Multi-Validator Genesis", func() {
	Context("Group validator genesis initialization", func() {
		apps.ForEachApp("should initialize genesis with multiple group validators",
			WithNamespace(func(app apps.TestApp, ns *corev1.Namespace) {
				chainNodeSet := app.BuildChainNodeSet(ns.Name, 0)
				validator := chainNodeSet.Spec.Validator
				Expect(validator).NotTo(BeNil())
				Expect(validator.Init).NotTo(BeNil())

				chainID := validator.Init.ChainID
				chainNodeSet.Spec.Validator = nil
				chainNodeSet.Spec.Genesis = nil
				chainNodeSet.Spec.Nodes = []appsv1.NodeGroupSpec{{
					Name:      "validators",
					Instances: ptr.To(2),
					Validator: validator,
				}}

				err := Framework().Client().Create(Framework().Context(), chainNodeSet)
				Expect(err).NotTo(HaveOccurred())

				WaitForChainNodeSetRunning(chainNodeSet)
				WaitForChainNodeCount(ns.Name, 2)

				RefreshChainNodeSet(chainNodeSet)
				Expect(chainNodeSet.Status.ChainID).To(Equal(chainID))
				Expect(chainNodeSet.Status.Validators).To(HaveLen(2))
				Expect(chainNodeSet.Status.Validators[0].Name).To(Equal(fmt.Sprintf("%s-validators-0", chainNodeSet.Name)))
				Expect(chainNodeSet.Status.Validators[1].Name).To(Equal(fmt.Sprintf("%s-validators-1", chainNodeSet.Name)))

				initValidator := &appsv1.ChainNode{}
				err = Framework().Client().Get(Framework().Context(), client.ObjectKey{
					Namespace: ns.Name,
					Name:      fmt.Sprintf("%s-validators-0", chainNodeSet.Name),
				}, initValidator)
				Expect(err).NotTo(HaveOccurred())
				Expect(initValidator.Spec.Validator).NotTo(BeNil())
				Expect(initValidator.Spec.Validator.Init).NotTo(BeNil())
				Expect(initValidator.Spec.Genesis).To(BeNil())

				nonInitValidator := &appsv1.ChainNode{}
				err = Framework().Client().Get(Framework().Context(), client.ObjectKey{
					Namespace: ns.Name,
					Name:      fmt.Sprintf("%s-validators-1", chainNodeSet.Name),
				}, nonInitValidator)
				Expect(err).NotTo(HaveOccurred())
				Expect(nonInitValidator.Spec.Validator).NotTo(BeNil())
				Expect(nonInitValidator.Spec.Validator.Init).To(BeNil())
				Expect(nonInitValidator.Spec.Genesis).NotTo(BeNil())
				Expect(nonInitValidator.Spec.Genesis.ConfigMap).NotTo(BeNil())
				Expect(*nonInitValidator.Spec.Genesis.ConfigMap).To(Equal(fmt.Sprintf("%s-genesis", chainID)))
				Expect(*nonInitValidator.Spec.Genesis.ConfigMap).NotTo(Equal("-genesis"))

				genesis, err := Framework().KubeClient().CoreV1().ConfigMaps(ns.GetName()).Get(
					Framework().Context(),
					fmt.Sprintf("%s-genesis", chainID),
					metav1.GetOptions{},
				)
				Expect(err).NotTo(HaveOccurred())
				Expect(genesis.Data[chainutils.GenesisFilename]).NotTo(BeEmpty())

				// The generated genesis must register BOTH group validators — one gentx (MsgCreateValidator)
				// per instance — so both are part of the initial validator set, not just instance 0.
				var genDoc struct {
					AppState struct {
						Genutil struct {
							GenTxs []json.RawMessage `json:"gen_txs"`
						} `json:"genutil"`
					} `json:"app_state"`
				}
				Expect(json.Unmarshal([]byte(genesis.Data[chainutils.GenesisFilename]), &genDoc)).To(Succeed())
				Expect(genDoc.AppState.Genutil.GenTxs).To(HaveLen(2),
					"genesis must contain one gentx per group validator so both join the initial validator set")

				WaitForChainNodesHeight(chainNodeSet, 1)

				// Both validators must end up effectively in the on-chain consensus set: the controller
				// sources ValidatorAddress/Status/PubKey from a live staking query, so both reporting
				// "bonded" with a valoper address and a distinct consensus key proves both are active
				// genesis validators (a 2-equal-power set can only advance if both actually have power).
				Eventually(func(g Gomega) {
					RefreshChainNodeSet(chainNodeSet)
					vals := chainNodeSet.Status.Validators
					g.Expect(vals).To(HaveLen(2))
					pubKeys := map[string]struct{}{}
					for i, v := range vals {
						g.Expect(v.Group).To(Equal("validators"), "validator %d group", i)
						g.Expect(v.Init).To(BeTrue(), "validator %d should be flagged init", i)
						g.Expect(v.PubKey).NotTo(BeEmpty(), "validator %d pubKey", i)
						g.Expect(v.Address).NotTo(BeEmpty(), "validator %d valoper address", i)
						g.Expect(v.Status).To(BeEquivalentTo(appsv1.ValidatorStatusBonded), "validator %d should be bonded", i)
						g.Expect(v.SigningKeyDigest).NotTo(BeEmpty(), "validator %d signing digest", i)
						pubKeys[v.PubKey] = struct{}{}
					}
					g.Expect(pubKeys).To(HaveLen(2), "validators must use distinct consensus keys")
				}).Should(Succeed())
			}),
		)
	})
})
