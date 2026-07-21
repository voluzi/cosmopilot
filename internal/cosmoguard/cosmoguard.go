// Package cosmoguard renders the Kubernetes resources for a clustered CosmoGuard deployment
// (StatefulSet, client Service, headless peer Service, HorizontalPodAutoscaler and an optional
// dashboard Ingress) that fronts one or more blockchain nodes.
//
// CosmoGuard runs as a StatefulSet so its embedded olric cache cluster gets stable per-pod DNS:
// every replica shares one distributed cache, so a request cached by one pod is served from cache
// by all of them and the backing nodes are shielded regardless of how the load balancer spreads
// traffic. Peers find each other through the headless peer Service (olric DNS discovery), and the
// gossip channel is encrypted with a key the operator provisions as a Secret.
//
// CosmoGuard v4 is configured through a user-owned ConfigMap that carries ONLY policy rules.
// Everything the operator controls (upstream node discovery, listener/metrics/dashboard settings
// and the whole cache.cluster block) is injected through COSMOGUARD_* environment variables, so the
// user ConfigMap is never rewritten and CosmoGuard's rules hot-reload keeps working. See
// internal/controllers for the wiring.
package cosmoguard

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"

	"github.com/voluzi/cosmopilot/v2/internal/chainutils"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
	"github.com/voluzi/cosmopilot/v2/internal/k8s"
	"github.com/voluzi/cosmopilot/v2/pkg/utils"
)

func intToStr(i int) string { return strconv.Itoa(i) }

const (
	// labelAppName / appName / labelInstance identify CosmoGuard pods and back the StatefulSet
	// selector. They are kept separate from caller-provided routing labels so the selector stays
	// stable regardless of what the owner stamps on the resources.
	labelAppName  = "app.kubernetes.io/name"
	labelInstance = "app.kubernetes.io/instance"
	appName       = "cosmoguard"

	// containerName is the name of the CosmoGuard container.
	containerName = "cosmoguard"

	// configVolumeName / configMountPath mount the user rules ConfigMap. The whole ConfigMap is
	// mounted as a volume (not via subPath) so kubelet's atomic symlink swap triggers CosmoGuard's
	// fsnotify-based rules hot-reload on edits.
	configVolumeName = "config"
	configMountPath  = "/config"

	// olric cluster ports (match CosmoGuard's ClusterConfig defaults). The gossip port carries both
	// TCP and UDP. peerApiPort is bindPort+1 (CosmoGuard's default when left 0), declared explicitly
	// here so the peer Service can expose it.
	clusterBindPort    = 3320
	clusterGossipPort  = 3322
	clusterPeerApiPort = 3321

	clusterBindPortName    = "cluster-resp"
	clusterGossipPortName  = "cluster-gossip"
	clusterPeerApiPortName = "cluster-peer"
	// Kubernetes port names are IANA service names (max 15 chars); "cluster-gossip-udp" (18) is
	// rejected, so the UDP gossip port gets its own short name.
	clusterGossipUDPPortName = "gossip-udp"

	// EncryptionKeySecretKey is the key under which the olric gossip encryption key is stored in the
	// operator-managed Secret.
	EncryptionKeySecretKey = "encryptionKey"
)

// GenerateEncryptionKey returns a fresh base64-encoded 32-byte key for olric gossip encryption.
func GenerateEncryptionKey() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating cosmoguard encryption key: %w", err)
	}
	return base64.StdEncoding.EncodeToString(buf), nil
}

