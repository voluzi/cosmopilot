package controllers

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	appsv1 "github.com/voluzi/cosmopilot/v3/api/v1"
)

// GuardState is the observed state of one CosmoGuard deployment fronting public API routes.
type GuardState struct {
	// Name identifies the guard in messages: its deployment name, or the group it fronts.
	Name string
	// ConfigMissing is true when the guard is enabled without a config ConfigMap and so was not deployed.
	ConfigMissing bool
	// Serving is true when the guard has ready replicas for its current generation.
	Serving bool
	// Routed is true when the public routes already point at the guard (they stay there once flipped).
	Routed bool
}

// CosmoGuardCondition summarises guard states into the CosmoGuardReady condition. It returns nil when
// no guard is managed, meaning the condition should be removed.
func CosmoGuardCondition(guards []GuardState, generation int64) *metav1.Condition {
	if len(guards) == 0 {
		return nil
	}
	var missing, bypassed, stalled []string
	for _, guard := range guards {
		switch {
		case guard.ConfigMissing:
			missing = append(missing, guard.Name)
		case guard.Serving:
		case guard.Routed:
			stalled = append(stalled, guard.Name)
		default:
			bypassed = append(bypassed, guard.Name)
		}
	}
	condition := &metav1.Condition{
		Type:               appsv1.ConditionCosmoGuardReady,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: generation,
	}
	var problems []string
	if len(missing) > 0 {
		problems = append(problems, fmt.Sprintf("CosmoGuard %s is enabled without a config ConfigMap and is not deployed; public API traffic is not filtered",
			strings.Join(missing, ", ")))
	}
	if len(bypassed) > 0 {
		problems = append(problems, fmt.Sprintf("CosmoGuard %s is not serving yet; public API routes reach the node directly and are not filtered until it is ready",
			strings.Join(bypassed, ", ")))
	}
	if len(stalled) > 0 {
		problems = append(problems, fmt.Sprintf("CosmoGuard %s is not serving; public API routes stay on the guard",
			strings.Join(stalled, ", ")))
	}
	switch {
	case len(missing) > 0:
		condition.Reason = appsv1.ReasonCosmoGuardConfigMissing
	case len(problems) > 0:
		condition.Reason = appsv1.ReasonCosmoGuardNotServing
	default:
		condition.Status = metav1.ConditionTrue
		condition.Reason = appsv1.ReasonCosmoGuardServing
		problems = append(problems, "CosmoGuard is serving public API routes")
	}
	condition.Message = strings.Join(problems, "; ")
	return condition
}

// UpdateCosmoGuardCondition sets conditions to desired (removing the condition when desired is nil). It
// reports whether anything changed and the event type to record for a transition: Warning when the
// condition becomes false for a new reason, Normal when it recovers, "" otherwise.
func UpdateCosmoGuardCondition(conditions *[]metav1.Condition, desired *metav1.Condition) (bool, string) {
	previous := apimeta.FindStatusCondition(*conditions, appsv1.ConditionCosmoGuardReady)
	if desired == nil {
		return apimeta.RemoveStatusCondition(conditions, appsv1.ConditionCosmoGuardReady), ""
	}
	eventType := ""
	switch {
	case desired.Status == metav1.ConditionFalse && (previous == nil || previous.Reason != desired.Reason):
		eventType = corev1.EventTypeWarning
	case desired.Status == metav1.ConditionTrue && previous != nil && previous.Status == metav1.ConditionFalse:
		eventType = corev1.EventTypeNormal
	}
	return apimeta.SetStatusCondition(conditions, *desired), eventType
}
