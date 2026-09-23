package controllers

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	appsv1 "github.com/voluzi/cosmopilot/v3/api/v1"
)

func TestCosmoGuardCondition(t *testing.T) {
	cases := []struct {
		name     string
		guards   []GuardState
		status   metav1.ConditionStatus
		reason   string
		contains []string
	}{
		{
			name:   "serving",
			guards: []GuardState{{Name: "node-cg", Serving: true}},
			status: metav1.ConditionTrue, reason: appsv1.ReasonCosmoGuardServing,
		},
		{
			name:   "not serving before the first flip",
			guards: []GuardState{{Name: "node-cg"}},
			status: metav1.ConditionFalse, reason: appsv1.ReasonCosmoGuardNotServing,
			contains: []string{"node-cg is not serving yet", "reach the node directly and are not filtered"},
		},
		{
			name:   "not serving after the flip",
			guards: []GuardState{{Name: "node-cg", Routed: true}},
			status: metav1.ConditionFalse, reason: appsv1.ReasonCosmoGuardNotServing,
			contains: []string{"node-cg is not serving; public API routes stay on the guard"},
		},
		{
			name:   "route that can never be guarded",
			guards: []GuardState{{Name: "a-cg", Serving: true}, {Name: "public", Bypassed: true}},
			status: metav1.ConditionFalse, reason: appsv1.ReasonCosmoGuardBypassed,
			contains: []string{"public route public also spans groups without CosmoGuard and is never filtered"},
		},
		{
			name:   "a guard that is not serving outranks a bypassed route",
			guards: []GuardState{{Name: "b-cg"}, {Name: "public", Bypassed: true}},
			status: metav1.ConditionFalse, reason: appsv1.ReasonCosmoGuardNotServing,
			contains: []string{"b-cg is not serving yet", "public route public"},
		},
		{
			name:   "config missing wins and every problem is listed",
			guards: []GuardState{{Name: "a-cg", ConfigMissing: true}, {Name: "b-cg"}, {Name: "c-cg", Serving: true}},
			status: metav1.ConditionFalse, reason: appsv1.ReasonCosmoGuardConfigMissing,
			contains: []string{"a-cg is enabled without a config ConfigMap", "b-cg is not serving yet"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CosmoGuardCondition(tc.guards, 7)
			require.NotNil(t, got)
			require.Equal(t, appsv1.ConditionCosmoGuardReady, got.Type)
			require.Equal(t, tc.status, got.Status)
			require.Equal(t, tc.reason, got.Reason)
			require.Equal(t, int64(7), got.ObservedGeneration)
			for _, want := range tc.contains {
				require.Contains(t, got.Message, want)
			}
			require.NotContains(t, got.Message, "c-cg", "serving guards are not reported")
		})
	}
	require.Nil(t, CosmoGuardCondition(nil, 1), "no managed guard means no condition")
}

func TestUpdateCosmoGuardConditionEventsOnlyOnTransitions(t *testing.T) {
	var conditions []metav1.Condition
	notServing := CosmoGuardCondition([]GuardState{{Name: "cg"}}, 1)
	flipped := CosmoGuardCondition([]GuardState{{Name: "cg", Routed: true}}, 1)
	missing := CosmoGuardCondition([]GuardState{{Name: "cg", ConfigMissing: true}}, 1)
	serving := CosmoGuardCondition([]GuardState{{Name: "cg", Serving: true}}, 1)

	steps := []struct {
		name      string
		desired   *metav1.Condition
		changed   bool
		eventType string
	}{
		{"first seen not serving", notServing, true, corev1.EventTypeWarning},
		{"still not serving", notServing, false, ""},
		{"same reason, new message", flipped, true, ""},
		{"new false reason", missing, true, corev1.EventTypeWarning},
		{"recovered", serving, true, corev1.EventTypeNormal},
		{"still serving", serving, false, ""},
		{"guard no longer managed", nil, true, ""},
		{"already removed", nil, false, ""},
		{"first seen serving", serving, true, ""},
	}
	for _, step := range steps {
		changed, eventType := UpdateCosmoGuardCondition(&conditions, step.desired)
		require.Equal(t, step.changed, changed, step.name)
		require.Equal(t, step.eventType, eventType, step.name)
		current := apimeta.FindStatusCondition(conditions, appsv1.ConditionCosmoGuardReady)
		if step.desired == nil {
			require.Nil(t, current, step.name)
		} else {
			require.Equal(t, step.desired.Reason, current.Reason, step.name)
			require.Equal(t, step.desired.Message, current.Message, step.name)
		}
	}
}
