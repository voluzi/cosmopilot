package integration

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1 "github.com/voluzi/cosmopilot/v5/api/v1"
)

var _ = Describe("PKCS11 Cosmosigner admission", func() {
	var ns *corev1.Namespace
	BeforeEach(func() { ns = CreateTestNamespace() })
	signer := func() *appsv1.Cosmosigner {
		return &appsv1.Cosmosigner{
			Replicas: ptr.To(int32(1)), Image: ptr.To("ghcr.io/voluzi/cosmosigner:edge-pkcs11"),
			Backend: appsv1.CosmosignerBackend{PKCS11: &appsv1.CosmosignerPKCS11Backend{
				Module: "/vendor/module.so", Slot: ptr.To(int64(0)), KeyLabel: "validator", KeyID: "01aB", PublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
				PINSecret: corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "pin"}, Key: "pin"},
			}},
			Env:          []corev1.EnvVar{{Name: "VENDOR_CONFIG", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "vendor"}, Key: "config"}}}},
			Volumes:      []corev1.Volume{{Name: "vendor", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{SecretName: "vendor"}}}},
			VolumeMounts: []corev1.VolumeMount{{Name: "vendor", MountPath: "/vendor", ReadOnly: true}},
		}
	}
	object := func(placement string, c *appsv1.Cosmosigner) client.Object {
		if placement == "ChainNode" {
			return &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{GenerateName: ChainNodePrefix, Namespace: ns.Name}, Spec: appsv1.ChainNodeSpec{App: DefaultChainNodeTestApp, Genesis: &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis")}, Cosmosigner: c}}
		}
		set := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{GenerateName: ChainNodeSetPrefix, Namespace: ns.Name}, Spec: appsv1.ChainNodeSetSpec{App: DefaultChainNodeSetTestApp, Genesis: &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis")}, Nodes: []appsv1.NodeGroupSpec{{Name: "sentries", Instances: ptr.To(1)}}}}
		if placement == "group" {
			set.Spec.Nodes[0].Cosmosigner = c
		} else {
			c.NodeGroups = []string{"sentries"}
			set.Spec.Cosmosigner = c
		}
		return set
	}
	getSigner := func(obj client.Object, placement string) *appsv1.Cosmosigner {
		switch v := obj.(type) {
		case *appsv1.ChainNode:
			return v.Spec.Cosmosigner
		case *appsv1.ChainNodeSet:
			if placement == "group" {
				return v.Spec.Nodes[0].Cosmosigner
			}
			return v.Spec.Cosmosigner
		}
		return nil
	}
	It("persists slot zero, PIN and vendor extensions and admits same-key addressing changes in all placements", func() {
		for _, placement := range []string{"ChainNode", "ChainNodeSet", "group"} {
			c := signer()
			obj := object(placement, c)
			Expect(Framework().Client().Create(Framework().Context(), obj)).To(Succeed())
			live := obj.DeepCopyObject().(client.Object)
			Expect(Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(obj), live)).To(Succeed())
			Expect(getSigner(live, placement)).To(Equal(c))
			Eventually(func() error {
				if err := Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(obj), live); err != nil {
					return err
				}
				p := getSigner(live, placement).Backend.PKCS11
				p.Slot = nil
				p.TokenLabel = "validator"
				p.Module = "/other/module.so"
				return Framework().Client().Update(Framework().Context(), live)
			}).Should(Succeed())
		}
	})
	It("admits only a public-key correction before rollout in all placements", func() {
		for _, placement := range []string{"ChainNode", "ChainNodeSet", "group"} {
			obj := object(placement, signer())
			Expect(Framework().Client().Create(Framework().Context(), obj)).To(Succeed())
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				if err := Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(obj), obj); err != nil {
					return err
				}
				switch v := obj.(type) {
				case *appsv1.ChainNode:
					v.SetEstablishedChainID("external-1")
				case *appsv1.ChainNodeSet:
					v.SetEstablishedChainID("external-1")
				}
				return Framework().Client().Status().Update(Framework().Context(), obj)
			})).To(Succeed())
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				if err := Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(obj), obj); err != nil {
					return err
				}
				getSigner(obj, placement).Backend.PKCS11.PublicKey = "AQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
				return Framework().Client().Update(Framework().Context(), obj)
			})).To(Succeed())
			Expect(retry.RetryOnConflict(retry.DefaultRetry, func() error {
				if err := Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(obj), obj); err != nil {
					return err
				}
				switch v := obj.(type) {
				case *appsv1.ChainNode:
					v.Status.CosmosignerSigningDigest = "served"
				case *appsv1.ChainNodeSet:
					v.Status.Cosmosigners[0].SigningDigest = "served"
				}
				return Framework().Client().Status().Update(Framework().Context(), obj)
			})).To(Succeed())
			err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
				if err := Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(obj), obj); err != nil {
					return err
				}
				getSigner(obj, placement).Backend.PKCS11.PublicKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
				return Framework().Client().Update(Framework().Context(), obj)
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("records its applied public key"))
		}
	})
	It("rejects malformed backend shapes in all placements", func() {
		for _, placement := range []string{"ChainNode", "ChainNodeSet", "group"} {
			for _, mutate := range []func(*appsv1.CosmosignerPKCS11Backend){
				func(p *appsv1.CosmosignerPKCS11Backend) { p.TokenLabel = "both" }, func(p *appsv1.CosmosignerPKCS11Backend) { p.Slot = nil },
				func(p *appsv1.CosmosignerPKCS11Backend) { p.KeyLabel = ""; p.KeyID = "" }, func(p *appsv1.CosmosignerPKCS11Backend) { p.KeyID = "abc" },
				func(p *appsv1.CosmosignerPKCS11Backend) { p.PublicKey = "not-base64" }, func(p *appsv1.CosmosignerPKCS11Backend) { p.Module = "" },
				func(p *appsv1.CosmosignerPKCS11Backend) { p.PINSecret.Name = "" },
				func(p *appsv1.CosmosignerPKCS11Backend) { p.PINSecret.Key = "" },
				func(p *appsv1.CosmosignerPKCS11Backend) { p.Slot = ptr.To(int64(-1)) },
			} {
				c := signer()
				mutate(c.Backend.PKCS11)
				Expect(Framework().Client().Create(Framework().Context(), object(placement, c))).NotTo(Succeed())
			}
		}
	})
	It("rejects duplicate public keys at different coordinates on creation and update", func() {
		for _, placement := range []string{"ChainNodeSet", "group"} {
			set := object(placement, signer()).(*appsv1.ChainNodeSet)
			other := signer()
			other.Backend.PKCS11.Module = "/other/module.so"
			other.Backend.PKCS11.Slot = nil
			other.Backend.PKCS11.TokenLabel = "other-token"
			other.Backend.PKCS11.KeyID = "02"
			set.Spec.Nodes = append(set.Spec.Nodes, appsv1.NodeGroupSpec{Name: "other", Instances: ptr.To(1), Cosmosigner: other})
			err := Framework().Client().Create(Framework().Context(), set)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("same PKCS#11 signing key"))
			other.Backend.PKCS11.PublicKey = "AQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
			Expect(Framework().Client().Create(Framework().Context(), set)).To(Succeed())
			err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
				if err := Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(set), set); err != nil {
					return err
				}
				set.Spec.Nodes[1].Cosmosigner.Backend.PKCS11.PublicKey = getSigner(set, placement).Backend.PKCS11.PublicKey
				return Framework().Client().Update(Framework().Context(), set)
			})
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("same PKCS#11 signing key"))
		}
	})
	It("rejects duplicate coordinates and local genesis registration", func() {
		set := object("group", signer()).(*appsv1.ChainNodeSet)
		set.Spec.Nodes = append(set.Spec.Nodes, appsv1.NodeGroupSpec{Name: "other", Instances: ptr.To(1), Cosmosigner: signer()})
		err := Framework().Client().Create(Framework().Context(), set)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("same PKCS#11 signing key"))
		for _, placement := range []string{"ChainNode", "ChainNodeSet", "group"} {
			obj := object(placement, signer())
			init := &appsv1.GenesisInitConfig{ChainID: "test-1", Assets: []string{"1000000utest"}, StakeAmount: "100000utest"}
			switch v := obj.(type) {
			case *appsv1.ChainNode:
				v.Spec.Genesis = nil
				v.Spec.Validator = &appsv1.ValidatorConfig{Init: init}
			case *appsv1.ChainNodeSet:
				v.Spec.Genesis = nil
				val := &appsv1.NodeSetValidatorConfig{Init: init}
				if placement == "group" {
					v.Spec.Nodes[0].Validator = val
				} else {
					v.Spec.Cosmosigner.NodeGroups = nil
					v.Spec.Validator = val
				}
			}
			err := Framework().Client().Create(Framework().Context(), obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("requires the software backend, vault.uploadGenerated or gcpKms.import"))
		}
	})
})
