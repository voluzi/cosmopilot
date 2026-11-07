package controllers

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

var gkeLoadBalancerFinalizers = []string{"gke.networking.io/l4-netlb-v1", "service.kubernetes.io/load-balancer-cleanup"}

var gkeLoadBalancerAnnotations = map[string]string{
	"service.kubernetes.io/backend-service":      "k8s2-node-p2p",
	"service.kubernetes.io/firewall-rule":        "k8s2-node-p2p",
	"service.kubernetes.io/firewall-rule-for-hc": "k8s2-node-p2p-fw",
	"service.kubernetes.io/healthcheck":          "k8s2-node-p2p",
	"service.kubernetes.io/tcp-forwarding-rule":  "a808124abf8ce406ca51ab3d4e7d0b7d",
}

var serviceKey = client.ObjectKey{Name: "node-p2p", Namespace: "default"}

func desiredLoadBalancerService(t *testing.T, annotations map[string]string, ports ...int32) *corev1.Service {
	t.Helper()
	owner, _ := ownershipOwners()
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: serviceKey.Name, Namespace: serviceKey.Namespace, Labels: map[string]string{"app": "node"}, Annotations: annotations},
		Spec: corev1.ServiceSpec{
			Type:                     corev1.ServiceTypeLoadBalancer,
			PublishNotReadyAddresses: true,
			Selector:                 map[string]string{"app": "node"},
		},
	}
	for _, port := range ports {
		svc.Spec.Ports = append(svc.Spec.Ports, corev1.ServicePort{Name: fmt.Sprintf("port-%d", port), Protocol: corev1.ProtocolTCP, Port: port, TargetPort: intstr.FromInt32(port)})
	}
	require.NoError(t, controllerutil.SetControllerReference(owner, svc, ownershipScheme(t)))
	return svc
}

// provisionLikeGKE stamps the API server's LoadBalancer defaults and what GKE's backend service-based
// load balancer controller adds to the live Service, and returns its resourceVersion.
func provisionLikeGKE(t *testing.T, c client.Client) string {
	t.Helper()
	live := getService(t, c)
	live.Spec.ClusterIP = "10.0.0.10"
	live.Spec.ClusterIPs = []string{"10.0.0.10"}
	live.Spec.IPFamilies = []corev1.IPFamily{corev1.IPv4Protocol}
	live.Spec.IPFamilyPolicy = ptr.To(corev1.IPFamilyPolicySingleStack)
	live.Spec.SessionAffinity = corev1.ServiceAffinityNone
	live.Spec.ExternalTrafficPolicy = corev1.ServiceExternalTrafficPolicyCluster
	live.Spec.InternalTrafficPolicy = ptr.To(corev1.ServiceInternalTrafficPolicyCluster)
	live.Spec.AllocateLoadBalancerNodePorts = ptr.To(true)
	for i := range live.Spec.Ports {
		live.Spec.Ports[i].NodePort = 30000 + int32(i)
	}
	live.Finalizers = gkeLoadBalancerFinalizers
	if live.Annotations == nil {
		live.Annotations = map[string]string{}
	}
	for k, v := range gkeLoadBalancerAnnotations {
		live.Annotations[k] = v
	}
	require.NoError(t, c.Update(context.Background(), live))
	return live.ResourceVersion
}

func getService(t *testing.T, c client.Client) *corev1.Service {
	t.Helper()
	live := &corev1.Service{}
	require.NoError(t, c.Get(context.Background(), serviceKey, live))
	return live
}

func requireGKEFieldsKept(t *testing.T, live *corev1.Service) {
	t.Helper()
	require.Equal(t, gkeLoadBalancerFinalizers, live.Finalizers)
	for k, v := range gkeLoadBalancerAnnotations {
		require.Equal(t, v, live.Annotations[k], k)
	}
	require.Equal(t, int32(30000), live.Spec.Ports[0].NodePort)
}

