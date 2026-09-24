package chainnode

import (
	"context"
	"fmt"

	"github.com/banzaicloud/k8s-objectmatcher/patch"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	appsv1 "github.com/voluzi/cosmopilot/v4/api/v1"
	"github.com/voluzi/cosmopilot/v4/internal/chainutils"
	"github.com/voluzi/cosmopilot/v4/internal/controllers"
	"github.com/voluzi/cosmopilot/v4/internal/cosmoguard"
)

func (r *Reconciler) ensureIngresses(ctx context.Context, chainNode *appsv1.ChainNode) error {
	if chainNode.Spec.Ingress == nil {
		// Remove only the Ingresses this ChainNode owns.
		return r.deleteOwnedIngresses(ctx, chainNode)
	}

	// Resolve the API backend once (readiness-gated for a standalone/individual guard) so all
	// ingress rules point at the same Service and stay on the raw node until the guard is serving.
	apiSvcName := r.apiServiceName(ctx, chainNode)

	ingress, err := r.getIngressSpec(chainNode, apiSvcName)
	if err != nil {
		return err
	}

	if err = r.ensureIngress(ctx, ingress); err != nil {
		return err
	}

	grpcIngress, err := r.getGrpcIngressSpec(chainNode)
	if err != nil {
		return err
	}

	if !chainNode.Spec.Ingress.EnableGRPC {
		if _, err = controllers.DeleteIfControlledBy(ctx, r.Client, grpcIngress, chainNode); err != nil {
			return err
		}
		return r.deleteGrpcService(ctx, chainNode)
	}

	// The gRPC Service must exist before the Ingress targets it.
	if err = r.ensureGrpcService(ctx, chainNode, apiSvcName); err != nil {
		return err
	}
	return r.ensureIngress(ctx, grpcIngress)
}

// ensureGrpcService applies the gRPC-only Service the gRPC Ingress targets. It mirrors the pods and
// target port of apiSvcName, so it follows the same internal/CosmoGuard routing as the other routes.
func (r *Reconciler) ensureGrpcService(ctx context.Context, chainNode *appsv1.ChainNode, apiSvcName string) error {
	backend := &corev1.Service{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: chainNode.GetNamespace(), Name: apiSvcName}, backend); err != nil {
		return fmt.Errorf("failed to get gRPC backend service %s: %w", apiSvcName, err)
	}
	svc, err := controllers.GrpcOnlyService(backend, grpcName(chainNode), WithChainNodeLabels(chainNode), chainNode.GetGrpcServiceAnnotations())
	if err != nil {
		return err
	}
	// ApplyOwned tracks the last-applied state, so fields the backend drops (publishNotReadyAddresses
	// after useInternalServices is reverted, the Traefik annotation after a class change) are removed too.
	return cosmoguard.ApplyOwned(ctx, r.Client, r.Scheme, chainNode, svc)
}

func (r *Reconciler) deleteGrpcService(ctx context.Context, chainNode *appsv1.ChainNode) error {
	svc := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: grpcName(chainNode), Namespace: chainNode.GetNamespace()}}
	_, err := controllers.DeleteIfControlledBy(ctx, r.Client, svc, chainNode)
	return err
}

// grpcName names both the gRPC Ingress and the gRPC-only Service behind it.
func grpcName(chainNode *appsv1.ChainNode) string {
	return fmt.Sprintf("%s-grpc", chainNode.GetName())
}

// deleteOwnedIngresses removes the API and gRPC Ingresses of this ChainNode and the gRPC-only
// Service, leaving same-name objects that belong to someone else in place.
func (r *Reconciler) deleteOwnedIngresses(ctx context.Context, chainNode *appsv1.ChainNode) error {
	for _, name := range []string{chainNode.GetName(), grpcName(chainNode)} {
		ingress := &v1.Ingress{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: chainNode.GetNamespace()}}
		if _, err := controllers.DeleteIfControlledBy(ctx, r.Client, ingress, chainNode); err != nil {
			return err
		}
	}
	return r.deleteGrpcService(ctx, chainNode)
}

