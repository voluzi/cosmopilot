package integration

import (
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	appsv1 "github.com/voluzi/cosmopilot/v5/api/v1"
	"github.com/voluzi/cosmopilot/v5/internal/cometbft"
	"github.com/voluzi/cosmopilot/v5/internal/controllers"
)

var _ = Describe("P2P TCPRoute v1", func() {
	BeforeEach(func() {
		mapper := Framework().Client().RESTMapper()
		kind := gwapiv1.SchemeGroupVersion.WithKind("TCPRoute").GroupKind()
		_, err := mapper.RESTMapping(kind, "v1")
		Expect(err).NotTo(HaveOccurred())
		_, err = mapper.RESTMapping(kind, "v1alpha2")
		Expect(meta.IsNoMatchError(err)).To(BeTrue(), "expected the v1alpha2 endpoint to be unserved, got %v", err)
	})

	It("reconciles ChainNode routes and preserves a same-name unowned route", WithNamespace(func(ns *corev1.Namespace) {
		ctx, c := Framework().Context(), Framework().Client()
		key, err := cometbft.GeneratePrivKey()
		Expect(err).NotTo(HaveOccurred())
		Expect(c.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "signing-key", Namespace: ns.Name},
			Data:       map[string][]byte{"priv_validator_key.json": key},
		})).To(Succeed())
		Expect(c.Create(ctx, &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: "node", Namespace: ns.Name, Annotations: map[string]string{
				controllers.AnnotationDataInitialized:   "true",
				controllers.AnnotationGenesisDownloaded: "true",
				controllers.AnnotationGenesisChainID:    "tcp-test",
				controllers.AnnotationDataHeight:        "0",
			}},
			Spec: corev1.PersistentVolumeClaimSpec{
				AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
				},
			},
		})).To(Succeed())

		node := &appsv1.ChainNode{
			ObjectMeta: metav1.ObjectMeta{Name: "node", Namespace: ns.Name},
			Spec: appsv1.ChainNodeSpec{
				App:     DefaultChainNodeTestApp,
				Genesis: NewGenesisConfigWithChainID("tcp-test"),
				Expose:  tcpGatewayExposure(30056, ptr.To("node-p2p")),
				Cosmosigner: &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{
					Software: &appsv1.CosmosignerSoftwareBackend{PrivateKeySecret: ptr.To("signing-key")},
				}},
			},
		}
		Expect(c.Create(ctx, node)).To(Succeed())
		routeKey := client.ObjectKey{Namespace: ns.Name, Name: "node-p2p"}
		want := tcpRouteSpec(30056, ptr.To("node-p2p"), "node-internal")
		initial := waitForTCPRoute(routeKey, node, want)

		patchExposure := func(change func(*appsv1.ExposeConfig)) {
			Expect(c.Get(ctx, client.ObjectKeyFromObject(node), node)).To(Succeed())
			base := node.DeepCopy()
			change(node.Spec.Expose)
			Expect(c.Patch(ctx, node, client.MergeFrom(base))).To(Succeed())
		}
		patchExposure(func(expose *appsv1.ExposeConfig) {
			expose.Gateway.Port = ptr.To[int32](31056)
			expose.Gateway.SectionName = ptr.To("new-p2p")
		})
		Expect(waitForTCPRoute(routeKey, node, tcpRouteSpec(31056, ptr.To("new-p2p"), "node-internal")).UID).To(Equal(initial.UID))

		patchExposure(func(expose *appsv1.ExposeConfig) { expose.P2P = ptr.To(false) })
		waitForTCPRouteDeletion(routeKey)

		foreign := &gwapiv1.TCPRoute{
			ObjectMeta: metav1.ObjectMeta{Name: routeKey.Name, Namespace: routeKey.Namespace},
			Spec:       want,
		}
		Expect(c.Create(ctx, foreign)).To(Succeed())
		Eventually(func() error {
			Expect(c.Get(ctx, client.ObjectKeyFromObject(node), node)).To(Succeed())
			node.Status.PublicAddress = "stale@192.0.2.1:30056"
			return c.Status().Update(ctx, node)
		}).Should(Succeed())
		patchExposure(func(expose *appsv1.ExposeConfig) { expose.Gateway.Port = ptr.To[int32](32056) })
		Eventually(func(g Gomega) {
			current := &appsv1.ChainNode{}
			g.Expect(c.Get(ctx, client.ObjectKeyFromObject(node), current)).To(Succeed())
			g.Expect(current.Status.PublicAddress).To(BeEmpty())
		}).Should(Succeed())
		expectTCPRouteUnchanged(foreign)
	}))

	It("reconciles Cosmoseed routes and cleans up only routes it owns", WithNamespace(func(ns *corev1.Namespace) {
		ctx, c := Framework().Context(), Framework().Client()
		set := &appsv1.ChainNodeSet{
			ObjectMeta: metav1.ObjectMeta{Name: "network", Namespace: ns.Name},
			Spec: appsv1.ChainNodeSetSpec{
				App:     DefaultChainNodeSetTestApp,
				Genesis: NewGenesisConfigWithChainID("tcp-test"),
				Nodes:   []appsv1.NodeGroupSpec{{Name: "fullnodes", Instances: ptr.To(1)}},
				Cosmoseed: &appsv1.CosmoseedConfig{
					Enabled:   ptr.To(true),
					Instances: ptr.To(2),
					Expose:    tcpGatewayExposure(30056, nil),
				},
			},
		}
		Expect(c.Create(ctx, set)).To(Succeed())

		routeKey := func(index int) client.ObjectKey {
			return client.ObjectKey{Namespace: ns.Name, Name: fmt.Sprintf("network-seed-%d-p2p", index)}
		}
		waitForSeedRoute := func(index, basePort int) *gwapiv1.TCPRoute {
			return waitForTCPRoute(routeKey(index), set, tcpRouteSpec(
				gwapiv1.PortNumber(basePort+index), nil, gwapiv1.ObjectName(fmt.Sprintf("network-seed-%d-internal", index)),
			))
		}
		first, second := waitForSeedRoute(0, 30056), waitForSeedRoute(1, 30056)
		foreign := &gwapiv1.TCPRoute{
			ObjectMeta: metav1.ObjectMeta{Name: "user-p2p", Namespace: ns.Name, Labels: map[string]string{
				controllers.LabelApp:          controllers.CosmoseedName,
				controllers.LabelChainNodeSet: set.Name,
			}},
			Spec: tcpRouteSpec(40056, nil, "user-backend"),
		}
		Expect(c.Create(ctx, foreign)).To(Succeed())

		patchSeed := func(change func(*appsv1.CosmoseedConfig)) {
			Expect(c.Get(ctx, client.ObjectKeyFromObject(set), set)).To(Succeed())
			base := set.DeepCopy()
			change(set.Spec.Cosmoseed)
			Expect(c.Patch(ctx, set, client.MergeFrom(base))).To(Succeed())
		}
		waitForSeedCount := func(count int) {
			Eventually(func(g Gomega) {
				current := &appsv1.ChainNodeSet{}
				g.Expect(c.Get(ctx, client.ObjectKeyFromObject(set), current)).To(Succeed())
				g.Expect(current.Status.Seeds).To(HaveLen(count))
			}).Should(Succeed())
		}

		patchSeed(func(seed *appsv1.CosmoseedConfig) { seed.Expose.Gateway.Port = ptr.To[int32](31056) })
		Expect(waitForSeedRoute(0, 31056).UID).To(Equal(first.UID))
		Expect(waitForSeedRoute(1, 31056).UID).To(Equal(second.UID))
		waitForSeedCount(2)

		patchSeed(func(seed *appsv1.CosmoseedConfig) { seed.Instances = ptr.To(1) })
		waitForSeedCount(1)
		waitForTCPRouteDeletion(routeKey(1))
		Expect(waitForSeedRoute(0, 31056).UID).To(Equal(first.UID))
		expectTCPRouteUnchanged(foreign)

		patchSeed(func(seed *appsv1.CosmoseedConfig) { seed.Enabled = ptr.To(false) })
		waitForSeedCount(0)
		waitForTCPRouteDeletion(routeKey(0))
		expectTCPRouteUnchanged(foreign)
	}))
})

