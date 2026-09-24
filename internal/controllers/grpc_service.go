package controllers

import (
	"fmt"
	"maps"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/voluzi/cosmopilot/v4/internal/chainutils"
)

// GrpcOnlyService derives the Service a gRPC Ingress targets from the multi-port Service that backs
// the other API routes. It selects the same pods on the same target port, but exposes only gRPC, so
// ingress controllers that take the backend scheme from Service annotations (Traefik) can be told to
// speak h2c without also switching RPC/LCD, which only speak HTTP/1.1.
func GrpcOnlyService(backend *corev1.Service, name string, labels, annotations map[string]string) (*corev1.Service, error) {
	// Empty maps read back as nil; keep them nil so a no-op reconcile compares equal.
	if len(labels) == 0 {
		labels = nil
	}
	if len(annotations) == 0 {
		annotations = nil
	}
	for _, port := range backend.Spec.Ports {
		if port.Port != chainutils.GrpcPort {
			continue
		}
		return &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:        name,
				Namespace:   backend.GetNamespace(),
				Labels:      labels,
				Annotations: annotations,
			},
			Spec: corev1.ServiceSpec{
				Type:                     corev1.ServiceTypeClusterIP,
				Selector:                 maps.Clone(backend.Spec.Selector),
				PublishNotReadyAddresses: backend.Spec.PublishNotReadyAddresses,
				// Only the fields meaningful on a ClusterIP port: a NodePort copied from a
				// NodePort/LoadBalancer backend would be rejected.
				Ports: []corev1.ServicePort{{
					Name:        port.Name,
					Protocol:    port.Protocol,
					AppProtocol: port.AppProtocol,
					Port:        port.Port,
					TargetPort:  port.TargetPort,
				}},
			},
		}, nil
	}
	return nil, fmt.Errorf("service %q has no gRPC port %d", backend.GetName(), chainutils.GrpcPort)
}
