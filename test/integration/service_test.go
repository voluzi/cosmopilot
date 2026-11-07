package integration

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/voluzi/cosmopilot/v2/internal/controllers"
)

var _ = Describe("EnsureService", func() {
	var (
		ns    *corev1.Namespace
		owner *corev1.ConfigMap
	)

	BeforeEach(func() {
		ns = CreateTestNamespace()
		owner = &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: ns.Name}}
		Expect(Framework().Client().Create(Framework().Context(), owner)).To(Succeed())
	})

	desired := func(serviceType corev1.ServiceType, ports ...int32) *corev1.Service {
		svc := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: "node-p2p", Namespace: ns.Name, Labels: map[string]string{"app": "node"}},
			Spec: corev1.ServiceSpec{
				Type:                     serviceType,
				PublishNotReadyAddresses: true,
				Selector:                 map[string]string{"app": "node"},
			},
		}
		for _, port := range ports {
			svc.Spec.Ports = append(svc.Spec.Ports, corev1.ServicePort{Name: fmt.Sprintf("port-%d", port), Protocol: corev1.ProtocolTCP, Port: port, TargetPort: intstr.FromInt32(port)})
		}
		Expect(controllerutil.SetControllerReference(owner, svc, Framework().Client().Scheme())).To(Succeed())
		return svc
	}

	live := func() *corev1.Service {
		svc := &corev1.Service{}
		Expect(Framework().Client().Get(Framework().Context(), client.ObjectKey{Name: "node-p2p", Namespace: ns.Name}, svc)).To(Succeed())
		return svc
	}

	ensureStable := func(svc *corev1.Service) *corev1.Service {
		Expect(controllers.EnsureService(Framework().Context(), Framework().Client(), svc.DeepCopy())).To(Succeed())
		applied := live()
		Expect(controllers.EnsureService(Framework().Context(), Framework().Client(), svc.DeepCopy())).To(Succeed())
		Expect(live().ResourceVersion).To(Equal(applied.ResourceVersion), "a second reconcile must not update the Service")
		return applied
	}

	It("keeps fields set by others when a managed field changes", func() {
		svc := desired(corev1.ServiceTypeLoadBalancer, 26656)
		svc.Labels["example.com/foreign"] = "x"
		svc.Spec.LoadBalancerClass = ptr.To("service.k8s.aws/nlb")
		other := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: ns.Name}}
		Expect(Framework().Client().Create(Framework().Context(), other)).To(Succeed())
		Expect(controllerutil.SetOwnerReference(other, svc, Framework().Client().Scheme())).To(Succeed())
		Expect(Framework().Client().Create(Framework().Context(), svc)).To(Succeed())

		applied := ensureStable(desired(corev1.ServiceTypeLoadBalancer, 26656, 26657))
		Expect(applied.Spec.Ports).To(HaveLen(2))
		Expect(applied.Labels).To(HaveKeyWithValue("example.com/foreign", "x"))
		Expect(applied.Spec.LoadBalancerClass).To(Equal(ptr.To("service.k8s.aws/nlb")))
		Expect(applied.OwnerReferences).To(ContainElement(HaveField("UID", other.UID)))
	})

	It("follows Service type changes", func() {
		svc := desired(corev1.ServiceTypeLoadBalancer, 26656)
		svc.Spec.LoadBalancerSourceRanges = []string{"10.0.0.0/8"}
		svc.Spec.LoadBalancerIP = "192.0.2.10"
		Expect(Framework().Client().Create(Framework().Context(), svc)).To(Succeed())

		nodePort := ensureStable(desired(corev1.ServiceTypeNodePort, 26656))
		Expect(nodePort.Spec.Type).To(Equal(corev1.ServiceTypeNodePort))
		Expect(nodePort.Spec.Ports[0].NodePort).NotTo(BeZero())

		loadBalancer := ensureStable(desired(corev1.ServiceTypeLoadBalancer, 26656))
		Expect(loadBalancer.Spec.Type).To(Equal(corev1.ServiceTypeLoadBalancer))
		Expect(loadBalancer.Spec.AllocateLoadBalancerNodePorts).To(Equal(ptr.To(true)))

		clusterIP := ensureStable(desired(corev1.ServiceTypeClusterIP, 26656))
		Expect(clusterIP.Spec.Type).To(Equal(corev1.ServiceTypeClusterIP))
		Expect(clusterIP.Spec.Ports[0].NodePort).To(BeZero())
	})
})