// Params carries everything needed to render a CosmoGuard deployment's resources. The owner
// (ChainNode or ChainNodeSet controller) sets owner references and applies the returned objects.
type Params struct {
	// Name is the name of the CosmoGuard Deployment/Service (e.g. "<node>-cosmoguard").
	Name      string
	Namespace string

	Image    string
	Replicas int32

	// UpstreamHost is the DNS name of a single upstream node Service (static upstream). Used for a
	// standalone ChainNode. Exactly one of UpstreamHost / DiscoveryHost must be set.
	UpstreamHost string

	// DiscoveryHost is the DNS name of a headless Service whose endpoints are the node pods to front.
	// When set, CosmoGuard uses DNS discovery so each node pod becomes a first-class upstream. Used
	// for a node group.
	DiscoveryHost string

	EvmEnabled bool

	ConfigMap *corev1.ConfigMapKeySelector
	Resources corev1.ResourceRequirements

	// Labels are extra routing labels (nodeset/group/global-route) merged onto the guard pods so
	// group/global/gateway Services can select them. They must never include node selector labels
	// (chain-id/node-id/validator/peer/seed) — the caller is responsible for excluding those.
	Labels map[string]string

	Dashboard   *DashboardParams
	Autoscaling *AutoscalingParams

	// PeerServiceName is the headless Service that backs the StatefulSet and the olric cluster's DNS
	// discovery (every replica resolves its peers through it).
	PeerServiceName string

	// EncryptionKeySecret is the name of the operator-managed Secret holding the olric gossip
	// encryption key (under EncryptionKeySecretKey).
	EncryptionKeySecret string

	PriorityClassName string
	ImagePullSecrets  []corev1.LocalObjectReference

	// NodeSelector / Affinity place the guard alongside the nodes it fronts, so it can schedule in a
	// dedicated/tainted node pool instead of staying Pending.
	NodeSelector map[string]string
	Affinity     *corev1.Affinity
}

// peerDiscoveryHost is the in-cluster DNS name olric resolves to find peers.
func (p Params) peerDiscoveryHost() string {
	return fmt.Sprintf("%s.%s.svc.cluster.local", p.PeerServiceName, p.Namespace)
}

// DashboardParams configures CosmoGuard's read-only dashboard listener.
type DashboardParams struct {
	Port         int32
	AuthUser     *corev1.SecretKeySelector
	AuthPassword *corev1.SecretKeySelector
	Ingress      *DashboardIngressParams
}

// DashboardIngressParams configures an Ingress exposing the dashboard.
type DashboardIngressParams struct {
	Host             string
	IngressClassName *string
	Annotations      map[string]string
	TLSSecretName    *string
}

// AutoscalingParams configures the HorizontalPodAutoscaler for a CosmoGuard Deployment.
type AutoscalingParams struct {
	MinReplicas  *int32
	MaxReplicas  int32
	TargetCPU    *int32
	TargetMemory *int32
}

// InstanceLabels returns the immutable selector labels identifying a CosmoGuard instance's pods.
// A group Service uses these to target exactly one guard's pods.
func InstanceLabels(name string) map[string]string {
	return map[string]string{
		labelAppName:  appName,
		labelInstance: name,
	}
}

// AppLabel returns the label shared by every CosmoGuard pod. A global ingress/gateway Service uses
// it (plus a per-route label) to target the guard pods of several groups at once.
func AppLabel() map[string]string {
	return map[string]string{labelAppName: appName}
}

// SelectsGuard reports whether a Service selector targets CosmoGuard pods (i.e. the Service has
// already been flipped to the guard). Used to keep a guarded Service on the guard through transient
// guard rollouts rather than reverting it to raw node pods.
func SelectsGuard(selector map[string]string) bool {
	return selector[labelAppName] == appName
}

// PeerServiceName derives the headless peer Service name from the guard name.
func PeerServiceName(name string) string { return name + "-peer" }

// EncryptionKeySecretName derives the olric encryption-key Secret name from the guard name.
func EncryptionKeySecretName(name string) string { return name + "-cluster" }

func (p Params) podLabels() map[string]string {
	return utils.MergeMaps(p.Labels, InstanceLabels(p.Name))
}

