package integration

import (
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1 "github.com/voluzi/cosmopilot/v5/api/v1"
)

var _ = Describe("AWS KMS Cosmosigner admission", func() {
	const arn = "arn:aws:kms:eu-west-1:123456789012:key/12345678-1234-1234-1234-123456789012"
	var ns *corev1.Namespace
	BeforeEach(func() { ns = CreateTestNamespace() })

	signer := func() *appsv1.Cosmosigner {
		return &appsv1.Cosmosigner{Image: ptr.To("ghcr.io/voluzi/cosmosigner:edge"), Backend: appsv1.CosmosignerBackend{AwsKMS: &appsv1.CosmosignerAwsKmsBackend{
			KeyID: arn, Region: "eu-west-1", ClaimRoleARN: ptr.To("arn:aws:iam::123456789012:role/claimer"), Timeout: ptr.To("20s"),
			CredentialsSecret: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "aws-credentials"}, Key: "credentials"},
		}}}
	}
	object := func(placement string, c *appsv1.Cosmosigner) client.Object {
		if placement == "ChainNode" {
			return &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{GenerateName: ChainNodePrefix, Namespace: ns.Name}, Spec: appsv1.ChainNodeSpec{
				App: DefaultChainNodeTestApp, Genesis: &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis")}, Cosmosigner: c,
			}}
		}
		set := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{GenerateName: ChainNodeSetPrefix, Namespace: ns.Name}, Spec: appsv1.ChainNodeSetSpec{
			App: DefaultChainNodeSetTestApp, Genesis: &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis")}, Nodes: []appsv1.NodeGroupSpec{{Name: "sentries", Instances: ptr.To(1)}},
		}}
		if placement == "group" {
			set.Spec.Nodes[0].Cosmosigner = c
		} else {
			c.NodeGroups = []string{"sentries"}
			set.Spec.Cosmosigner = c
		}
		return set
	}

	It("persists AWS settings and admits a key ARN change in every placement", func() {
		ctx := Framework().Context()
		for _, placement := range []string{"ChainNode", "ChainNodeSet", "group"} {
			c := signer()
			obj := object(placement, c)
			Expect(Framework().Client().Create(ctx, obj)).To(Succeed())
			live := obj.DeepCopyObject().(client.Object)
			Expect(Framework().Client().Get(ctx, client.ObjectKeyFromObject(obj), live)).To(Succeed())
			var backend *appsv1.CosmosignerAwsKmsBackend
			switch v := live.(type) {
			case *appsv1.ChainNode:
				backend = v.Spec.Cosmosigner.Backend.AwsKMS
			case *appsv1.ChainNodeSet:
				if placement == "group" {
					backend = v.Spec.Nodes[0].Cosmosigner.Backend.AwsKMS
				} else {
					backend = v.Spec.Cosmosigner.Backend.AwsKMS
				}
			}
			Expect(backend).To(Equal(c.Backend.AwsKMS))
			Eventually(func() error {
				if err := Framework().Client().Get(ctx, client.ObjectKeyFromObject(obj), live); err != nil {
					return err
				}
				switch v := live.(type) {
				case *appsv1.ChainNode:
					v.Spec.Cosmosigner.Backend.AwsKMS.KeyID = arn + "-other"
				case *appsv1.ChainNodeSet:
					if placement == "group" {
						v.Spec.Nodes[0].Cosmosigner.Backend.AwsKMS.KeyID = arn + "-other"
					} else {
						v.Spec.Cosmosigner.Backend.AwsKMS.KeyID = arn + "-other"
					}
				}
				return Framework().Client().Update(ctx, live)
			}).Should(Succeed())
		}
	})

	It("rejects an alias, a bare key ID and an empty region in every placement", func() {
		for _, placement := range []string{"ChainNode", "ChainNodeSet", "group"} {
			for _, key := range []string{"alias/validator", "12345678-1234-1234-1234-123456789012", "arn:aws:kms:eu-west-1:123456789012:alias/validator"} {
				c := signer()
				c.Backend.AwsKMS.KeyID = key
				err := Framework().Client().Create(Framework().Context(), object(placement, c))
				Expect(err).To(HaveOccurred())
				Expect(err.Error()).To(ContainSubstring("keyId"))
			}
			c := signer()
			c.Backend.AwsKMS.Region = ""
			err := Framework().Client().Create(Framework().Context(), object(placement, c))
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("region"))
		}
	})

	It("rejects duplicate AWS signing keys and local genesis registration", func() {
		c := signer()
		obj := object("group", c).(*appsv1.ChainNodeSet)
		obj.Spec.Nodes = append(obj.Spec.Nodes, appsv1.NodeGroupSpec{Name: "other", Instances: ptr.To(1), Cosmosigner: signer()})
		err := Framework().Client().Create(Framework().Context(), obj)
		Expect(err).To(HaveOccurred())
		Expect(err.Error()).To(ContainSubstring("same AWS KMS signing key"))
		for _, placement := range []string{"ChainNode", "ChainNodeSet", "group"} {
			obj := object(placement, signer())
			switch v := obj.(type) {
			case *appsv1.ChainNode:
				v.Spec.Genesis = nil
				v.Spec.Validator = &appsv1.ValidatorConfig{Init: &appsv1.GenesisInitConfig{ChainID: "test-1", Assets: []string{"1000000utest"}, StakeAmount: "100000utest"}}
			case *appsv1.ChainNodeSet:
				v.Spec.Genesis = nil
				validator := &appsv1.NodeSetValidatorConfig{Init: &appsv1.GenesisInitConfig{ChainID: "test-1", Assets: []string{"1000000utest"}, StakeAmount: "100000utest"}}
				if placement == "group" {
					v.Spec.Nodes[0].Validator = validator
				} else {
					v.Spec.Cosmosigner.NodeGroups = nil
					v.Spec.Validator = validator
				}
			}
			err := Framework().Client().Create(Framework().Context(), obj)
			Expect(err).To(HaveOccurred())
			Expect(err.Error()).To(ContainSubstring("requires the software backend, vault.uploadGenerated or gcpKms.import"))
		}
	})
})
