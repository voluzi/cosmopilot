package chainnodeset

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/banzaicloud/k8s-objectmatcher/patch"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/chainutils"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
)

func (r *Reconciler) initializeLegacySignerServiceNames(ctx context.Context, nodeSet *appsv1.ChainNodeSet) (bool, error) {
	if nodeSet.Status.LegacySignerServiceNamesInitialized {
		return false, nil
	}

	expected := map[string]string{}
	for i := range nodeSet.Spec.Nodes {
		expected[nodeSet.Spec.Nodes[i].GetServiceName(nodeSet)] = scopeGroup
	}
	for i := range nodeSet.Spec.Ingresses {
		expected[nodeSet.Spec.Ingresses[i].GetName(nodeSet)] = scopeGlobal
	}
	for i := range nodeSet.Spec.GatewayRoutes {
		expected[fmt.Sprintf("%s-global-%s", nodeSet.GetName(), nodeSet.Spec.GatewayRoutes[i].Name)] = scopeGlobal
	}

	services := &corev1.ServiceList{}
	if err := r.List(ctx, services, client.InNamespace(nodeSet.GetNamespace())); err != nil {
		return false, err
	}
	names := map[string]struct{}{}
	for i := range services.Items {
		svc := &services.Items[i]
		if !metav1.IsControlledBy(svc, nodeSet) {
			continue
		}
		scope, derived := expected[svc.GetName()]
		if !derived || svc.GetLabels()[controllers.LabelScope] != scope {
			continue
		}
		if strings.HasSuffix(svc.GetName(), "-signer") || strings.HasSuffix(svc.GetName(), "-signer-privval") {
			names[svc.GetName()] = struct{}{}
		}
	}

	nodeSet.Status.LegacySignerServiceNames = make([]string, 0, len(names))
	for name := range names {
		nodeSet.Status.LegacySignerServiceNames = append(nodeSet.Status.LegacySignerServiceNames, name)
	}
	sort.Strings(nodeSet.Status.LegacySignerServiceNames)
	nodeSet.Status.LegacySignerServiceNamesInitialized = true
	if err := r.Status().Update(ctx, nodeSet); err != nil {
		return false, err
	}
	return true, nil
}

