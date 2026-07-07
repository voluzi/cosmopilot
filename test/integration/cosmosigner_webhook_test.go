package integration

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

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
			// A plain (external-genesis) validator must supply its key for the software signer.
			&appsv1.NodeSetValidatorConfig{PrivateKeySecret: ptr.To("existing-val-key")},
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

	It("rejects targeting more than one validator group", func() {
		cs := newNodeSet(
			&appsv1.Cosmosigner{NodeGroups: []string{"val-a", "val-b"}, Backend: vaultBackend()},
			[]appsv1.NodeGroupSpec{
				{Name: "val-a", Validator: &appsv1.NodeSetValidatorConfig{}},
				{Name: "val-b", Validator: &appsv1.NodeSetValidatorConfig{}},
			},
			nil,
		)
		err := Framework().Client().Create(Framework().Context(), cs)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("cannot target more than one validator"))
	})

	It("rejects a sentry-mode software backend without an explicit key", func() {
		cs := newNodeSet(
			&appsv1.Cosmosigner{NodeGroups: []string{"fullnodes"}, Backend: softwareBackend()},
			[]appsv1.NodeGroupSpec{{Name: "fullnodes"}},
			nil,
		)
		err := Framework().Client().Create(Framework().Context(), cs)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("privateKeySecret is required when no validator is targeted"))
	})

	It("accepts a sentry-mode software backend with an explicit key", func() {
		cs := newNodeSet(
			&appsv1.Cosmosigner{NodeGroups: []string{"fullnodes"}, Backend: appsv1.CosmosignerBackend{
				Software: &appsv1.CosmosignerSoftwareBackend{PrivateKeySecret: ptr.To("my-key")},
			}},
			[]appsv1.NodeGroupSpec{{Name: "fullnodes"}},
			nil,
		)
		Expect(Framework().Client().Create(Framework().Context(), cs)).To(Succeed())
	})

	It("rejects uploadGenerated without a validator target", func() {
		vb := vaultBackend()
		vb.Vault.UploadGenerated = true
		cs := newNodeSet(
			&appsv1.Cosmosigner{NodeGroups: []string{"fullnodes"}, Backend: vb},
			[]appsv1.NodeGroupSpec{{Name: "fullnodes"}},
			nil,
		)
		err := Framework().Client().Create(Framework().Context(), cs)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("uploadGenerated requires targeting a validator"))
	})

	It("accepts uploadGenerated when a validator with an importable key is targeted", func() {
		vb := vaultBackend()
		vb.Vault.UploadGenerated = true
		cs := newNodeSet(
			&appsv1.Cosmosigner{Backend: vb},
			[]appsv1.NodeGroupSpec{{Name: "fullnodes"}},
			&appsv1.NodeSetValidatorConfig{PrivateKeySecret: ptr.To("existing-val-key")},
		)
		Expect(Framework().Client().Create(Framework().Context(), cs)).To(Succeed())
	})

	It("rejects an explicit software key when a validator is targeted", func() {
		cs := newNodeSet(
			&appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{
				Software: &appsv1.CosmosignerSoftwareBackend{PrivateKeySecret: ptr.To("some-key")},
			}},
			[]appsv1.NodeGroupSpec{{Name: "fullnodes"}},
			&appsv1.NodeSetValidatorConfig{},
		)
		err := Framework().Client().Create(Framework().Context(), cs)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("cannot be set when targeting a validator"))
	})

	It("rejects a pre-provisioned Vault key for a genesis-init validator target", func() {
		cs := newNodeSet(
			&appsv1.Cosmosigner{Backend: vaultBackend()}, // uploadGenerated=false
			[]appsv1.NodeGroupSpec{{Name: "fullnodes"}},
			&appsv1.NodeSetValidatorConfig{Init: &appsv1.GenesisInitConfig{
				ChainID: "test-localnet", Assets: []string{"10000000unibi"}, StakeAmount: "1000000unibi",
			}},
		)
		// Genesis-init + external genesis is itself mutually exclusive, so use init without genesis.
		cs.Spec.Genesis = nil
		err := Framework().Client().Create(Framework().Context(), cs)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("requires the software backend or vault.uploadGenerated"))
	})

	It("rejects targeting a zero-instance group", func() {
		// A zero-instance group has no pods to sign for; it is rejected (by group validation and,
		// defensively, by the cosmosigner target check).
		cs := newNodeSet(
			&appsv1.Cosmosigner{NodeGroups: []string{"fullnodes"}, Backend: appsv1.CosmosignerBackend{
				Software: &appsv1.CosmosignerSoftwareBackend{PrivateKeySecret: ptr.To("k")},
			}},
			[]appsv1.NodeGroupSpec{{Name: "fullnodes", Instances: ptr.To(0)}},
			nil,
		)
		Expect(Framework().Client().Create(Framework().Context(), cs)).NotTo(Succeed())
	})

	It("rejects a node group named 'signer' when cosmosigner is configured", func() {
		cs := newNodeSet(
			&appsv1.Cosmosigner{Backend: vaultBackend()},
			[]appsv1.NodeGroupSpec{{Name: "signer"}},
			&appsv1.NodeSetValidatorConfig{},
		)
		err := Framework().Client().Create(Framework().Context(), cs)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("is reserved when .spec.cosmosigner is configured"))
	})

	It("rejects two validators using the same Vault key via tmKMS and cosmosigner", func() {
		cs := newNodeSet(
			&appsv1.Cosmosigner{NodeGroups: []string{"fullnodes"}, Backend: vaultBackend()},
			[]appsv1.NodeGroupSpec{
				{Name: "fullnodes"},
				{Name: "val", Validator: &appsv1.NodeSetValidatorConfig{TmKMS: &appsv1.TmKMS{Provider: appsv1.TmKmsProvider{Hashicorp: &appsv1.TmKmsHashicorpProvider{
					Address:     "https://vault:8200",
					Key:         "myval", // same key the cosmosigner vaultBackend() uses
					TokenSecret: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "vault-token"}, Key: "token"},
				}}}}},
			},
			nil,
		)
		err := Framework().Client().Create(Framework().Context(), cs)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("same Vault signing key"))
	})

	It("rejects a standalone software signer on a non-validator node without a key", func() {
		cn := &appsv1.ChainNode{
			ObjectMeta: metav1.ObjectMeta{GenerateName: ChainNodePrefix, Namespace: ns.Name},
			Spec: appsv1.ChainNodeSpec{
				App:         DefaultChainNodeTestApp,
				Genesis:     &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis")},
				Cosmosigner: &appsv1.Cosmosigner{Backend: softwareBackend()},
			},
		}
		err := Framework().Client().Create(Framework().Context(), cn)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("privateKeySecret is required when the node is not a validator"))
	})

	It("rejects vault uploadGenerated on a non-validator standalone node", func() {
		vb := vaultBackend()
		vb.Vault.UploadGenerated = true
		cn := &appsv1.ChainNode{
			ObjectMeta: metav1.ObjectMeta{GenerateName: ChainNodePrefix, Namespace: ns.Name},
			Spec: appsv1.ChainNodeSpec{
				App:         DefaultChainNodeTestApp,
				Genesis:     &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis")},
				Cosmosigner: &appsv1.Cosmosigner{Backend: vb},
			},
		}
		err := Framework().Client().Create(Framework().Context(), cn)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("uploadGenerated requires the node to be a validator"))
	})

	It("rejects a non-controller ChainNodeSet ownerRef for remoteSignerTarget", func() {
		// A well-formed but non-controller owner reference must not pass the guard.
		cn := &appsv1.ChainNode{
			ObjectMeta: metav1.ObjectMeta{
				GenerateName: ChainNodePrefix,
				Namespace:    ns.Name,
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "cosmopilot.voluzi.com/v1",
					Kind:       "ChainNodeSet",
					Name:       "fake",
					UID:        "12345678-1234-1234-1234-123456789012",
					Controller: ptr.To(false),
				}},
			},
			Spec: appsv1.ChainNodeSpec{
				App:                DefaultChainNodeTestApp,
				Genesis:            &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis")},
				RemoteSignerTarget: true,
			},
		}
		err := Framework().Client().Create(Framework().Context(), cn)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("managed by the ChainNodeSet controller"))
	})

	It("rejects a pre-provisioned Vault key for a standalone genesis-init validator", func() {
		cn := &appsv1.ChainNode{
			ObjectMeta: metav1.ObjectMeta{GenerateName: ChainNodePrefix, Namespace: ns.Name},
			Spec: appsv1.ChainNodeSpec{
				App: DefaultChainNodeTestApp,
				Validator: &appsv1.ValidatorConfig{Init: &appsv1.GenesisInitConfig{
					ChainID: "test-localnet", Assets: []string{"10000000unibi"}, StakeAmount: "1000000unibi",
				}},
				Cosmosigner: &appsv1.Cosmosigner{Backend: vaultBackend()}, // uploadGenerated=false
			},
		}
		err := Framework().Client().Create(Framework().Context(), cn)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("requires the software backend or vault.uploadGenerated"))
	})

	It("rejects an explicit software key on a standalone validator", func() {
		cn := &appsv1.ChainNode{
			ObjectMeta: metav1.ObjectMeta{GenerateName: ChainNodePrefix, Namespace: ns.Name},
			Spec: appsv1.ChainNodeSpec{
				App: DefaultChainNodeTestApp,
				Validator: &appsv1.ValidatorConfig{Init: &appsv1.GenesisInitConfig{
					ChainID: "test-localnet", Assets: []string{"10000000unibi"}, StakeAmount: "1000000unibi",
				}},
				Cosmosigner: &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{
					Software: &appsv1.CosmosignerSoftwareBackend{PrivateKeySecret: ptr.To("other-key")},
				}},
			},
		}
		err := Framework().Client().Create(Framework().Context(), cn)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("cannot be set when the node is a validator"))
	})

	It("rejects a manually-set remoteSignerTarget on a standalone node", func() {
		cn := &appsv1.ChainNode{
			ObjectMeta: metav1.ObjectMeta{GenerateName: ChainNodePrefix, Namespace: ns.Name},
			Spec: appsv1.ChainNodeSpec{
				App:                DefaultChainNodeTestApp,
				Genesis:            &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis")},
				RemoteSignerTarget: true,
			},
		}
		err := Framework().Client().Create(Framework().Context(), cn)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("managed by the ChainNodeSet controller"))
	})

	It("rejects a Vault backend with an empty tokenSecret name", func() {
		cs := newNodeSet(
			&appsv1.Cosmosigner{NodeGroups: []string{"fullnodes"}, Backend: appsv1.CosmosignerBackend{
				Vault: &appsv1.CosmosignerVaultBackend{
					Address:     "https://vault:8200",
					KeyName:     "myval",
					TokenSecret: &corev1.SecretKeySelector{Key: "token"}, // no name
				},
			}},
			[]appsv1.NodeGroupSpec{{Name: "fullnodes"}},
			nil,
		)
		err := Framework().Client().Create(Framework().Context(), cs)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("tokenSecret.name and .key are required"))
	})

	It("rejects a software signer targeting a plain validator without a key", func() {
		cs := newNodeSet(
			&appsv1.Cosmosigner{Backend: softwareBackend()},
			[]appsv1.NodeGroupSpec{{Name: "fullnodes"}},
			&appsv1.NodeSetValidatorConfig{}, // plain external-genesis validator, no key
		)
		err := Framework().Client().Create(Framework().Context(), cs)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("requires the validator to set privateKeySecret"))
	})

	It("rejects a replicas change after creation", func() {
		cs := newNodeSet(
			&appsv1.Cosmosigner{NodeGroups: []string{"fullnodes"}, Replicas: ptr.To(int32(3)), Backend: vaultBackend()},
			[]appsv1.NodeGroupSpec{{Name: "fullnodes"}},
			nil,
		)
		Expect(Framework().Client().Create(Framework().Context(), cs)).To(Succeed())
		// Re-fetch and update in a retry loop: the controller reconciles the object concurrently, so a
		// stale-write conflict is retried until the webhook's immutability rule is what rejects it.
		Eventually(func() string {
			fresh := &appsv1.ChainNodeSet{}
			if err := Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(cs), fresh); err != nil {
				return err.Error()
			}
			fresh.Spec.Cosmosigner.Replicas = ptr.To(int32(1))
			if err := Framework().Client().Update(Framework().Context(), fresh); err != nil {
				return err.Error()
			}
			return ""
		}).Should(ContainSubstring("replicas is immutable"))
	})

	It("rejects a Vault backend with an empty tokenSecret key", func() {
		cs := newNodeSet(
			&appsv1.Cosmosigner{NodeGroups: []string{"fullnodes"}, Backend: appsv1.CosmosignerBackend{
				Vault: &appsv1.CosmosignerVaultBackend{
					Address:     "https://vault:8200",
					KeyName:     "myval",
					TokenSecret: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "vault-token"}}, // no key
				},
			}},
			[]appsv1.NodeGroupSpec{{Name: "fullnodes"}},
			nil,
		)
		err := Framework().Client().Create(Framework().Context(), cs)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("tokenSecret.name and .key are required"))
	})

	It("rejects changing the cosmosigner signing key after the chain is established", func() {
		cn := &appsv1.ChainNode{
			ObjectMeta: metav1.ObjectMeta{GenerateName: ChainNodePrefix, Namespace: ns.Name},
			Spec: appsv1.ChainNodeSpec{
				App:         DefaultChainNodeTestApp,
				Genesis:     &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis")},
				Cosmosigner: &appsv1.Cosmosigner{Backend: vaultBackend()},
			},
		}
		Expect(Framework().Client().Create(Framework().Context(), cn)).To(Succeed())

		// Mark the chain as established.
		cn.Status.ChainID = "test-chain-1"
		Expect(Framework().Client().Status().Update(Framework().Context(), cn)).To(Succeed())

		// Changing the Vault key is now rejected.
		Eventually(func() string {
			fresh := &appsv1.ChainNode{}
			if err := Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(cn), fresh); err != nil {
				return err.Error()
			}
			fresh.Spec.Cosmosigner.Backend.Vault.KeyName = "different-key"
			if err := Framework().Client().Update(Framework().Context(), fresh); err != nil {
				return err.Error()
			}
			return ""
		}).Should(ContainSubstring("immutable after the chain is established"))
	})

	It("allows a same-key migration from tmKMS to cosmosigner after the chain is established", func() {
		// A validator signing via tmKMS on the same Vault key it later uses through cosmosigner is a
		// supported migration: the effective key is unchanged, so it must be accepted.
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
			},
		}
		Expect(Framework().Client().Create(Framework().Context(), cn)).To(Succeed())
		cn.Status.ChainID = "test-chain-1"
		Expect(Framework().Client().Status().Update(Framework().Context(), cn)).To(Succeed())

		// Switch to cosmosigner pointing at the same Vault transit key (default mount, no namespace).
		Eventually(func() error {
			fresh := &appsv1.ChainNode{}
			if err := Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(cn), fresh); err != nil {
				return err
			}
			fresh.Spec.Validator.TmKMS = nil
			fresh.Spec.Cosmosigner = &appsv1.Cosmosigner{Backend: vaultBackend()} // keyName "myval", same address
			return Framework().Client().Update(Framework().Context(), fresh)
		}).Should(Succeed())
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