func TestEnsureServiceLeavesMatchingServiceAlone(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(ownershipScheme(t)).Build()
	require.NoError(t, EnsureService(ctx, c, desiredLoadBalancerService(t, nil, 26656)))
	provisioned := provisionLikeGKE(t, c)

	require.NoError(t, EnsureService(ctx, c, desiredLoadBalancerService(t, nil, 26656)))
	require.Equal(t, provisioned, getService(t, c).ResourceVersion, "a Service matching the desired spec must not be updated")
}

func TestEnsureServiceRecordsServiceCreatedWithoutRecords(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(ownershipScheme(t)).Build()
	previous := desiredLoadBalancerService(t, map[string]string{
		AnnotationStateSyncTrustHeight:        "100",
		AnnotationStateSyncTrustHash:          "ABC",
		"cosmopilot.voluzi.com/operator-note": "hand-added",
	}, 26656)
	previous.Labels["monitoring"] = "enabled"
	require.NoError(t, c.Create(ctx, previous))
	provisionLikeGKE(t, c)

	require.NoError(t, EnsureService(ctx, c, desiredLoadBalancerService(t, nil, 26656)))
	live := getService(t, c)
	require.NotContains(t, live.Annotations, AnnotationStateSyncTrustHeight)
	require.NotContains(t, live.Annotations, AnnotationStateSyncTrustHash)
	require.Equal(t, "hand-added", live.Annotations["cosmopilot.voluzi.com/operator-note"])
	require.Equal(t, "enabled", live.Labels["monitoring"], "without a record a label cannot be told apart from a foreign one")
	requireGKEFieldsKept(t, live)

	require.NoError(t, EnsureService(ctx, c, desiredLoadBalancerService(t, nil, 26656)))
	require.Equal(t, live.ResourceVersion, getService(t, c).ResourceVersion)
}

func TestEnsureServiceRemovesDroppedConfiguredAnnotation(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(ownershipScheme(t)).Build()
	configured := map[string]string{"example.com/dropped": "x", AnnotationStateSyncTrustHeight: "100", "example.com/kept": "y"}
	require.NoError(t, EnsureService(ctx, c, desiredLoadBalancerService(t, configured, 26656)))
	provisionLikeGKE(t, c)
	live := getService(t, c)
	live.Annotations["cosmopilot.voluzi.com/operator-note"] = "hand-added"
	require.NoError(t, c.Update(ctx, live))

	require.NoError(t, EnsureService(ctx, c, desiredLoadBalancerService(t, map[string]string{"example.com/kept": "y"}, 26656)))
	live = getService(t, c)
	require.NotContains(t, live.Annotations, "example.com/dropped")
	require.NotContains(t, live.Annotations, AnnotationStateSyncTrustHeight)
	require.Equal(t, "y", live.Annotations["example.com/kept"])
	require.Equal(t, "hand-added", live.Annotations["cosmopilot.voluzi.com/operator-note"])
	requireGKEFieldsKept(t, live)

	require.NoError(t, EnsureService(ctx, c, desiredLoadBalancerService(t, map[string]string{"example.com/kept": "y"}, 26656)))
	require.Equal(t, live.ResourceVersion, getService(t, c).ResourceVersion)
}

func TestEnsureServiceRemovesDroppedOwnerLabel(t *testing.T) {
	ctx := context.Background()
	c := fake.NewClientBuilder().WithScheme(ownershipScheme(t)).Build()
	labelled := desiredLoadBalancerService(t, nil, 26656)
	labelled.Labels["monitoring"] = "enabled"
	require.NoError(t, EnsureService(ctx, c, labelled))
	provisionLikeGKE(t, c)
	live := getService(t, c)
	live.Labels["example.com/foreign"] = "x"
	require.NoError(t, c.Update(ctx, live))

	require.NoError(t, EnsureService(ctx, c, desiredLoadBalancerService(t, nil, 26656)))
	live = getService(t, c)
	require.NotContains(t, live.Labels, "monitoring")
	require.Equal(t, "x", live.Labels["example.com/foreign"])
	require.Equal(t, "node", live.Labels["app"])
	requireGKEFieldsKept(t, live)

	require.NoError(t, EnsureService(ctx, c, desiredLoadBalancerService(t, nil, 26656)))
	require.Equal(t, live.ResourceVersion, getService(t, c).ResourceVersion)
}