func (r *Reconciler) ensureIngress(ctx context.Context, ingress *v1.Ingress) error {
	logger := log.FromContext(ctx)

	currentIg := &v1.Ingress{}
	err := r.Get(ctx, client.ObjectKeyFromObject(ingress), currentIg)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("creating ingress", "ingress", ingress.GetName())
			return r.Create(ctx, ingress)
		}
		return err
	}
	if err := controllers.RequireSameController(currentIg, ingress, "Ingress"); err != nil {
		return err
	}

	patchResult, err := patch.DefaultPatchMaker.Calculate(currentIg, ingress)
	if err != nil {
		return err
	}

	if !patchResult.IsEmpty() {
		logger.Info("updating ingress", "ingress", ingress.GetName())

		ingress.ObjectMeta.ResourceVersion = currentIg.ObjectMeta.ResourceVersion
		if err := r.Update(ctx, ingress); err != nil {
			return err
		}
	}

	*ingress = *currentIg
	return nil
}

func (r *Reconciler) getIngressSpec(chainNode *appsv1.ChainNode, apiSvcName string) (*v1.Ingress, error) {
	ingress := &v1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:        chainNode.GetName(),
			Namespace:   chainNode.GetNamespace(),
			Labels:      WithChainNodeLabels(chainNode),
			Annotations: chainNode.Spec.Ingress.Annotations,
		},
		Spec: v1.IngressSpec{
			IngressClassName: ptr.To(chainNode.GetIngressClass()),
			Rules:            make([]v1.IngressRule, 0),
		},
	}

	if !chainNode.Spec.Ingress.DisableTLS {
		ingress.Spec.TLS = []v1.IngressTLS{
			{
				Hosts:      []string{},
				SecretName: chainNode.GetIngressSecretName(),
			},
		}
	}

	pathType := v1.PathTypeImplementationSpecific

	if chainNode.Spec.Ingress.EnableRPC {
		host := fmt.Sprintf("%s.%s", chainNode.Spec.Ingress.Subdomains.GetRPC(), chainNode.Spec.Ingress.Host)
		if ingress.Spec.TLS != nil {
			ingress.Spec.TLS[0].Hosts = append(ingress.Spec.TLS[0].Hosts, host)
		}
		ingress.Spec.Rules = append(ingress.Spec.Rules, v1.IngressRule{
			Host: host,
			IngressRuleValue: v1.IngressRuleValue{
				HTTP: &v1.HTTPIngressRuleValue{
					Paths: []v1.HTTPIngressPath{
						{
							PathType: &pathType,
							Backend: v1.IngressBackend{
								Service: &v1.IngressServiceBackend{
									Name: apiSvcName,
									Port: v1.ServiceBackendPort{
										Number: chainutils.RpcPort,
									},
								},
								Resource: nil,
							},
						},
					},
				},
			},
		})
	}

	if chainNode.Spec.Ingress.EnableLCD {
		host := fmt.Sprintf("%s.%s", chainNode.Spec.Ingress.Subdomains.GetLCD(), chainNode.Spec.Ingress.Host)
		if ingress.Spec.TLS != nil {
			ingress.Spec.TLS[0].Hosts = append(ingress.Spec.TLS[0].Hosts, host)
		}
		ingress.Spec.Rules = append(ingress.Spec.Rules, v1.IngressRule{
			Host: host,
			IngressRuleValue: v1.IngressRuleValue{
				HTTP: &v1.HTTPIngressRuleValue{
					Paths: []v1.HTTPIngressPath{
						{
							PathType: &pathType,
							Backend: v1.IngressBackend{
								Service: &v1.IngressServiceBackend{
									Name: apiSvcName,
									Port: v1.ServiceBackendPort{
										Number: chainutils.LcdPort,
									},
								},
								Resource: nil,
							},
						},
					},
				},
			},
		})
	}

	if chainNode.Spec.Ingress.EnableEvmRPC {
		host := fmt.Sprintf("%s.%s", chainNode.Spec.Ingress.Subdomains.GetEvmRPC(), chainNode.Spec.Ingress.Host)
		if ingress.Spec.TLS != nil {
			ingress.Spec.TLS[0].Hosts = append(ingress.Spec.TLS[0].Hosts, host)
		}
		ingress.Spec.Rules = append(ingress.Spec.Rules, v1.IngressRule{
			Host: host,
			IngressRuleValue: v1.IngressRuleValue{
				HTTP: &v1.HTTPIngressRuleValue{
					Paths: []v1.HTTPIngressPath{
						{
							PathType: &pathType,
							Backend: v1.IngressBackend{
								Service: &v1.IngressServiceBackend{
									Name: apiSvcName,
									Port: v1.ServiceBackendPort{
										Number: controllers.EvmRpcPort,
									},
								},
								Resource: nil,
							},
						},
					},
				},
			},
		})
	}

	if chainNode.Spec.Ingress.EnableEvmRpcWs {
		host := fmt.Sprintf("%s.%s", chainNode.Spec.Ingress.Subdomains.GetEvmRpcWs(), chainNode.Spec.Ingress.Host)
		if ingress.Spec.TLS != nil {
			ingress.Spec.TLS[0].Hosts = append(ingress.Spec.TLS[0].Hosts, host)
		}
		ingress.Spec.Rules = append(ingress.Spec.Rules, v1.IngressRule{
			Host: host,
			IngressRuleValue: v1.IngressRuleValue{
				HTTP: &v1.HTTPIngressRuleValue{
					Paths: []v1.HTTPIngressPath{
						{
							PathType: &pathType,
							Backend: v1.IngressBackend{
								Service: &v1.IngressServiceBackend{
									Name: apiSvcName,
									Port: v1.ServiceBackendPort{
										Number: controllers.EvmRpcWsPort,
									},
								},
								Resource: nil,
							},
						},
					},
				},
			},
		})
	}

	if chainNode.Spec.Ingress.EnableGRPC && !chainNode.Spec.Ingress.DisableTLS {
		// We just append the hostname to TLS config and add no rule as it will be handled by a separate ingress
		// but will use the same certificate
		ingress.Spec.TLS[0].Hosts = append(ingress.Spec.TLS[0].Hosts, fmt.Sprintf("%s.%s", chainNode.Spec.Ingress.Subdomains.GetGRPC(), chainNode.Spec.Ingress.Host))
	}

	return ingress, controllerutil.SetControllerReference(chainNode, ingress, r.Scheme)
}

