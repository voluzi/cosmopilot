package chainnode

import (
	"context"
	"fmt"

	v1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"
	gwapiv1a2 "sigs.k8s.io/gateway-api/apis/v1alpha2"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/chainutils"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
)

const (
	labelGatewayRoute = "gateway-route"
)

func (r *Reconciler) ensureGatewayRoutes(ctx context.Context, chainNode *appsv1.ChainNode) error {
	cfg := chainNode.Spec.Gateway
	if cfg == nil {
		return r.cleanupGatewayRoutes(ctx, chainNode)
	}

	// Build desired HTTPRoutes (one per enabled HTTP endpoint)
	desiredRoutes, err := r.getHTTPRouteSpecs(chainNode)
	if err != nil {
		return err
	}

	desiredNames := map[string]bool{}
	routesApplied := true
	for _, route := range desiredRoutes {
		desiredNames[route.Name] = true
		applied, err := controllers.EnsureHTTPRoute(ctx, r.Client, route)
		if err != nil {
			return err
		}
		if !applied {
			routesApplied = false
		}
	}

	// Build or delete GRPCRoute
	grpcRouteName := fmt.Sprintf("%s-grpc", chainNode.GetName())
	if cfg.EnableGRPC {
		grpcRoute, err := r.getGRPCRouteSpec(chainNode)
		if err != nil {
			return err
		}
		applied, err := controllers.EnsureGRPCRoute(ctx, r.Client, grpcRoute)
		if err != nil {
			return err
		}
		if !applied {
			routesApplied = false
		}
	} else {
		if err = r.Delete(ctx, &gwapiv1a2.GRPCRoute{
			ObjectMeta: metav1.ObjectMeta{
				Name:      grpcRouteName,
				Namespace: chainNode.GetNamespace(),
			},
		}); err != nil && !errors.IsNotFound(err) && !controllers.IsCRDNotInstalled(err) {
			return err
		}
	}

	// Cleanup stale HTTPRoutes (e.g. endpoint was disabled)
	existingRoutes, err := r.listChainNodeHTTPRoutes(ctx, chainNode)
	if err != nil {
		return err
	}
	for _, route := range existingRoutes {
		if !desiredNames[route.Name] {
			if err = r.Delete(ctx, &route); err != nil && !errors.IsNotFound(err) {
				return err
			}
		}
	}

	// Cross-cleanup: delete stale Ingress resources if switching to Gateway API.
	// Skip this when no routes were actually applied (e.g. Gateway API CRDs missing)
	// so we don't tear down working Ingress without a replacement.
	if !routesApplied {
		return nil
	}
	if err = r.Delete(ctx, &v1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      chainNode.GetName(),
			Namespace: chainNode.GetNamespace(),
		},
	}); err != nil && !errors.IsNotFound(err) {
		return err
	}
	if err = r.Delete(ctx, &v1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-grpc", chainNode.GetName()),
			Namespace: chainNode.GetNamespace(),
		},
	}); err != nil && !errors.IsNotFound(err) {
		return err
	}

	return nil
}