// env builds the operator-managed COSMOGUARD_* environment injected into the guard container.
func (p Params) env() []corev1.EnvVar {
	env := []corev1.EnvVar{
		{Name: "COSMOGUARD_METRICS_ENABLE", Value: "true"},
		{Name: "COSMOGUARD_METRICS_PORT", Value: intToStr(controllers.CosmoGuardMetricsPort)},

		// olric cache cluster: every replica binds its own pod IP as the member name and discovers
		// peers through the headless peer Service. The gossip encryption key comes from a Secret.
		{Name: "COSMOGUARD_CLUSTER_ENABLE", Value: "true"},
		{Name: "COSMOGUARD_CLUSTER_BIND_ADDR", ValueFrom: &corev1.EnvVarSource{
			FieldRef: &corev1.ObjectFieldSelector{FieldPath: "status.podIP"},
		}},
		{Name: "COSMOGUARD_CLUSTER_DISCOVERY_MODE", Value: "dns"},
		{Name: "COSMOGUARD_CLUSTER_DISCOVERY_DNS_HOST", Value: p.peerDiscoveryHost()},
		{Name: "COSMOGUARD_CLUSTER_ENCRYPTION_KEY", ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: p.EncryptionKeySecret},
				Key:                  EncryptionKeySecretKey,
			},
		}},
	}

	// Upstream: DNS discovery against a headless Service (group) or a static host (single node).
	// CosmoGuard's NodeConfig port defaults already match the node's raw ports
	// (rpc 26657 / lcd 1317 / grpc 9090 / evm 8545 / evm-ws 8546), so only host/discovery is set.
	if p.DiscoveryHost != "" {
		env = append(env,
			corev1.EnvVar{Name: "COSMOGUARD_DISCOVERY_HOST", Value: p.DiscoveryHost},
			corev1.EnvVar{Name: "COSMOGUARD_DISCOVERY_TYPE", Value: "dns"},
		)
	} else {
		env = append(env, corev1.EnvVar{Name: "COSMOGUARD_NODE_HOST", Value: p.UpstreamHost})
	}

	if p.EvmEnabled {
		env = append(env, corev1.EnvVar{Name: "COSMOGUARD_ENABLE_EVM", Value: "true"})
	}

	if p.Dashboard == nil {
		// Keep the dashboard explicitly off unless it is configured, so the operator's intent doesn't
		// depend on CosmoGuard's default.
		env = append(env, corev1.EnvVar{Name: "COSMOGUARD_DASHBOARD_ENABLE", Value: "false"})
	} else {
		env = append(env,
			corev1.EnvVar{Name: "COSMOGUARD_DASHBOARD_ENABLE", Value: "true"},
			corev1.EnvVar{Name: "COSMOGUARD_DASHBOARD_PORT", Value: intToStr(int(p.Dashboard.Port))},
		)
		if p.Dashboard.AuthUser != nil {
			env = append(env, corev1.EnvVar{
				Name:      "COSMOGUARD_DASHBOARD_AUTH_USER",
				ValueFrom: &corev1.EnvVarSource{SecretKeyRef: p.Dashboard.AuthUser},
			})
		}
		if p.Dashboard.AuthPassword != nil {
			env = append(env, corev1.EnvVar{
				Name:      "COSMOGUARD_DASHBOARD_AUTH_PASSWORD",
				ValueFrom: &corev1.EnvVarSource{SecretKeyRef: p.Dashboard.AuthPassword},
			})
		}
	}

	return env
}