func (r *Reconciler) ensureServices(ctx context.Context, nodeSet *appsv1.ChainNodeSet, guardReady map[string]bool) error {
	logger := log.FromContext(ctx)

	expectedGroup := map[string]bool{}
	expectedGlobal := map[string]bool{}

	ensure := func(svc *corev1.Service, scope string) error {
		if scope == scopeGroup {
			expectedGroup[svc.GetName()] = true
		} else {
			expectedGlobal[svc.GetName()] = true
		}
		return r.ensureService(ctx, svc)
	}

	for _, group := range nodeSet.Spec.Nodes {
		svc, err := r.getServiceSpec(nodeSet, group, guardReady[group.Name])
		if err != nil {
			return err
		}
		if err = ensure(svc, scopeGroup); err != nil {
			return err
		}

		svc, err = r.getInternalServiceSpec(nodeSet, group)
		if err != nil {
			return err
		}
		if err = ensure(svc, scopeGroup); err != nil {
			return err
		}
	}

	for _, ingress := range nodeSet.Spec.Ingresses {
		svc, err := r.getGlobalServiceSpec(nodeSet, ingress, cosmoGuardRouteReady(nodeSet, ingress.Groups, guardReady))
		if err != nil {
			return err
		}
		if err = ensure(svc, scopeGlobal); err != nil {
			return err
		}

		svc, err = r.getGlobalInternalServiceSpec(nodeSet, ingress)
		if err != nil {
			return err
		}
		if err = ensure(svc, scopeGlobal); err != nil {
			return err
		}
	}

	for _, gw := range nodeSet.Spec.GatewayRoutes {
		svc, err := r.getGlobalGatewayServiceSpec(nodeSet, gw, cosmoGuardRouteReady(nodeSet, gw.Groups, guardReady))
		if err != nil {
			return err
		}
		if err = ensure(svc, scopeGlobal); err != nil {
			return err
		}

		svc, err = r.getGlobalGatewayInternalServiceSpec(nodeSet, gw)
		if err != nil {
			return err
		}
		if err = ensure(svc, scopeGlobal); err != nil {
			return err
		}
	}

	// Clean up group-scoped services that are no longer expected. This also catches
	// legacy "-internal-internal" services created by older releases that used the
	// deprecated group-level UseInternalServices option.
	groupServices, err := r.listChainNodeSetServices(ctx, nodeSet, controllers.LabelScope, scopeGroup)
	if err != nil {
		return err
	}

	for _, svc := range groupServices.Items {
		groupName, hasGroup := svc.Labels[controllers.LabelChainNodeSetGroup]
		groupGone := !hasGroup || !ContainsGroup(nodeSet.Spec.Nodes, groupName)
		if groupGone || !expectedGroup[svc.GetName()] {
			logger.Info("deleting service", "svc", svc.GetName())
			if err = r.Delete(ctx, &svc); err != nil {
				return err
			}
		}
	}

	globalServices, err := r.listChainNodeSetServices(ctx, nodeSet, controllers.LabelScope, scopeGlobal)
	if err != nil {
		return err
	}

	for _, svc := range globalServices.Items {
		ingressName := svc.Labels[controllers.LabelGlobalIngress]
		gatewayName := svc.Labels[labelGlobalGateway]
		ownerGone := !ContainsGlobalIngress(nodeSet.Spec.Ingresses, ingressName, false) &&
			!ContainsGlobalGateway(nodeSet.Spec.GatewayRoutes, gatewayName)
		if ownerGone || !expectedGlobal[svc.GetName()] {
			logger.Info("deleting service", "svc", svc.GetName())
			if err = r.Delete(ctx, &svc); err != nil {
				return err
			}
		}
	}

	return nil
}

func (r *Reconciler) ensureService(ctx context.Context, svc *corev1.Service) error {
	logger := log.FromContext(ctx)

	currentSvc := &corev1.Service{}
	err := r.Get(ctx, client.ObjectKeyFromObject(svc), currentSvc)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("creating service", "svc", svc.GetName())
			return r.Create(ctx, svc)
		}
		return err
	}
	if desiredOwner := metav1.GetControllerOf(svc); desiredOwner != nil {
		currentOwner := metav1.GetControllerOf(currentSvc)
		if currentOwner == nil || currentOwner.UID != desiredOwner.UID {
			return fmt.Errorf("service %q is managed by another owner or is unowned; refusing to overwrite it", svc.GetName())
		}
	}

	patchResult, err := patch.DefaultPatchMaker.Calculate(currentSvc, svc)
	if err != nil {
		return err
	}

	if !patchResult.IsEmpty() {
		logger.Info("updating service", "svc", svc.GetName())

		svc.ObjectMeta.ResourceVersion = currentSvc.ObjectMeta.ResourceVersion
		if err := r.Update(ctx, svc); err != nil {
			return err
		}
	}

	*svc = *currentSvc
	return nil
}