// getHTTPRouteSpecs returns one HTTPRoute per enabled HTTP endpoint (each has its own backend port).
func (r *Reconciler) getHTTPRouteSpecs(chainNode *appsv1.ChainNode) ([]*gwapiv1.HTTPRoute, error) {
	cfg := chainNode.Spec.Gateway
	parentRef := chainNode.GetGatewayParentRef()
	svcName := chainNode.GetServiceName()

	type endpointDef struct {
		suffix string
		prefix string
		port   int32
	}

	var endpoints []endpointDef
	if cfg.EnableRPC {
		endpoints = append(endpoints, endpointDef{"rpc", "rpc", chainutils.RpcPort})
	}
	if cfg.EnableLCD {
		endpoints = append(endpoints, endpointDef{"lcd", "lcd", chainutils.LcdPort})
	}
	if cfg.EnableEvmRPC {
		endpoints = append(endpoints, endpointDef{"evm-rpc", "evm-rpc", controllers.EvmRpcPort})
	}
	if cfg.EnableEvmRpcWs {
		endpoints = append(endpoints, endpointDef{"evm-rpc-ws", "evm-rpc-ws", controllers.EvmRpcWsPort})
	}

	routes := make([]*gwapiv1.HTTPRoute, 0, len(endpoints))
	for _, ep := range endpoints {
		hostname := gwapiv1.Hostname(fmt.Sprintf("%s.%s", ep.prefix, cfg.Host))
		port := gwapiv1.PortNumber(ep.port)
		route := &gwapiv1.HTTPRoute{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("%s-%s", chainNode.GetName(), ep.suffix),
				Namespace: chainNode.GetNamespace(),
				Labels: WithChainNodeLabels(chainNode, map[string]string{
					controllers.LabelChainNode: chainNode.GetName(),
					labelGatewayRoute:          "true",
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
		if err := controllerutil.SetControllerReference(chainNode, route, r.Scheme); err != nil {
			return nil, err
		}
		routes = append(routes, route)
	}

	return routes, nil
}

func (r *Reconciler) getGRPCRouteSpec(chainNode *appsv1.ChainNode) (*gwapiv1a2.GRPCRoute, error) {
	cfg := chainNode.Spec.Gateway
	parentRef := chainNode.GetGatewayParentRef()
	svcName := chainNode.GetServiceName()
	hostname := gwapiv1.Hostname(fmt.Sprintf("grpc.%s", cfg.Host))
	port := gwapiv1.PortNumber(chainutils.GrpcPort)

	route := &gwapiv1a2.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-grpc", chainNode.GetName()),
			Namespace: chainNode.GetNamespace(),
			Labels: WithChainNodeLabels(chainNode, map[string]string{
				controllers.LabelChainNode: chainNode.GetName(),
				labelGatewayRoute:          "true",
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
	return route, controllerutil.SetControllerReference(chainNode, route, r.Scheme)
}

func (r *Reconciler) listChainNodeHTTPRoutes(ctx context.Context, chainNode *appsv1.ChainNode) ([]gwapiv1.HTTPRoute, error) {
	list := &gwapiv1.HTTPRouteList{}
	err := r.List(ctx, list, &client.ListOptions{
		Namespace: chainNode.GetNamespace(),
		LabelSelector: labels.SelectorFromSet(map[string]string{
			controllers.LabelChainNode: chainNode.GetName(),
			labelGatewayRoute:          "true",
		}),
	})
	if err != nil {
		if controllers.IsCRDNotInstalled(err) {
			return nil, nil
		}
		return nil, err
	}
	return list.Items, nil
}

func (r *Reconciler) cleanupTCPRoute(ctx context.Context, chainNode *appsv1.ChainNode) error {
	err := r.Delete(ctx, &gwapiv1a2.TCPRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-p2p", chainNode.GetName()),
			Namespace: chainNode.GetNamespace(),
		},
	})
	if err != nil && !errors.IsNotFound(err) && !controllers.IsCRDNotInstalled(err) {
		return err
	}
	return nil
}

func (r *Reconciler) cleanupGatewayRoutes(ctx context.Context, chainNode *appsv1.ChainNode) error {
	routes, err := r.listChainNodeHTTPRoutes(ctx, chainNode)
	if err != nil {
		return err
	}
	for _, route := range routes {
		if err = r.Delete(ctx, &route); err != nil && !errors.IsNotFound(err) {
			return err
		}
	}

	if err = r.Delete(ctx, &gwapiv1a2.GRPCRoute{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-grpc", chainNode.GetName()),
			Namespace: chainNode.GetNamespace(),
		},
	}); err != nil && !errors.IsNotFound(err) && !controllers.IsCRDNotInstalled(err) {
		return err
	}

	return nil
}