// containerPorts returns the guard's listener ports.
func (p Params) containerPorts() []corev1.ContainerPort {
	ports := []corev1.ContainerPort{
		{Name: controllers.CosmoGuardRpcPortName, ContainerPort: controllers.CosmoGuardRpcPort, Protocol: corev1.ProtocolTCP},
		{Name: controllers.CosmoGuardLcdPortName, ContainerPort: controllers.CosmoGuardLcdPort, Protocol: corev1.ProtocolTCP},
		{Name: controllers.CosmoGuardGrpcPortName, ContainerPort: controllers.CosmoGuardGrpcPort, Protocol: corev1.ProtocolTCP},
		{Name: controllers.CosmoGuardMetricsPortName, ContainerPort: controllers.CosmoGuardMetricsPort, Protocol: corev1.ProtocolTCP},
	}
	if p.EvmEnabled {
		ports = append(ports,
			corev1.ContainerPort{Name: controllers.CosmoGuardEvmRpcPortName, ContainerPort: controllers.CosmoGuardEvmRpcPort, Protocol: corev1.ProtocolTCP},
			corev1.ContainerPort{Name: controllers.CosmoGuardEvmRpcWsPortName, ContainerPort: controllers.CosmoGuardEvmRpcWsPort, Protocol: corev1.ProtocolTCP},
		)
	}
	if p.Dashboard != nil {
		ports = append(ports, corev1.ContainerPort{Name: "dashboard", ContainerPort: p.Dashboard.Port, Protocol: corev1.ProtocolTCP})
	}
	// olric cluster ports: RESP (TCP), gossip (TCP + UDP), peer API (TCP).
	ports = append(ports,
		corev1.ContainerPort{Name: clusterBindPortName, ContainerPort: clusterBindPort, Protocol: corev1.ProtocolTCP},
		corev1.ContainerPort{Name: clusterGossipPortName, ContainerPort: clusterGossipPort, Protocol: corev1.ProtocolTCP},
		corev1.ContainerPort{Name: clusterGossipUDPPortName, ContainerPort: clusterGossipPort, Protocol: corev1.ProtocolUDP},
		corev1.ContainerPort{Name: clusterPeerApiPortName, ContainerPort: clusterPeerApiPort, Protocol: corev1.ProtocolTCP},
	)
	return ports
}

// securityContext returns the restricted context hardened with a read-only root filesystem
// (CosmoGuard is entirely in-memory and needs no writable paths).
func securityContext() *corev1.SecurityContext {
	sc := k8s.RestrictedSecurityContext()
	sc.ReadOnlyRootFilesystem = ptr.To(true)
	return sc
}

// StatefulSet renders the clustered CosmoGuard StatefulSet.
func (p Params) StatefulSet() *appsv1.StatefulSet {
	metricsPort := intstr.FromInt32(controllers.CosmoGuardMetricsPort)

	container := corev1.Container{
		Name:            containerName,
		Image:           p.Image,
		ImagePullPolicy: corev1.PullIfNotPresent,
		SecurityContext: securityContext(),
		Args:            []string{"--config", configMountPath + "/" + p.ConfigMap.Key},
		Env:             p.env(),
		Ports:           p.containerPorts(),
		Resources:       p.Resources,
		VolumeMounts: []corev1.VolumeMount{
			{Name: configVolumeName, MountPath: configMountPath, ReadOnly: true},
		},
		StartupProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{Path: "/readyz", Port: metricsPort, Scheme: corev1.URISchemeHTTP},
			},
			PeriodSeconds:    2,
			FailureThreshold: 30,
		},
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{Path: "/readyz", Port: metricsPort, Scheme: corev1.URISchemeHTTP},
			},
			PeriodSeconds:    5,
			FailureThreshold: 3,
		},
		LivenessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: metricsPort, Scheme: corev1.URISchemeHTTP},
			},
			PeriodSeconds:    10,
			FailureThreshold: 3,
		},
	}

	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      p.Name,
			Namespace: p.Namespace,
			Labels:    p.podLabels(),
		},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: p.PeerServiceName,
			// Parallel so all replicas start together and can form the olric cluster quorum without
			// waiting for ordinal-by-ordinal readiness.
			PodManagementPolicy: appsv1.ParallelPodManagement,
			Selector:            &metav1.LabelSelector{MatchLabels: InstanceLabels(p.Name)},
			UpdateStrategy: appsv1.StatefulSetUpdateStrategy{
				Type: appsv1.RollingUpdateStatefulSetStrategyType,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: p.podLabels()},
				Spec: corev1.PodSpec{
					SecurityContext:   k8s.RestrictedPodSecurityContext(),
					PriorityClassName: p.PriorityClassName,
					ImagePullSecrets:  p.ImagePullSecrets,
					NodeSelector:      p.NodeSelector,
					Affinity:          p.Affinity,
					Containers:        []corev1.Container{container},
					Volumes: []corev1.Volume{
						{
							Name: configVolumeName,
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: p.ConfigMap.LocalObjectReference,
								},
							},
						},
					},
				},
			},
		},
	}

	// When autoscaling is enabled the HPA owns .spec.replicas; leave it unset so reconciles don't
	// fight the autoscaler.
	if p.Autoscaling == nil {
		sts.Spec.Replicas = ptr.To(p.Replicas)
	}

	return sts
}

