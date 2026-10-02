package cosmosigner

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
)

func TestTargetNetworkPolicyAllowsOnlySignerOnPrivValPort(t *testing.T) {
	p := testParams()
	p.TargetSelector = map[string]string{"cosmosigner-target": p.Name}
	policy := p.TargetNetworkPolicy()
	if policy.Name != p.Name+"-privval" || policy.Namespace != p.Namespace || !reflect.DeepEqual(policy.Spec.PodSelector.MatchLabels, p.TargetSelector) {
		t.Fatalf("wrong target policy identity/selector: %#v", policy)
	}
	if len(policy.Spec.Ingress) != 2 {
		t.Fatalf("want two ingress rules, got %#v", policy.Spec.Ingress)
	}
	private := policy.Spec.Ingress[0]
	if len(private.From) != 1 || !reflect.DeepEqual(private.From[0].PodSelector, &metav1.LabelSelector{MatchLabels: p.selectorLabels()}) || private.From[0].NamespaceSelector != nil || private.From[0].IPBlock != nil {
		t.Fatalf("privval peers must be this signer's pods in the same namespace: %#v", private.From)
	}
	if len(private.Ports) != 1 || *private.Ports[0].Protocol != corev1.ProtocolTCP || *private.Ports[0].Port != intstr.FromInt32(26659) || private.Ports[0].EndPort != nil {
		t.Fatalf("private rule must allow only TCP privval: %#v", private.Ports)
	}
	public := policy.Spec.Ingress[1]
	if len(public.From) != 0 {
		t.Fatalf("other ports must accept all sources: %#v", public.From)
	}
	for _, tc := range []struct {
		protocol    corev1.Protocol
		first, last int32
	}{
		{corev1.ProtocolTCP, 1, 26658}, {corev1.ProtocolTCP, 26660, 65535},
		{corev1.ProtocolUDP, 1, 65535}, {corev1.ProtocolSCTP, 1, 65535},
	} {
		found := false
		for _, port := range public.Ports {
			if ptr.Deref(port.Protocol, "") == tc.protocol && port.Port != nil && *port.Port == intstr.FromInt32(tc.first) && ptr.Deref(port.EndPort, 0) == tc.last {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing public range %#v: %#v", tc, public.Ports)
		}
	}
	if len(public.Ports) != 4 {
		t.Fatalf("unexpected public ports: %#v", public.Ports)
	}
}
