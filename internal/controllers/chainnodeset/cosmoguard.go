package chainnodeset

import (
	"context"
	"fmt"
	"strings"

	k8sappsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/chainutils"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
	"github.com/voluzi/cosmopilot/v2/internal/cosmoguard"
	"github.com/voluzi/cosmopilot/v2/pkg/utils"
)

// groupCosmoGuardName returns the name of the CosmoGuard Deployment/Service fronting a node group.
func groupCosmoGuardName(nodeSet *appsv1.ChainNodeSet, group appsv1.NodeGroupSpec) string {
	return fmt.Sprintf("%s-cosmoguard", group.GetServiceName(nodeSet))
}

// groupCosmoGuardUpstreamName returns the name of the headless Service CosmoGuard uses to discover
// the group's node pods.
func groupCosmoGuardUpstreamName(nodeSet *appsv1.ChainNodeSet, group appsv1.NodeGroupSpec) string {
	return fmt.Sprintf("%s-cosmoguard-upstream", group.GetServiceName(nodeSet))
}

func cosmoGuardRouteLabelKey(routeName string) string {
	return cosmoGuardRouteLabelPrefix + routeName
}

// cosmoGuardRouteLabels returns the per-route labels a group's guard pods must carry so global
// ingress/gateway Services (which target several groups) can select them.
func cosmoGuardRouteLabels(nodeSet *appsv1.ChainNodeSet, groupName string) map[string]string {
	labels := map[string]string{}
	for _, ing := range nodeSet.Spec.Ingresses {
		if ing.HasGroup(groupName) {
			labels[cosmoGuardRouteLabelKey(ing.GetName(nodeSet))] = controllers.StringValueTrue
		}
	}
	for _, gw := range nodeSet.Spec.GatewayRoutes {
		if gw.HasGroup(groupName) {
			labels[cosmoGuardRouteLabelKey(gw.GetName(nodeSet))] = controllers.StringValueTrue
		}
	}
	return labels
}

// groupCosmoGuardParams builds the CosmoGuard render parameters for a node group.
func (r *Reconciler) groupCosmoGuardParams(nodeSet *appsv1.ChainNodeSet, group appsv1.NodeGroupSpec) cosmoguard.Params {
	cfg := group.GetServiceConfig()

	name := groupCosmoGuardName(nodeSet, group)
	p := cosmoguard.Params{
		Name:                name,
		Namespace:           nodeSet.GetNamespace(),
		Image:               cfg.GetCosmoGuardImage(r.opts.CosmoGuardImage),
		Replicas:            cfg.GetCosmoGuardReplicas(),
		DiscoveryHost:       fmt.Sprintf("%s.%s.svc.cluster.local", groupCosmoGuardUpstreamName(nodeSet, group), nodeSet.GetNamespace()),
		EvmEnabled:          cfg.IsEvmEnabled(),
		ConfigMap:           cfg.GetCosmoGuardConfig(),
		Resources:           cfg.GetCosmoGuardResources(),
		Labels:              cosmoGuardRouteLabels(nodeSet, group.Name),
		PeerServiceName:     cosmoguard.PeerServiceName(name),
		EncryptionKeySecret: cosmoguard.EncryptionKeySecretName(name),
	}

	if cfg.CosmoGuardAutoscalingEnabled() {
		as := cfg.GetCosmoGuardAutoscaling()
		target := as.TargetCPUUtilizationPercentage
		if target == nil && as.TargetMemoryUtilizationPercentage == nil {
			target = ptr.To(appsv1.DefaultCosmoGuardAutoscalingCPUTarget)
		}
		p.Autoscaling = &cosmoguard.AutoscalingParams{
			MinReplicas:  as.MinReplicas,
			MaxReplicas:  as.MaxReplicas,
			TargetCPU:    target,
			TargetMemory: as.TargetMemoryUtilizationPercentage,
		}
	}

	if cfg.CosmoGuardDashboardEnabled() {
		p.Dashboard = buildDashboardParams(cfg)
	}

	return p
}

// buildDashboardParams maps the CosmoGuard dashboard config onto render params.
func buildDashboardParams(cfg *appsv1.Config) *cosmoguard.DashboardParams {
	d := cfg.GetCosmoGuardDashboard()
	dp := &cosmoguard.DashboardParams{Port: cfg.GetCosmoGuardDashboardPort()}
	if d.BasicAuth != nil {
		dp.AuthUser = &d.BasicAuth.Username
		dp.AuthPassword = &d.BasicAuth.Password
	}
	if d.Ingress != nil {
		dp.Ingress = &cosmoguard.DashboardIngressParams{
			Host:             d.Ingress.Host,
			IngressClassName: d.Ingress.IngressClassName,
			Annotations:      d.Ingress.Annotations,
			TLSSecretName:    d.Ingress.TLSSecretName,
		}
	}
	return dp
}

