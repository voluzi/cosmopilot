// Package cosmoguard renders the Kubernetes resources for a standalone CosmoGuard deployment
// (Deployment, Service and HorizontalPodAutoscaler) that fronts one or more blockchain nodes.
//
// CosmoGuard v4 is configured through a user-owned ConfigMap that carries ONLY policy rules.
// Everything the operator controls (upstream node discovery, listener/metrics/dashboard settings)
// is injected through COSMOGUARD_* environment variables, so the user ConfigMap is never rewritten
// and CosmoGuard's rules hot-reload keeps working. See internal/controllers for the wiring.
package cosmoguard

import (
	"strconv"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
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
	// labelAppName / appName / labelInstance identify CosmoGuard pods and back the Deployment
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
)

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

	PriorityClassName string
	ImagePullSecrets  []corev1.LocalObjectReference
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

func (p Params) podLabels() map[string]string {
	return utils.MergeMaps(p.Labels, InstanceLabels(p.Name))
}

// env builds the operator-managed COSMOGUARD_* environment injected into the guard container.
func (p Params) env() []corev1.EnvVar {
	env := []corev1.EnvVar{
		{Name: "COSMOGUARD_METRICS_ENABLE", Value: "true"},
		{Name: "COSMOGUARD_METRICS_PORT", Value: intToStr(controllers.CosmoGuardMetricsPort)},
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

	if p.Dashboard != nil {
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
	return ports
}

// securityContext returns the restricted context hardened with a read-only root filesystem
// (CosmoGuard is entirely in-memory and needs no writable paths).
func securityContext() *corev1.SecurityContext {
	sc := k8s.RestrictedSecurityContext()
	sc.ReadOnlyRootFilesystem = ptr.To(true)
	return sc
}

// Deployment renders the CosmoGuard Deployment.
func (p Params) Deployment() *appsv1.Deployment {
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

	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      p.Name,
			Namespace: p.Namespace,
			Labels:    p.podLabels(),
		},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: InstanceLabels(p.Name)},
			Strategy: appsv1.DeploymentStrategy{
				Type: appsv1.RollingUpdateDeploymentStrategyType,
				RollingUpdate: &appsv1.RollingUpdateDeployment{
					MaxUnavailable: ptr.To(intstr.FromInt32(0)),
					MaxSurge:       ptr.To(intstr.FromInt32(1)),
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: p.podLabels()},
				Spec: corev1.PodSpec{
					SecurityContext:   k8s.RestrictedPodSecurityContext(),
					PriorityClassName: p.PriorityClassName,
					ImagePullSecrets:  p.ImagePullSecrets,
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
		dep.Spec.Replicas = ptr.To(p.Replicas)
	}

	return dep
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

// HPA renders the HorizontalPodAutoscaler for the guard Deployment, or nil when autoscaling is off.
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
				Kind:       "Deployment",
				Name:       p.Name,
			},
			MinReplicas: p.Autoscaling.MinReplicas,
			MaxReplicas: p.Autoscaling.MaxReplicas,
			Metrics:     metrics,
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
