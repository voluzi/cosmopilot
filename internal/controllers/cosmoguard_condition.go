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
	// Name identifies the guard, or the route Service it fronts, in messages.
	Name string
	// ConfigMissing is true when the guard is enabled without a config ConfigMap and so was not deployed.
	ConfigMissing bool
	// Serving is true when the guard is ready to take the traffic (for a route Service, its flip gate).
	Serving bool
	// Routed is true when the public routes already point at the guard (they stay there once flipped).
	Routed bool
	// Bypassed is true for a route that can never go through a guard because it also spans groups
	// without one.
	Bypassed bool
	// Route is true when the entry is a public route Service rather than a guard.
	Route bool
}

// CosmoGuardCondition summarises guard states into the CosmoGuardReady condition. It returns nil when
// no guard is managed, meaning the condition should be removed.
func CosmoGuardCondition(guards []GuardState, generation int64) *metav1.Condition {
	if len(guards) == 0 {
		return nil
	}
	var missing, unguardable, unflippedGuards, unflippedRoutes, stalledGuards, stalledRoutes []string
	for _, guard := range guards {
		switch {
		case guard.ConfigMissing:
			missing = append(missing, guard.Name)
		case guard.Bypassed:
			unguardable = append(unguardable, guard.Name)
		case guard.Serving:
			// Serving guards and switched routes are not a problem.
		case guard.Routed && guard.Route:
			stalledRoutes = append(stalledRoutes, guard.Name)
		case guard.Routed:
			stalledGuards = append(stalledGuards, guard.Name)
		case guard.Route:
			unflippedRoutes = append(unflippedRoutes, guard.Name)
		default:
			unflippedGuards = append(unflippedGuards, guard.Name)
		}
	}
	condition := &metav1.Condition{
		Type:               appsv1.ConditionCosmoGuardReady,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: generation,
	}
	var problems []string
	report := func(names []string, format string) {
		if len(names) > 0 {
			problems = append(problems, fmt.Sprintf(format, strings.Join(names, ", ")))
		}
	}
	report(missing, "CosmoGuard %s is enabled without a config ConfigMap and is not reconciled; public API traffic may not be filtered")
	report(unflippedGuards, "CosmoGuard %s is not filtering traffic yet; public API routes reach the node directly and are not filtered until it serves them")
	report(unflippedRoutes, "public route %s has not switched to its guard yet; it reaches the nodes directly and is not filtered")
	report(stalledGuards, "CosmoGuard %s is not serving; public API routes stay on the guard")
	report(stalledRoutes, "public route %s stays on its guard, which is not serving")
	report(unguardable, "public route %s also spans groups without CosmoGuard and is never filtered")
	switch {
	case len(missing) > 0:
		condition.Reason = appsv1.ReasonCosmoGuardConfigMissing
	case len(unflippedGuards)+len(unflippedRoutes)+len(stalledGuards)+len(stalledRoutes) > 0:
		condition.Reason = appsv1.ReasonCosmoGuardNotServing
	case len(unguardable) > 0:
		condition.Reason = appsv1.ReasonCosmoGuardBypassed
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