// Service renders the ClusterIP Service that fronts the guard pods. It keeps the node's public
// port numbers (26657/1317/9090/8545/8546) and targets the guard's listener ports.
func (p Params) Service() *corev1.Service {
	ports := []corev1.ServicePort{
		{Name: chainutils.RpcPortName, Protocol: corev1.ProtocolTCP, Port: chainutils.RpcPort, TargetPort: intstr.FromInt32(controllers.CosmoGuardRpcPort)},
		{Name: chainutils.LcdPortName, Protocol: corev1.ProtocolTCP, Port: chainutils.LcdPort, TargetPort: intstr.FromInt32(controllers.CosmoGuardLcdPort)},
		{Name: chainutils.GrpcPortName, Protocol: corev1.ProtocolTCP, Port: chainutils.GrpcPort, TargetPort: intstr.FromInt32(controllers.CosmoGuardGrpcPort)},
		{Name: controllers.CosmoGuardMetricsPortName, Protocol: corev1.ProtocolTCP, Port: controllers.CosmoGuardMetricsPort, TargetPort: intstr.FromInt32(controllers.CosmoGuardMetricsPort)},
	}
	if p.EvmEnabled {
		ports = append(ports,
			corev1.ServicePort{Name: controllers.EvmRpcPortName, Protocol: corev1.ProtocolTCP, Port: controllers.EvmRpcPort, TargetPort: intstr.FromInt32(controllers.CosmoGuardEvmRpcPort)},
			corev1.ServicePort{Name: controllers.EvmRpcWsPortName, Protocol: corev1.ProtocolTCP, Port: controllers.EvmRpcWsPort, TargetPort: intstr.FromInt32(controllers.CosmoGuardEvmRpcWsPort)},
		)
	}
	if p.Dashboard != nil {
		ports = append(ports, corev1.ServicePort{Name: "dashboard", Protocol: corev1.ProtocolTCP, Port: p.Dashboard.Port, TargetPort: intstr.FromInt32(p.Dashboard.Port)})
	}

	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      p.Name,
			Namespace: p.Namespace,
			Labels:    p.podLabels(),
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: InstanceLabels(p.Name),
			Ports:    ports,
		},
	}
}

// PeerService renders the headless Service that backs the StatefulSet and the olric cluster's DNS
// discovery. It publishes not-ready addresses so peers can find each other during the initial
// cluster join, before any pod passes its readiness probe.
func (p Params) PeerService() *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      p.PeerServiceName,
			Namespace: p.Namespace,
			Labels:    p.podLabels(),
		},
		Spec: corev1.ServiceSpec{
			ClusterIP:                corev1.ClusterIPNone,
			PublishNotReadyAddresses: true,
			Selector:                 InstanceLabels(p.Name),
			Ports: []corev1.ServicePort{
				{Name: clusterBindPortName, Protocol: corev1.ProtocolTCP, Port: clusterBindPort, TargetPort: intstr.FromInt32(clusterBindPort)},
				{Name: clusterGossipPortName, Protocol: corev1.ProtocolTCP, Port: clusterGossipPort, TargetPort: intstr.FromInt32(clusterGossipPort)},
				{Name: clusterGossipUDPPortName, Protocol: corev1.ProtocolUDP, Port: clusterGossipPort, TargetPort: intstr.FromInt32(clusterGossipPort)},
				{Name: clusterPeerApiPortName, Protocol: corev1.ProtocolTCP, Port: clusterPeerApiPort, TargetPort: intstr.FromInt32(clusterPeerApiPort)},
			},
		},
	}
}