// buildGroupCosmoGuardUpstreamService renders the headless Service whose endpoints are the group's
// node pods (at raw ports). CosmoGuard discovers each pod through it.
func (r *Reconciler) buildGroupCosmoGuardUpstreamService(nodeSet *appsv1.ChainNodeSet, group appsv1.NodeGroupSpec) (*corev1.Service, error) {
	cfg := group.GetServiceConfig()

	ports := []corev1.ServicePort{
		{Name: chainutils.RpcPortName, Protocol: corev1.ProtocolTCP, Port: chainutils.RpcPort, TargetPort: intstr.FromInt32(chainutils.RpcPort)},
		{Name: chainutils.LcdPortName, Protocol: corev1.ProtocolTCP, Port: chainutils.LcdPort, TargetPort: intstr.FromInt32(chainutils.LcdPort)},
		{Name: chainutils.GrpcPortName, Protocol: corev1.ProtocolTCP, Port: chainutils.GrpcPort, TargetPort: intstr.FromInt32(chainutils.GrpcPort)},
	}
	if cfg.IsEvmEnabled() {
		ports = append(ports,
			corev1.ServicePort{Name: controllers.EvmRpcPortName, Protocol: corev1.ProtocolTCP, Port: controllers.EvmRpcPort, TargetPort: intstr.FromInt32(controllers.EvmRpcPort)},
			corev1.ServicePort{Name: controllers.EvmRpcWsPortName, Protocol: corev1.ProtocolTCP, Port: controllers.EvmRpcWsPort, TargetPort: intstr.FromInt32(controllers.EvmRpcWsPort)},
		)
	}

	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      groupCosmoGuardUpstreamName(nodeSet, group),
			Namespace: nodeSet.GetNamespace(),
			Labels: WithChainNodeSetLabels(nodeSet, map[string]string{
				controllers.LabelChainNodeSet:      nodeSet.GetName(),
				controllers.LabelChainNodeSetGroup: group.Name,
				controllers.LabelScope:             scopeCosmoGuard,
			}),
		},
		Spec: corev1.ServiceSpec{
			ClusterIP:                corev1.ClusterIPNone,
			PublishNotReadyAddresses: true,
			Ports:                    ports,
			Selector: map[string]string{
				controllers.LabelChainNodeSet:      nodeSet.GetName(),
				controllers.LabelChainNodeSetGroup: group.Name,
			},
		},
	}
	return svc, controllerutil.SetControllerReference(nodeSet, svc, r.Scheme)
}

// withCosmoGuardScope stamps the cosmoguard scope label onto a guard resource so stale guards can be
// listed and cleaned up without colliding with the group/global Service cleanup selectors.
func withCosmoGuardScope(obj client.Object) {
	labels := obj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[controllers.LabelScope] = scopeCosmoGuard
	obj.SetLabels(labels)
}