func (r *Reconciler) getServiceSpec(nodeSet *appsv1.ChainNodeSet, group appsv1.NodeGroupSpec, guardReady bool) (*corev1.Service, error) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      group.GetServiceName(nodeSet),
			Namespace: nodeSet.GetNamespace(),
			Labels: WithChainNodeSetLabels(nodeSet, map[string]string{
				controllers.LabelChainNodeSet:      nodeSet.GetName(),
				controllers.LabelChainNodeSetGroup: group.Name,
				controllers.LabelScope:             scopeGroup,
			}),
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{
					Name:       chainutils.RpcPortName,
					Protocol:   corev1.ProtocolTCP,
					Port:       chainutils.RpcPort,
					TargetPort: intstr.FromInt32(chainutils.RpcPort),
				},
				{
					Name:       chainutils.LcdPortName,
					Protocol:   corev1.ProtocolTCP,
					Port:       chainutils.LcdPort,
					TargetPort: intstr.FromInt32(chainutils.LcdPort),
				},
				{
					Name:       chainutils.GrpcPortName,
					Protocol:   corev1.ProtocolTCP,
					Port:       chainutils.GrpcPort,
					TargetPort: intstr.FromInt32(chainutils.GrpcPort),
				},
			},
			Selector: map[string]string{
				controllers.LabelChainNodeSet:      nodeSet.GetName(),
				controllers.LabelChainNodeSetGroup: group.Name,
			},
		},
	}

	cfg := group.GetServiceConfig()
	if cfg.IsEvmEnabled() {
		svc.Spec.Ports = append(svc.Spec.Ports, corev1.ServicePort{
			Name:       controllers.EvmRpcPortName,
			Protocol:   corev1.ProtocolTCP,
			Port:       controllers.EvmRpcPort,
			TargetPort: intstr.FromInt32(controllers.EvmRpcPort),
		})
		svc.Spec.Ports = append(svc.Spec.Ports, corev1.ServicePort{
			Name:       controllers.EvmRpcWsPortName,
			Protocol:   corev1.ProtocolTCP,
			Port:       controllers.EvmRpcWsPort,
			TargetPort: intstr.FromInt32(controllers.EvmRpcWsPort),
		})
	}

	// When CosmoGuard is enabled and its Deployment is serving, flip the group Service to the guard
	// pods: repoint the selector at the standalone guard and target its listener ports. Until the
	// guard is ready the Service keeps targeting the node pods on raw ports (make-before-break).
	if cfg != nil && cfg.CosmoGuardEnabled() && guardReady {
		svc.Spec.Selector = cosmoGuardGroupSelector(nodeSet, group)
		svc.Spec.Ports[0].TargetPort = intstr.FromInt32(controllers.CosmoGuardRpcPort)
		svc.Spec.Ports[1].TargetPort = intstr.FromInt32(controllers.CosmoGuardLcdPort)
		svc.Spec.Ports[2].TargetPort = intstr.FromInt32(controllers.CosmoGuardGrpcPort)
		if cfg.IsEvmEnabled() {
			svc.Spec.Ports[3].TargetPort = intstr.FromInt32(controllers.CosmoGuardEvmRpcPort)
			svc.Spec.Ports[4].TargetPort = intstr.FromInt32(controllers.CosmoGuardEvmRpcWsPort)
		}
	}

	return svc, controllerutil.SetControllerReference(nodeSet, svc, r.Scheme)
}

func (r *Reconciler) getInternalServiceSpec(nodeSet *appsv1.ChainNodeSet, group appsv1.NodeGroupSpec) (*corev1.Service, error) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-internal", group.GetServiceName(nodeSet)),
			Namespace: nodeSet.GetNamespace(),
			Labels: WithChainNodeSetLabels(nodeSet, map[string]string{
				controllers.LabelChainNodeSet:      nodeSet.GetName(),
				controllers.LabelChainNodeSetGroup: group.Name,
				controllers.LabelScope:             scopeGroup,
			}),
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{
					Name:       chainutils.RpcPortName,
					Protocol:   corev1.ProtocolTCP,
					Port:       chainutils.RpcPort,
					TargetPort: intstr.FromInt32(chainutils.RpcPort),
				},
				{
					Name:       chainutils.LcdPortName,
					Protocol:   corev1.ProtocolTCP,
					Port:       chainutils.LcdPort,
					TargetPort: intstr.FromInt32(chainutils.LcdPort),
				},
				{
					Name:       chainutils.GrpcPortName,
					Protocol:   corev1.ProtocolTCP,
					Port:       chainutils.GrpcPort,
					TargetPort: intstr.FromInt32(chainutils.GrpcPort),
				},
			},
			Selector: map[string]string{
				controllers.LabelChainNodeSet:      nodeSet.GetName(),
				controllers.LabelChainNodeSetGroup: group.Name,
			},
		},
	}

	if group.GetServiceConfig().IsEvmEnabled() {
		svc.Spec.Ports = append(svc.Spec.Ports, corev1.ServicePort{
			Name:       controllers.EvmRpcPortName,
			Protocol:   corev1.ProtocolTCP,
			Port:       controllers.EvmRpcPort,
			TargetPort: intstr.FromInt32(controllers.EvmRpcPort),
		})
		svc.Spec.Ports = append(svc.Spec.Ports, corev1.ServicePort{
			Name:       controllers.EvmRpcWsPortName,
			Protocol:   corev1.ProtocolTCP,
			Port:       controllers.EvmRpcWsPort,
			TargetPort: intstr.FromInt32(controllers.EvmRpcWsPort),
		})
	}

	return svc, controllerutil.SetControllerReference(nodeSet, svc, r.Scheme)
}