// HPA renders the HorizontalPodAutoscaler for the guard StatefulSet, or nil when autoscaling is off.
func (p Params) HPA() *autoscalingv2.HorizontalPodAutoscaler {
	if p.Autoscaling == nil {
		return nil
	}

	metrics := make([]autoscalingv2.MetricSpec, 0, 2)
	if p.Autoscaling.TargetCPU != nil {
		metrics = append(metrics, resourceUtilizationMetric(corev1.ResourceCPU, p.Autoscaling.TargetCPU))
	}
	if p.Autoscaling.TargetMemory != nil {
		metrics = append(metrics, resourceUtilizationMetric(corev1.ResourceMemory, p.Autoscaling.TargetMemory))
	}

	return &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{
			Name:      p.Name,
			Namespace: p.Namespace,
			Labels:    p.podLabels(),
		},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: "apps/v1",
				Kind:       "StatefulSet",
				Name:       p.Name,
			},
			MinReplicas: p.Autoscaling.MinReplicas,
			MaxReplicas: p.Autoscaling.MaxReplicas,
			Metrics:     metrics,
		},
	}
}

// PDB renders a PodDisruptionBudget protecting the guard from mass eviction (e.g. a node drain taking
// down every replica while the protected nodes stay up). Returns nil for a single fixed-replica guard,
// where a PDB would only risk blocking drains without providing availability.
func (p Params) PDB() *policyv1.PodDisruptionBudget {
	if p.Autoscaling == nil && p.Replicas <= 1 {
		return nil
	}
	maxUnavailable := intstr.FromInt32(1)
	return &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      p.Name,
			Namespace: p.Namespace,
			Labels:    p.podLabels(),
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MaxUnavailable: &maxUnavailable,
			Selector:       &metav1.LabelSelector{MatchLabels: InstanceLabels(p.Name)},
		},
	}
}

// DashboardIngressName returns the name of the dashboard Ingress for this guard.
func (p Params) DashboardIngressName() string {
	return p.Name + "-dashboard"
}

// DashboardIngress renders an Ingress exposing the guard dashboard, or nil when not configured.
func (p Params) DashboardIngress() *networkingv1.Ingress {
	if p.Dashboard == nil || p.Dashboard.Ingress == nil {
		return nil
	}
	ing := p.Dashboard.Ingress
	pathType := networkingv1.PathTypePrefix

	obj := &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:        p.DashboardIngressName(),
			Namespace:   p.Namespace,
			Labels:      p.podLabels(),
			Annotations: ing.Annotations,
		},
		Spec: networkingv1.IngressSpec{
			IngressClassName: ing.IngressClassName,
			Rules: []networkingv1.IngressRule{
				{
					Host: ing.Host,
					IngressRuleValue: networkingv1.IngressRuleValue{
						HTTP: &networkingv1.HTTPIngressRuleValue{
							Paths: []networkingv1.HTTPIngressPath{
								{
									Path:     "/",
									PathType: &pathType,
									Backend: networkingv1.IngressBackend{
										Service: &networkingv1.IngressServiceBackend{
											Name: p.Name,
											Port: networkingv1.ServiceBackendPort{Number: p.Dashboard.Port},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	}
	if ing.TLSSecretName != nil {
		obj.Spec.TLS = []networkingv1.IngressTLS{{Hosts: []string{ing.Host}, SecretName: *ing.TLSSecretName}}
	}
	return obj
}

func resourceUtilizationMetric(name corev1.ResourceName, target *int32) autoscalingv2.MetricSpec {
	return autoscalingv2.MetricSpec{
		Type: autoscalingv2.ResourceMetricSourceType,
		Resource: &autoscalingv2.ResourceMetricSource{
			Name: name,
			Target: autoscalingv2.MetricTarget{
				Type:               autoscalingv2.UtilizationMetricType,
				AverageUtilization: target,
			},
		},
	}
}
