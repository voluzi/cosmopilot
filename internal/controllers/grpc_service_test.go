package controllers

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/voluzi/cosmopilot/v4/internal/chainutils"
)

func TestGrpcOnlyService(t *testing.T) {
	grpc := corev1.ServicePort{Name: chainutils.GrpcPortName, Protocol: corev1.ProtocolTCP, Port: chainutils.GrpcPort, TargetPort: intstr.FromInt32(CosmoGuardGrpcPort)}
	backend := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "node", Namespace: "ns", Annotations: map[string]string{"backend": "only"}},
		Spec: corev1.ServiceSpec{
			Type:                     corev1.ServiceTypeLoadBalancer,
			Selector:                 map[string]string{"app": "guard"},
			PublishNotReadyAddresses: true,
			Ports: []corev1.ServicePort{
				{Name: chainutils.RpcPortName, Port: chainutils.RpcPort, TargetPort: intstr.FromInt32(chainutils.RpcPort)},
				grpc,
			},
		},
	}

	svc, err := GrpcOnlyService(backend, "node-grpc", map[string]string{"l": "v"}, map[string]string{"a": "b"})
	require.NoError(t, err)
	assert.Equal(t, "node-grpc", svc.Name)
	assert.Equal(t, "ns", svc.Namespace)
	assert.Equal(t, map[string]string{"l": "v"}, svc.Labels)
	assert.Equal(t, map[string]string{"a": "b"}, svc.Annotations)
	assert.Equal(t, corev1.ServiceTypeClusterIP, svc.Spec.Type)
	assert.Equal(t, backend.Spec.Selector, svc.Spec.Selector)
	assert.True(t, svc.Spec.PublishNotReadyAddresses)
	assert.Equal(t, []corev1.ServicePort{grpc}, svc.Spec.Ports)

	// A node port allocated on a NodePort/LoadBalancer backend is not carried over.
	backend.Spec.Ports[1].NodePort = 30090
	svc, err = GrpcOnlyService(backend, "node-grpc", nil, nil)
	require.NoError(t, err)
	assert.Equal(t, []corev1.ServicePort{grpc}, svc.Spec.Ports)

	backend.Spec.Ports = backend.Spec.Ports[:1]
	_, err = GrpcOnlyService(backend, "node-grpc", nil, nil)
	require.Error(t, err)
}