func tcpGatewayExposure(port int32, section *string) *appsv1.ExposeConfig {
	return &appsv1.ExposeConfig{
		P2P: ptr.To(true),
		Gateway: &appsv1.ExposeGatewayConfig{
			GatewayRef: appsv1.GatewayRef{Name: "edge", Namespace: ptr.To("gateways"), SectionName: section},
			Port:       ptr.To(port),
		},
	}
}

func tcpRouteSpec(port gwapiv1.PortNumber, section *string, backend gwapiv1.ObjectName) gwapiv1.TCPRouteSpec {
	spec := gwapiv1.TCPRouteSpec{
		CommonRouteSpec: gwapiv1.CommonRouteSpec{ParentRefs: []gwapiv1.ParentReference{{
			Group: ptr.To(gwapiv1.Group("gateway.networking.k8s.io")), Kind: ptr.To(gwapiv1.Kind("Gateway")),
			Name: "edge", Namespace: ptr.To(gwapiv1.Namespace("gateways")), Port: ptr.To(port),
		}}},
		Rules: []gwapiv1.TCPRouteRule{{BackendRefs: []gwapiv1.BackendRef{{
			BackendObjectReference: gwapiv1.BackendObjectReference{
				Name: backend, Group: ptr.To(gwapiv1.Group("")), Kind: ptr.To(gwapiv1.Kind("Service")),
				Port: ptr.To(gwapiv1.PortNumber(26656)),
			},
			Weight: ptr.To[int32](1),
		}}}},
	}
	if section != nil {
		spec.ParentRefs[0].SectionName = ptr.To(gwapiv1.SectionName(*section))
	}
	return spec
}

func waitForTCPRoute(key client.ObjectKey, owner client.Object, spec gwapiv1.TCPRouteSpec) *gwapiv1.TCPRoute {
	GinkgoHelper()
	ctx, c := Framework().Context(), Framework().Client()
	route := &gwapiv1.TCPRoute{}
	Eventually(func(g Gomega) {
		g.Expect(c.Get(ctx, key, route)).To(Succeed())
		g.Expect(metav1.IsControlledBy(route, owner)).To(BeTrue(), "%s should be controlled by %s", key, owner.GetName())
		g.Expect(route.Spec).To(Equal(spec))
	}).Should(Succeed())
	return route
}

func waitForTCPRouteDeletion(key client.ObjectKey) {
	GinkgoHelper()
	ctx, c := Framework().Context(), Framework().Client()
	Eventually(func() bool {
		return apierrors.IsNotFound(c.Get(ctx, key, &gwapiv1.TCPRoute{}))
	}).Should(BeTrue(), "expected TCPRoute %s to be deleted", key)
}

func expectTCPRouteUnchanged(original *gwapiv1.TCPRoute) {
	GinkgoHelper()
	ctx, c := Framework().Context(), Framework().Client()
	current := &gwapiv1.TCPRoute{}
	Expect(c.Get(ctx, client.ObjectKeyFromObject(original), current)).To(Succeed())
	Expect(current.UID).To(Equal(original.UID))
	Expect(current.ResourceVersion).To(Equal(original.ResourceVersion))
}
