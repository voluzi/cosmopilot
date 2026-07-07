package k8s

import (
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PreserveImmutableStatefulSetFields copies the fields Kubernetes forbids updating (selector,
// serviceName, podManagementPolicy, volumeClaimTemplates) from an existing StatefulSet onto the
// desired one, so a changed PVC template (e.g. size/storageClass) does not wedge the reconcile
// loop with a rejected update. Both arguments must be *appsv1.StatefulSet; otherwise it is a no-op.
func PreserveImmutableStatefulSetFields(desired, existing client.Object) {
	d, ok := desired.(*appsv1.StatefulSet)
	if !ok {
		return
	}
	e, ok := existing.(*appsv1.StatefulSet)
	if !ok {
		return
	}
	d.Spec.Selector = e.Spec.Selector
	d.Spec.ServiceName = e.Spec.ServiceName
	d.Spec.PodManagementPolicy = e.Spec.PodManagementPolicy
	d.Spec.VolumeClaimTemplates = e.Spec.VolumeClaimTemplates
}

const (
	// NonRootUID is the standard non-root user ID used across all pods
	NonRootUID = 1000
)

// RestrictedSecurityContext returns a container security context that complies
// with the Kubernetes PodSecurity "restricted" profile.
func RestrictedSecurityContext() *corev1.SecurityContext {
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr.To(false),
		RunAsNonRoot:             ptr.To(true),
		RunAsUser:                ptr.To[int64](NonRootUID),
		RunAsGroup:               ptr.To[int64](NonRootUID),
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
		Capabilities: &corev1.Capabilities{
			Drop: []corev1.Capability{"ALL"},
		},
	}
}

// RestrictedPodSecurityContext returns a pod security context that complies
// with the Kubernetes PodSecurity "restricted" profile.
func RestrictedPodSecurityContext() *corev1.PodSecurityContext {
	return &corev1.PodSecurityContext{
		RunAsNonRoot: ptr.To(true),
		RunAsUser:    ptr.To[int64](NonRootUID),
		RunAsGroup:   ptr.To[int64](NonRootUID),
		FSGroup:      ptr.To[int64](NonRootUID),
		SeccompProfile: &corev1.SeccompProfile{
			Type: corev1.SeccompProfileTypeRuntimeDefault,
		},
	}
}