// ensureCosmoGuards reconciles the per-group CosmoGuard deployments. It returns a map keyed by group
// name reporting whether each group's guard has finished rolling out; the Service builders use this
// to flip a group/global/gateway Service's selector to the guard pods only once it is serving,
// preserving traffic during rollout and migration (make-before-break).
func (r *Reconciler) ensureCosmoGuards(ctx context.Context, nodeSet *appsv1.ChainNodeSet) (map[string]bool, error) {
	logger := log.FromContext(ctx)

	ready := map[string]bool{}
	expected := map[string]bool{}
	expectedIngress := map[string]bool{}

	for _, group := range nodeSet.Spec.Nodes {
		cfg := group.GetServiceConfig()
		if !cfg.CosmoGuardEnabled() {
			continue
		}

		if cfg.GetCosmoGuardConfig() == nil {
			// The CRD requires a config ConfigMap, but guard against a nil deref in the builder in case
			// validation was bypassed. Skip this group's guard until a config is provided.
			logger.Info("cosmoguard enabled without a config ConfigMap; skipping", "group", group.Name)
			continue
		}

		name := groupCosmoGuardName(nodeSet, group)
		expected[name] = true

		upstream, err := r.buildGroupCosmoGuardUpstreamService(nodeSet, group)
		if err != nil {
			return nil, err
		}
		if err := cosmoguard.ApplyOwned(ctx, r.Client, r.Scheme, nodeSet, upstream); err != nil {
			return nil, fmt.Errorf("failed to apply cosmoguard upstream service for group %s: %w", group.Name, err)
		}

		// Provision the olric gossip encryption Secret (once) before the StatefulSet references it.
		if err := r.ensureCosmoGuardSecret(ctx, nodeSet, cosmoguard.EncryptionKeySecretName(name)); err != nil {
			return nil, fmt.Errorf("failed to ensure cosmoguard secret for group %s: %w", group.Name, err)
		}

		params := r.groupCosmoGuardParams(nodeSet, group)

		peer := params.PeerService()
		withCosmoGuardScope(peer)
		if err := cosmoguard.ApplyOwned(ctx, r.Client, r.Scheme, nodeSet, peer); err != nil {
			return nil, fmt.Errorf("failed to apply cosmoguard peer service for group %s: %w", group.Name, err)
		}

		sts := params.StatefulSet()
		withCosmoGuardScope(sts)
		if err := cosmoguard.ApplyOwned(ctx, r.Client, r.Scheme, nodeSet, sts); err != nil {
			return nil, fmt.Errorf("failed to apply cosmoguard statefulset for group %s: %w", group.Name, err)
		}

		svc := params.Service()
		withCosmoGuardScope(svc)
		if err := cosmoguard.ApplyOwned(ctx, r.Client, r.Scheme, nodeSet, svc); err != nil {
			return nil, fmt.Errorf("failed to apply cosmoguard service for group %s: %w", group.Name, err)
		}

		if ing := params.DashboardIngress(); ing != nil {
			withCosmoGuardScope(ing)
			if err := cosmoguard.ApplyOwned(ctx, r.Client, r.Scheme, nodeSet, ing); err != nil {
				return nil, fmt.Errorf("failed to apply cosmoguard dashboard ingress for group %s: %w", group.Name, err)
			}
			expectedIngress[ing.GetName()] = true
		}

		if hpa := params.HPA(); hpa != nil {
			withCosmoGuardScope(hpa)
			if err := cosmoguard.ApplyOwned(ctx, r.Client, r.Scheme, nodeSet, hpa); err != nil {
				return nil, fmt.Errorf("failed to apply cosmoguard hpa for group %s: %w", group.Name, err)
			}
		} else {
			// Autoscaling was disabled: remove any HPA we previously created.
			if err := r.deleteCosmoGuardHPA(ctx, nodeSet, name); err != nil {
				return nil, err
			}
		}

		rolledOut, err := cosmoguard.IsRolledOut(ctx, r.Client, nodeSet.GetNamespace(), name)
		if err != nil {
			return nil, err
		}
		ready[group.Name] = rolledOut
		if !rolledOut {
			logger.Info("waiting for cosmoguard rollout", "group", group.Name, "cosmoguard", name)
		}
	}

	if err := r.cleanupStaleCosmoGuards(ctx, nodeSet, expected, expectedIngress); err != nil {
		return nil, err
	}

	return ready, nil
}

func (r *Reconciler) deleteCosmoGuardHPA(ctx context.Context, nodeSet *appsv1.ChainNodeSet, name string) error {
	hpa := &autoscalingv2.HorizontalPodAutoscaler{}
	err := r.Get(ctx, client.ObjectKey{Namespace: nodeSet.GetNamespace(), Name: name}, hpa)
	if err != nil {
		return client.IgnoreNotFound(err)
	}
	if !metav1.IsControlledBy(hpa, nodeSet) {
		return nil
	}
	return client.IgnoreNotFound(r.Delete(ctx, hpa))
}

// ensureCosmoGuardSecret creates the olric gossip encryption Secret if it does not exist yet. It
// never overwrites an existing Secret: the key must stay stable for the life of the cluster, so it
// is generated exactly once.
func (r *Reconciler) ensureCosmoGuardSecret(ctx context.Context, nodeSet *appsv1.ChainNodeSet, name string) error {
	secret := &corev1.Secret{}
	err := r.Get(ctx, client.ObjectKey{Namespace: nodeSet.GetNamespace(), Name: name}, secret)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}

	key, err := cosmoguard.GenerateEncryptionKey()
	if err != nil {
		return err
	}
	secret = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: nodeSet.GetNamespace(),
			Labels:    map[string]string{controllers.LabelScope: scopeCosmoGuard},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{cosmoguard.EncryptionKeySecretKey: []byte(key)},
	}
	if err := controllerutil.SetControllerReference(nodeSet, secret, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, secret)
}