func (r *Reconciler) getGlobalServiceSpec(nodeSet *appsv1.ChainNodeSet, globalIngress appsv1.GlobalIngressConfig, guardReady bool) (*corev1.Service, error) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      globalIngress.GetName(nodeSet),
			Namespace: nodeSet.GetNamespace(),
			Labels: WithChainNodeSetLabels(nodeSet, map[string]string{
				controllers.LabelChainNodeSet:  nodeSet.GetName(),
				controllers.LabelGlobalIngress: globalIngress.Name,
				controllers.LabelScope:         scopeGlobal,
			}),
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{
					Name:       chainutils.RpcPortName,
					Protocol:   corev1.ProtocolTCP,
					Port:       chainutils.RpcPort,
					TargetPort: intstr.FromInt32(chainutils.RpcPort),
				},
				{
					Name:       chainutils.LcdPortName,
					Protocol:   corev1.ProtocolTCP,
					Port:       chainutils.LcdPort,
					TargetPort: intstr.FromInt32(chainutils.LcdPort),
				},
				{
					Name:       chainutils.GrpcPortName,
					Protocol:   corev1.ProtocolTCP,
					Port:       chainutils.GrpcPort,
					TargetPort: intstr.FromInt32(chainutils.GrpcPort),
				},
				{
					Name:       controllers.EvmRpcPortName,
					Protocol:   corev1.ProtocolTCP,
					Port:       controllers.EvmRpcPort,
					TargetPort: intstr.FromInt32(controllers.EvmRpcPort),
				},
				{
					Name:       controllers.EvmRpcWsPortName,
					Protocol:   corev1.ProtocolTCP,
					Port:       controllers.EvmRpcWsPort,
					TargetPort: intstr.FromInt32(controllers.EvmRpcWsPort),
				},
			},
			Selector: map[string]string{
				controllers.LabelChainNodeSet:  nodeSet.GetName(),
				globalIngress.GetName(nodeSet): strconv.FormatBool(true),
			},
		},
	}

	if globalIngress.ShouldUseCosmoGuard(nodeSet) && guardReady {
		svc.Spec.Selector = cosmoGuardRouteSelector(globalIngress.GetName(nodeSet))
		svc.Spec.Ports[0].TargetPort = intstr.FromInt32(controllers.CosmoGuardRpcPort)
		svc.Spec.Ports[1].TargetPort = intstr.FromInt32(controllers.CosmoGuardLcdPort)
		svc.Spec.Ports[2].TargetPort = intstr.FromInt32(controllers.CosmoGuardGrpcPort)
		svc.Spec.Ports[3].TargetPort = intstr.FromInt32(controllers.CosmoGuardEvmRpcPort)
		svc.Spec.Ports[4].TargetPort = intstr.FromInt32(controllers.CosmoGuardEvmRpcWsPort)
	}

	return svc, controllerutil.SetControllerReference(nodeSet, svc, r.Scheme)
}

