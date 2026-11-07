package controllers

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// operatorAnnotations are the annotations the operator generated before Services recorded their
// managed keys; they are removed from an unrecorded Service that no longer wants them.
var operatorAnnotations = []string{AnnotationStateSyncTrustHeight, AnnotationStateSyncTrustHash}

// EnsureService creates svc or writes the fields Cosmopilot manages onto the live Service, leaving
// fields set by others in place. svc is left holding the live Service.
func EnsureService(ctx context.Context, c client.Client, svc *corev1.Service) error {
	logger := log.FromContext(ctx).WithValues("svc", svc.GetName())

	current := &corev1.Service{}
	err := c.Get(ctx, client.ObjectKeyFromObject(svc), current)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("creating service")
			svc.Annotations = withRecords(maps.Clone(svc.Annotations), svc)
			return c.Create(ctx, svc)
		}
		return fmt.Errorf("failed to get service %s: %w", svc.GetName(), err)
	}
	if err := RequireSameController(current, svc, "Service"); err != nil {
		return err
	}

	merged := withManagedFields(current, svc)
	if equality.Semantic.DeepEqual(merged, current) {
		*svc = *current
		return nil
	}

	logger.Info("updating service")
	if err := c.Update(ctx, merged); err != nil {
		return err
	}
	*svc = *merged
	return nil
}

// withManagedFields copies the fields the Service builders set from desired onto a copy of live. The
// API server drops type-dependent fields such as node ports when the type changes.
func withManagedFields(live, desired *corev1.Service) *corev1.Service {
	merged := live.DeepCopy()

	merged.Labels = withManagedKeys(merged.Labels, desired.Labels, recordedKeys(live, AnnotationManagedLabels, nil))
	merged.Annotations = withManagedKeys(merged.Annotations, desired.Annotations, recordedKeys(live, AnnotationManagedAnnotations, operatorAnnotations))
	merged.Annotations = withRecords(merged.Annotations, desired)

	for _, ref := range desired.OwnerReferences {
		i := slices.IndexFunc(merged.OwnerReferences, func(r metav1.OwnerReference) bool { return r.UID == ref.UID })
		if i < 0 {
			merged.OwnerReferences = append(merged.OwnerReferences, ref)
		} else {
			merged.OwnerReferences[i] = ref
		}
	}

	merged.Spec.Type = desired.Spec.Type
	if merged.Spec.Type == "" {
		merged.Spec.Type = corev1.ServiceTypeClusterIP
	}
	// The API server rejects these off a LoadBalancer instead of dropping them.
	if merged.Spec.Type != corev1.ServiceTypeLoadBalancer {
		merged.Spec.LoadBalancerSourceRanges = nil
		merged.Spec.LoadBalancerIP = ""
	}
	merged.Spec.PublishNotReadyAddresses = desired.Spec.PublishNotReadyAddresses
	merged.Spec.Selector = desired.Spec.Selector
	merged.Spec.Ports = make([]corev1.ServicePort, 0, len(desired.Spec.Ports))
	for _, want := range desired.Spec.Ports {
		port := want
		if i := slices.IndexFunc(live.Spec.Ports, func(p corev1.ServicePort) bool { return p.Name == want.Name }); i >= 0 {
			port = live.Spec.Ports[i]
			port.Name, port.Protocol, port.Port, port.TargetPort = want.Name, want.Protocol, want.Port, want.TargetPort
		}
		merged.Spec.Ports = append(merged.Spec.Ports, port)
	}
	return merged
}

// withManagedKeys sets the desired entries on live and drops the owned keys no longer desired.
func withManagedKeys(live, desired map[string]string, owned []string) map[string]string {
	for _, key := range owned {
		if _, wanted := desired[key]; !wanted {
			delete(live, key)
		}
	}
	for key, value := range desired {
		if live == nil {
			live = map[string]string{}
		}
		live[key] = value
	}
	return live
}

// recordedKeys returns the keys listed in the record annotation, or fallback when there is none.
func recordedKeys(svc *corev1.Service, record string, fallback []string) []string {
	keys, ok := svc.Annotations[record]
	if !ok {
		return fallback
	}
	return strings.Split(keys, ",")
}

// withRecords records in annotations the label and annotation keys desired sets, so keys it drops
// later can be told apart from keys added by others.
func withRecords(annotations map[string]string, desired *corev1.Service) map[string]string {
	for record, applied := range map[string]map[string]string{
		AnnotationManagedLabels:      desired.Labels,
		AnnotationManagedAnnotations: desired.Annotations,
	} {
		keys := slices.DeleteFunc(slices.Sorted(maps.Keys(applied)), func(key string) bool {
			return key == AnnotationManagedLabels || key == AnnotationManagedAnnotations
		})
		if len(keys) == 0 {
			delete(annotations, record)
			continue
		}
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[record] = strings.Join(keys, ",")
	}
	return annotations
}
