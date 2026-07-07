package integration

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
)

var _ = Describe("Cosmosigner Webhook Validation", func() {
	var ns *corev1.Namespace

	BeforeEach(func() {
		ns = CreateTestNamespace()
	})

	softwareBackend := func() appsv1.CosmosignerBackend {
		return appsv1.CosmosignerBackend{Software: &appsv1.CosmosignerSoftwareBackend{}}
	}
	vaultBackend := func() appsv1.CosmosignerBackend {
		return appsv1.CosmosignerBackend{Vault: &appsv1.CosmosignerVaultBackend{
			Address:     "https://vault:8200",
			KeyName:     "myval",
			TokenSecret: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "vault-token"}, Key: "token"},
		}}
	}

	newNodeSet := func(c *appsv1.Cosmosigner, groups []appsv1.NodeGroupSpec, validator *appsv1.NodeSetValidatorConfig) *appsv1.ChainNodeSet {
		return &appsv1.ChainNodeSet{
			ObjectMeta: metav1.ObjectMeta{GenerateName: ChainNodeSetPrefix, Namespace: ns.Name},
			Spec: appsv1.ChainNodeSetSpec{
				App:         DefaultChainNodeSetTestApp,
				Genesis:     &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis")},
				Nodes:       groups,
				Validator:   validator,
				Cosmosigner: c,
			},
		}
	}

	It("accepts a sentry-mode signer targeting a fullnode group", func() {
		cs := newNodeSet(
			&appsv1.Cosmosigner{NodeGroups: []string{"fullnodes"}, Replicas: ptr.To(int32(3)), Backend: vaultBackend()},
			[]appsv1.NodeGroupSpec{{Name: "fullnodes", Instances: ptr.To(3)}},
			nil,
		)
		Expect(Framework().Client().Create(Framework().Context(), cs)).To(Succeed())
	})

	It("accepts a signer defaulting to the validator when nodeGroups is empty", func() {
		cs := newNodeSet(
			&appsv1.Cosmosigner{Backend: softwareBackend()},
			[]appsv1.NodeGroupSpec{{Name: "fullnodes"}},
			&appsv1.NodeSetValidatorConfig{},
		)
		Expect(Framework().Client().Create(Framework().Context(), cs)).To(Succeed())
	})

	It("rejects a signer with no backend configured", func() {
		cs := newNodeSet(
			&appsv1.Cosmosigner{NodeGroups: []string{"fullnodes"}},
			[]appsv1.NodeGroupSpec{{Name: "fullnodes"}},
			nil,
		)
		err := Framework().Client().Create(Framework().Context(), cs)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("exactly one of software, vault or gcpKms"))
	})

	It("rejects an even replica count", func() {
		cs := newNodeSet(
			&appsv1.Cosmosigner{NodeGroups: []string{"fullnodes"}, Replicas: ptr.To(int32(2)), Backend: vaultBackend()},
			[]appsv1.NodeGroupSpec{{Name: "fullnodes"}},
			nil,
		)
		err := Framework().Client().Create(Framework().Context(), cs)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("odd number"))
	})

	It("rejects nodeGroups that do not exist", func() {
		cs := newNodeSet(
			&appsv1.Cosmosigner{NodeGroups: []string{"missing"}, Backend: vaultBackend()},
			[]appsv1.NodeGroupSpec{{Name: "fullnodes"}},
			nil,
		)
		err := Framework().Client().Create(Framework().Context(), cs)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("does not match any group"))
	})

	It("rejects empty nodeGroups without a validator", func() {
		cs := newNodeSet(
			&appsv1.Cosmosigner{Backend: vaultBackend()},
			[]appsv1.NodeGroupSpec{{Name: "fullnodes"}},
			nil,
		)
		err := Framework().Client().Create(Framework().Context(), cs)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("nodeGroups is required when .spec.validator is not set"))
	})

	It("rejects targeting a multi-instance validator group", func() {
		cs := newNodeSet(
			&appsv1.Cosmosigner{NodeGroups: []string{"validators"}, Backend: vaultBackend()},
			[]appsv1.NodeGroupSpec{{Name: "validators", Instances: ptr.To(3), Validator: &appsv1.NodeSetValidatorConfig{}}},
			nil,
		)
		err := Framework().Client().Create(Framework().Context(), cs)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("multiple instances"))
	})

	It("rejects a signer and tmKMS on the same targeted validator", func() {
		cs := newNodeSet(
			&appsv1.Cosmosigner{Backend: vaultBackend()},
			[]appsv1.NodeGroupSpec{{Name: "fullnodes"}},
			&appsv1.NodeSetValidatorConfig{TmKMS: &appsv1.TmKMS{Provider: appsv1.TmKmsProvider{Hashicorp: &appsv1.TmKmsHashicorpProvider{
				Address:     "https://vault:8200",
				Key:         "myval",
				TokenSecret: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "vault-token"}, Key: "token"},
			}}}},
		)
		err := Framework().Client().Create(Framework().Context(), cs)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("mutually exclusive"))
	})

	It("rejects a standalone ChainNode with both cosmosigner and tmKMS", func() {
		cn := &appsv1.ChainNode{
			ObjectMeta: metav1.ObjectMeta{GenerateName: ChainNodePrefix, Namespace: ns.Name},
			Spec: appsv1.ChainNodeSpec{
				App:     DefaultChainNodeTestApp,
				Genesis: &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis")},
				Validator: &appsv1.ValidatorConfig{TmKMS: &appsv1.TmKMS{Provider: appsv1.TmKmsProvider{Hashicorp: &appsv1.TmKmsHashicorpProvider{
					Address:     "https://vault:8200",
					Key:         "myval",
					TokenSecret: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "vault-token"}, Key: "token"},
				}}}},
				Cosmosigner: &appsv1.Cosmosigner{Backend: vaultBackend()},
			},
		}
		err := Framework().Client().Create(Framework().Context(), cn)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("mutually exclusive"))
	})

	It("rejects nodeGroups on a standalone ChainNode", func() {
		cn := &appsv1.ChainNode{
			ObjectMeta: metav1.ObjectMeta{GenerateName: ChainNodePrefix, Namespace: ns.Name},
			Spec: appsv1.ChainNodeSpec{
				App:         DefaultChainNodeTestApp,
				Genesis:     &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis")},
				Cosmosigner: &appsv1.Cosmosigner{NodeGroups: []string{"x"}, Backend: vaultBackend()},
			},
		}
		err := Framework().Client().Create(Framework().Context(), cn)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("nodeGroups is only valid on a ChainNodeSet"))
	})
})