// cleanupStaleCosmoGuards deletes guard resources whose group no longer enables CosmoGuard (or was
// removed). It lists by the cosmoguard scope label and owner, deleting StatefulSets, Services (guard
// + headless upstream + peer), Secrets, HPAs and dashboard Ingresses that are not in the expected
// set. Auxiliary resources are matched by stripping their suffix back to the guard name.
func (r *Reconciler) cleanupStaleCosmoGuards(ctx context.Context, nodeSet *appsv1.ChainNodeSet, expected, expectedIngress map[string]bool) error {
	logger := log.FromContext(ctx)
	sel := client.MatchingLabels{controllers.LabelScope: scopeCosmoGuard}
	ns := client.InNamespace(nodeSet.GetNamespace())

	ingresses := &networkingv1.IngressList{}
	if err := r.List(ctx, ingresses, ns, sel); err != nil {
		return err
	}
	for i := range ingresses.Items {
		in := &ingresses.Items[i]
		if !metav1.IsControlledBy(in, nodeSet) || expectedIngress[in.GetName()] {
			continue
		}
		logger.Info("deleting stale cosmoguard dashboard ingress", "name", in.GetName())
		if err := client.IgnoreNotFound(r.Delete(ctx, in)); err != nil {
			return err
		}
	}

	sets := &k8sappsv1.StatefulSetList{}
	if err := r.List(ctx, sets, ns, sel); err != nil {
		return err
	}
	for i := range sets.Items {
		s := &sets.Items[i]
		if !metav1.IsControlledBy(s, nodeSet) || expected[s.GetName()] {
			continue
		}
		logger.Info("deleting stale cosmoguard statefulset", "name", s.GetName())
		if err := client.IgnoreNotFound(r.Delete(ctx, s)); err != nil {
			return err
		}
	}

	hpas := &autoscalingv2.HorizontalPodAutoscalerList{}
	if err := r.List(ctx, hpas, ns, sel); err != nil {
		return err
	}
	for i := range hpas.Items {
		h := &hpas.Items[i]
		if !metav1.IsControlledBy(h, nodeSet) || expected[h.GetName()] {
			continue
		}
		if err := client.IgnoreNotFound(r.Delete(ctx, h)); err != nil {
			return err
		}
	}

	secrets := &corev1.SecretList{}
	if err := r.List(ctx, secrets, ns, sel); err != nil {
		return err
	}
	for i := range secrets.Items {
		s := &secrets.Items[i]
		if !metav1.IsControlledBy(s, nodeSet) || expected[cosmoGuardBaseName(s.GetName())] {
			continue
		}
		logger.Info("deleting stale cosmoguard secret", "name", s.GetName())
		if err := client.IgnoreNotFound(r.Delete(ctx, s)); err != nil {
			return err
		}
	}

	svcs := &corev1.ServiceList{}
	if err := r.List(ctx, svcs, ns, sel); err != nil {
		return err
	}
	for i := range svcs.Items {
		s := &svcs.Items[i]
		if !metav1.IsControlledBy(s, nodeSet) || expected[cosmoGuardBaseName(s.GetName())] {
			continue
		}
		logger.Info("deleting stale cosmoguard service", "name", s.GetName())
		if err := client.IgnoreNotFound(r.Delete(ctx, s)); err != nil {
			return err
		}
	}

	return nil
}

// cosmoGuardBaseName strips a guard resource's suffix back to the base guard name so auxiliary
// resources (upstream/peer Services, cluster Secret) can be matched against the expected guard set.
func cosmoGuardBaseName(name string) string {
	for _, suffix := range []string{"-upstream", "-peer", "-cluster", "-dashboard"} {
		if strings.HasSuffix(name, suffix) {
			return strings.TrimSuffix(name, suffix)
		}
	}
	return name
}

// cosmoGuardGroupSelector returns the Service selector that targets a group's guard pods.
func cosmoGuardGroupSelector(nodeSet *appsv1.ChainNodeSet, group appsv1.NodeGroupSpec) map[string]string {
	return cosmoguard.InstanceLabels(groupCosmoGuardName(nodeSet, group))
}

// cosmoGuardRouteSelector returns the Service selector that targets the guard pods of every group a
// global ingress/gateway route spans.
func cosmoGuardRouteSelector(routeName string) map[string]string {
	return utils.MergeMaps(cosmoguard.AppLabel(), map[string]string{
		cosmoGuardRouteLabelKey(routeName): controllers.StringValueTrue,
	})
}

// cosmoGuardRouteReady reports whether every CosmoGuard-enabled group targeted by a global route has
// finished rolling out. A route Service is only flipped to guard pods once this is true, so traffic
// keeps flowing through the node pods until the guards are serving.
func cosmoGuardRouteReady(nodeSet *appsv1.ChainNodeSet, groups []string, guardReady map[string]bool) bool {
	for _, groupName := range groups {
		if groupName == appsv1.ReservedValidatorGroupName {
			// The legacy .spec.validator guard is not managed here; never block the flip on it.
			continue
		}
		for _, g := range nodeSet.Spec.Nodes {
			if g.Name != groupName {
				continue
			}
			if cfg := g.GetServiceConfig(); cfg != nil && cfg.CosmoGuardEnabled() && !guardReady[groupName] {
				return false
			}
		}
	}
	return true
}