func (r *Reconciler) getGlobalInternalServiceSpec(nodeSet *appsv1.ChainNodeSet, globalIngress appsv1.GlobalIngressConfig) (*corev1.Service, error) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-internal", globalIngress.GetName(nodeSet)),
			Namespace: nodeSet.GetNamespace(),
			Labels: WithChainNodeSetLabels(nodeSet, map[string]string{
				controllers.LabelChainNodeSet:  nodeSet.GetName(),
				controllers.LabelGlobalIngress: globalIngress.Name,
				controllers.LabelScope:         scopeGlobal,
			}),
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{
					Name:       chainutils.RpcPortName,
					Protocol:   corev1.ProtocolTCP,
					Port:       chainutils.RpcPort,
					TargetPort: intstr.FromInt32(chainutils.RpcPort),
				},
				{
					Name:       chainutils.LcdPortName,
					Protocol:   corev1.ProtocolTCP,
					Port:       chainutils.LcdPort,
					TargetPort: intstr.FromInt32(chainutils.LcdPort),
				},
				{
					Name:       chainutils.GrpcPortName,
					Protocol:   corev1.ProtocolTCP,
					Port:       chainutils.GrpcPort,
					TargetPort: intstr.FromInt32(chainutils.GrpcPort),
				},
				{
					Name:       controllers.EvmRpcPortName,
					Protocol:   corev1.ProtocolTCP,
					Port:       controllers.EvmRpcPort,
					TargetPort: intstr.FromInt32(controllers.EvmRpcPort),
				},
				{
					Name:       controllers.EvmRpcWsPortName,
					Protocol:   corev1.ProtocolTCP,
					Port:       controllers.EvmRpcWsPort,
					TargetPort: intstr.FromInt32(controllers.EvmRpcWsPort),
				},
			},
			Selector: map[string]string{
				controllers.LabelChainNodeSet:  nodeSet.GetName(),
				globalIngress.GetName(nodeSet): strconv.FormatBool(true),
			},
		},
	}

	return svc, controllerutil.SetControllerReference(nodeSet, svc, r.Scheme)
}

func (r *Reconciler) getGlobalGatewayServiceSpec(nodeSet *appsv1.ChainNodeSet, gw appsv1.GlobalGatewayConfig, guardReady bool) (*corev1.Service, error) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-global-%s", nodeSet.GetName(), gw.Name),
			Namespace: nodeSet.GetNamespace(),
			Labels: WithChainNodeSetLabels(nodeSet, map[string]string{
				controllers.LabelChainNodeSet: nodeSet.GetName(),
				labelGlobalGateway:            gw.Name,
				controllers.LabelScope:        scopeGlobal,
			}),
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{
					Name:       chainutils.RpcPortName,
					Protocol:   corev1.ProtocolTCP,
					Port:       chainutils.RpcPort,
					TargetPort: intstr.FromInt32(chainutils.RpcPort),
				},
				{
					Name:       chainutils.LcdPortName,
					Protocol:   corev1.ProtocolTCP,
					Port:       chainutils.LcdPort,
					TargetPort: intstr.FromInt32(chainutils.LcdPort),
				},
				{
					Name:       chainutils.GrpcPortName,
					Protocol:   corev1.ProtocolTCP,
					Port:       chainutils.GrpcPort,
					TargetPort: intstr.FromInt32(chainutils.GrpcPort),
				},
				{
					Name:       controllers.EvmRpcPortName,
					Protocol:   corev1.ProtocolTCP,
					Port:       controllers.EvmRpcPort,
					TargetPort: intstr.FromInt32(controllers.EvmRpcPort),
				},
				{
					Name:       controllers.EvmRpcWsPortName,
					Protocol:   corev1.ProtocolTCP,
					Port:       controllers.EvmRpcWsPort,
					TargetPort: intstr.FromInt32(controllers.EvmRpcWsPort),
				},
			},
			Selector: map[string]string{
				controllers.LabelChainNodeSet: nodeSet.GetName(),
				gw.GetName(nodeSet):           strconv.FormatBool(true),
			},
		},
	}

	if gw.ShouldUseCosmoGuard(nodeSet) && guardReady {
		svc.Spec.Selector = cosmoGuardRouteSelector(gw.GetName(nodeSet))
		svc.Spec.Ports[0].TargetPort = intstr.FromInt32(controllers.CosmoGuardRpcPort)
		svc.Spec.Ports[1].TargetPort = intstr.FromInt32(controllers.CosmoGuardLcdPort)
		svc.Spec.Ports[2].TargetPort = intstr.FromInt32(controllers.CosmoGuardGrpcPort)
		svc.Spec.Ports[3].TargetPort = intstr.FromInt32(controllers.CosmoGuardEvmRpcPort)
		svc.Spec.Ports[4].TargetPort = intstr.FromInt32(controllers.CosmoGuardEvmRpcWsPort)
	}

	return svc, controllerutil.SetControllerReference(nodeSet, svc, r.Scheme)
}

