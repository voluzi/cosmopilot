package chainnodeset

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"
	gwapiv1a2 "sigs.k8s.io/gateway-api/apis/v1alpha2"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/chainutils"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
)

const (
	labelGlobalGateway = "global-gateway"
)

func (r *Reconciler) ensureGatewayRoutes(ctx context.Context, nodeSet *appsv1.ChainNodeSet) error {
	logger := log.FromContext(ctx)

	desiredHTTPRouteNames := map[string]bool{}
	desiredGRPCRouteNames := map[string]bool{}

	for _, gw := range nodeSet.Spec.Gateways {
		if gw.CreateServicesOnly() {
			continue
		}

		httpRoutes, err := r.getGlobalHTTPRouteSpecs(nodeSet, gw)
		if err != nil {
			return err
		}
		for _, route := range httpRoutes {
			if err = controllers.EnsureHTTPRoute(ctx, r.Client, route); err != nil {
				return err
			}
			desiredHTTPRouteNames[route.Name] = true
		}

		grpcRouteName := gw.GetGrpcName(nodeSet)
		if gw.EnableGRPC {
			grpcRoute, err := r.getGlobalGRPCRouteSpec(nodeSet, gw)
			if err != nil {
				return err
			}
			if err = controllers.EnsureGRPCRoute(ctx, r.Client, grpcRoute); err != nil {
				return err
			}
			desiredGRPCRouteNames[grpcRoute.Name] = true
		} else {
			if err = r.Delete(ctx, &gwapiv1a2.GRPCRoute{
				ObjectMeta: metav1.ObjectMeta{
					Name:      grpcRouteName,
					Namespace: nodeSet.GetNamespace(),
				},
			}); err != nil && !errors.IsNotFound(err) && !controllers.IsCRDNotInstalled(err) {
				return err
			}
		}
	}

	// Cleanup stale routes
	existingHTTP, existingGRPC, err := r.listChainNodeSetGatewayRoutes(ctx, nodeSet)
	if err != nil {
		return err
	}

	for _, route := range existingHTTP {
		// Only clean up global gateway routes, skip cosmoseed routes
		if _, isGlobal := route.Labels[labelGlobalGateway]; !isGlobal {
			continue
		}
		if !desiredHTTPRouteNames[route.Name] {
			logger.Info("deleting stale httproute", "httproute", route.GetName())
			if err = r.Delete(ctx, &route); err != nil && !errors.IsNotFound(err) {
				return err
			}
		}
	}

	for _, route := range existingGRPC {
		if _, isGlobal := route.Labels[labelGlobalGateway]; !isGlobal {
			continue
		}
		if !desiredGRPCRouteNames[route.Name] {
			logger.Info("deleting stale grpcroute", "grpcroute", route.GetName())
			if err = r.Delete(ctx, &route); err != nil && !errors.IsNotFound(err) {
				return err
			}
		}
	}

	return nil
}

// getGlobalHTTPRouteSpecs returns one HTTPRoute per enabled HTTP endpoint for a global gateway config.
func (r *Reconciler) getGlobalHTTPRouteSpecs(nodeSet *appsv1.ChainNodeSet, gw appsv1.GlobalGatewayConfig) ([]*gwapiv1.HTTPRoute, error) {
	parentRef := gw.GetGatewayParentRef()
	svcName := gw.GetServiceName(nodeSet)

	type endpointDef struct {
		suffix string
		prefix string
		port   int32
	}

	var endpoints []endpointDef
	if gw.EnableRPC {
		endpoints = append(endpoints, endpointDef{"rpc", "rpc", chainutils.RpcPort})
	}
	if gw.EnableLCD {
		endpoints = append(endpoints, endpointDef{"lcd", "lcd", chainutils.LcdPort})
	}
	if gw.EnableEvmRPC {
		endpoints = append(endpoints, endpointDef{"evm-rpc", "evm-rpc", controllers.EvmRpcPort})
	}
	if gw.EnableEvmRpcWs {
		endpoints = append(endpoints, endpointDef{"evm-rpc-ws", "evm-rpc-ws", controllers.EvmRpcWsPort})
	}

	routes := make([]*gwapiv1.HTTPRoute, 0, len(endpoints))
	for _, ep := range endpoints {
		hostname := gwapiv1.Hostname(fmt.Sprintf("%s.%s", ep.prefix, gw.Host))
		port := gwapiv1.PortNumber(ep.port)
		route := &gwapiv1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("%s-%s", gw.GetName(nodeSet), ep.suffix),
				Namespace: nodeSet.GetNamespace(),
				Labels: WithChainNodeSetLabels(nodeSet, map[string]string{
					controllers.LabelChainNodeSet: nodeSet.GetName(),
					labelGlobalGateway:            gw.Name,
				}),
			},
			Spec: gwapiv1.HTTPRouteSpec{
				CommonRouteSpec: gwapiv1.CommonRouteSpec{
					ParentRefs: []gwapiv1.ParentReference{parentRef},
				},
				Hostnames: []gwapiv1.Hostname{hostname},
				Rules: []gwapiv1.HTTPRouteRule{
					{
						BackendRefs: []gwapiv1.HTTPBackendRef{
							{
								BackendRef: gwapiv1.BackendRef{
									BackendObjectReference: gwapiv1.BackendObjectReference{
										Name: gwapiv1.ObjectName(svcName),
										Port: &port,
									},
								},
							},
						},
					},
				},
			},
		}
		if err := controllerutil.SetControllerReference(nodeSet, route, r.Scheme); err != nil {
			return nil, err
		}
		routes = append(routes, route)
	}

	return routes, nil
}