func (r *Reconciler) getGrpcIngressSpec(chainNode *appsv1.ChainNode) (*v1.Ingress, error) {
	pathType := v1.PathTypeImplementationSpecific
	ingress := &v1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:        grpcName(chainNode),
			Namespace:   chainNode.GetNamespace(),
			Labels:      WithChainNodeLabels(chainNode),
			Annotations: chainNode.GetGrpcAnnotations(),
		},
		Spec: v1.IngressSpec{
			IngressClassName: ptr.To(chainNode.GetIngressClass()),
			Rules: []v1.IngressRule{
				{
					Host: fmt.Sprintf("%s.%s", chainNode.Spec.Ingress.Subdomains.GetGRPC(), chainNode.Spec.Ingress.Host),
					IngressRuleValue: v1.IngressRuleValue{
						HTTP: &v1.HTTPIngressRuleValue{Paths: []v1.HTTPIngressPath{
							{
								PathType: &pathType,
								Backend: v1.IngressBackend{
									Service: &v1.IngressServiceBackend{
										Name: grpcName(chainNode),
										Port: v1.ServiceBackendPort{
											Number: chainutils.GrpcPort,
										},
									},
								},
							},
						}},
					},
				},
			},
		},
	}
	if !chainNode.Spec.Ingress.DisableTLS {
		ingress.Spec.TLS = []v1.IngressTLS{
			{
				Hosts:      []string{fmt.Sprintf("%s.%s", chainNode.Spec.Ingress.Subdomains.GetGRPC(), chainNode.Spec.Ingress.Host)},
				SecretName: chainNode.GetIngressSecretName(),
			},
		}
	}
	return ingress, controllerutil.SetControllerReference(chainNode, ingress, r.Scheme)
}