func (r *Reconciler) getGlobalGatewayInternalServiceSpec(nodeSet *appsv1.ChainNodeSet, gw appsv1.GlobalGatewayConfig) (*corev1.Service, error) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-global-%s-internal", nodeSet.GetName(), gw.Name),
			Namespace: nodeSet.GetNamespace(),
			Labels: WithChainNodeSetLabels(nodeSet, map[string]string{
				controllers.LabelChainNodeSet: nodeSet.GetName(),
				labelGlobalGateway:            gw.Name,
				controllers.LabelScope:        scopeGlobal,
			}),
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{
					Name:       chainutils.RpcPortName,
					Protocol:   corev1.ProtocolTCP,
					Port:       chainutils.RpcPort,
					TargetPort: intstr.FromInt32(chainutils.RpcPort),
				},
				{
					Name:       chainutils.LcdPortName,
					Protocol:   corev1.ProtocolTCP,
					Port:       chainutils.LcdPort,
					TargetPort: intstr.FromInt32(chainutils.LcdPort),
				},
				{
					Name:       chainutils.GrpcPortName,
					Protocol:   corev1.ProtocolTCP,
					Port:       chainutils.GrpcPort,
					TargetPort: intstr.FromInt32(chainutils.GrpcPort),
				},
				{
					Name:       controllers.EvmRpcPortName,
					Protocol:   corev1.ProtocolTCP,
					Port:       controllers.EvmRpcPort,
					TargetPort: intstr.FromInt32(controllers.EvmRpcPort),
				},
				{
					Name:       controllers.EvmRpcWsPortName,
					Protocol:   corev1.ProtocolTCP,
					Port:       controllers.EvmRpcWsPort,
					TargetPort: intstr.FromInt32(controllers.EvmRpcWsPort),
				},
			},
			Selector: map[string]string{
				controllers.LabelChainNodeSet: nodeSet.GetName(),
				gw.GetName(nodeSet):           strconv.FormatBool(true),
			},
		},
	}

	return svc, controllerutil.SetControllerReference(nodeSet, svc, r.Scheme)
}

func (r *Reconciler) listChainNodeSetServices(ctx context.Context, nodeSet *appsv1.ChainNodeSet, l ...string) (*corev1.ServiceList, error) {
	if len(l)%2 != 0 {
		return nil, fmt.Errorf("list of labels must contain pairs of key-value")
	}

	selectorMap := map[string]string{controllers.LabelChainNodeSet: nodeSet.GetName()}
	for i := 0; i < len(l); i += 2 {
		selectorMap[l[i]] = l[i+1]
	}

	serviceList := &corev1.ServiceList{}
	return serviceList, r.List(ctx, serviceList, &client.ListOptions{
		LabelSelector: labels.SelectorFromSet(selectorMap),
	})
}