func (r *Reconciler) getGlobalGRPCRouteSpec(nodeSet *appsv1.ChainNodeSet, gw appsv1.GlobalGatewayConfig) (*gwapiv1a2.GRPCRoute, error) {
	parentRef := gw.GetGatewayParentRef()
	svcName := gw.GetServiceName(nodeSet)
	hostname := gwapiv1.Hostname(fmt.Sprintf("grpc.%s", gw.Host))
	port := gwapiv1.PortNumber(chainutils.GrpcPort)

	route := &gwapiv1a2.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      gw.GetGrpcName(nodeSet),
			Namespace: nodeSet.GetNamespace(),
			Labels: WithChainNodeSetLabels(nodeSet, map[string]string{
				controllers.LabelChainNodeSet: nodeSet.GetName(),
				labelGlobalGateway:            gw.Name,
			}),
		},
		Spec: gwapiv1a2.GRPCRouteSpec{
			CommonRouteSpec: gwapiv1.CommonRouteSpec{
				ParentRefs: []gwapiv1.ParentReference{parentRef},
			},
			Hostnames: []gwapiv1.Hostname{hostname},
			Rules: []gwapiv1a2.GRPCRouteRule{
				{
					BackendRefs: []gwapiv1a2.GRPCBackendRef{
						{
							BackendRef: gwapiv1.BackendRef{
								BackendObjectReference: gwapiv1.BackendObjectReference{
									Name: gwapiv1.ObjectName(svcName),
									Port: &port,
								},
							},
						},
					},
				},
			},
		},
	}

	return route, controllerutil.SetControllerReference(nodeSet, route, r.Scheme)
}

func (r *Reconciler) listChainNodeSetGatewayRoutes(ctx context.Context, nodeSet *appsv1.ChainNodeSet, l ...string) ([]gwapiv1.HTTPRoute, []gwapiv1a2.GRPCRoute, error) {
	if len(l)%2 != 0 {
		return nil, nil, fmt.Errorf("list of labels must contain pairs of key-value")
	}

	selectorMap := map[string]string{controllers.LabelChainNodeSet: nodeSet.GetName()}
	for i := 0; i < len(l); i += 2 {
		selectorMap[l[i]] = l[i+1]
	}
	sel := labels.SelectorFromSet(selectorMap)

	httpList := &gwapiv1.HTTPRouteList{}
	if err := r.List(ctx, httpList, &client.ListOptions{
		Namespace:     nodeSet.GetNamespace(),
		LabelSelector: sel,
	}); err != nil {
		if controllers.IsCRDNotInstalled(err) {
			return nil, nil, nil
		}
		return nil, nil, err
	}

	grpcList := &gwapiv1a2.GRPCRouteList{}
	if err := r.List(ctx, grpcList, &client.ListOptions{
		Namespace:     nodeSet.GetNamespace(),
		LabelSelector: sel,
	}); err != nil {
		if controllers.IsCRDNotInstalled(err) {
			return httpList.Items, nil, nil
		}
		return nil, nil, err
	}

	return httpList.Items, grpcList.Items, nil
}

func (r *Reconciler) getCosmoseedHTTPRoute(nodeSet *appsv1.ChainNodeSet) (*gwapiv1.HTTPRoute, error) {
	gw := nodeSet.Spec.Cosmoseed.Gateway
	var namespace *gwapiv1.Namespace
	if gw.Gateway.Namespace != nil {
		ns := gwapiv1.Namespace(*gw.Gateway.Namespace)
		namespace = &ns
	}
	parentRef := gwapiv1.ParentReference{
		Name:      gwapiv1.ObjectName(gw.Gateway.Name),
		Namespace: namespace,
	}
	hostname := gwapiv1.Hostname(gw.Host)
	svcName := fmt.Sprintf("%s-seed", nodeSet.GetName())
	port := gwapiv1.PortNumber(cosmoseedHttpPort)

	route := &gwapiv1.HTTPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-seed", nodeSet.GetName()),
			Namespace: nodeSet.GetNamespace(),
			Labels: map[string]string{
				controllers.LabelApp:          controllers.CosmoseedName,
				controllers.LabelChainNodeSet: nodeSet.GetName(),
			},
		},
		Spec: gwapiv1.HTTPRouteSpec{
			CommonRouteSpec: gwapiv1.CommonRouteSpec{
				ParentRefs: []gwapiv1.ParentReference{parentRef},
			},
			Hostnames: []gwapiv1.Hostname{hostname},
			Rules: []gwapiv1.HTTPRouteRule{
				{
					BackendRefs: []gwapiv1.HTTPBackendRef{
						{
							BackendRef: gwapiv1.BackendRef{
								BackendObjectReference: gwapiv1.BackendObjectReference{
									Name: gwapiv1.ObjectName(svcName),
									Port: &port,
								},
							},
						},
					},
				},
			},
		},
	}

	return route, controllerutil.SetControllerReference(nodeSet, route, r.Scheme)
}
