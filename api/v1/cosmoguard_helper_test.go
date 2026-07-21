package v1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"
)

func autoscaledConfig(res *corev1.ResourceRequirements) *Config {
	return &Config{
		CosmoGuard: &CosmoGuardConfig{
			Enable:      true,
			Autoscaling: &CosmoGuardAutoscalingConfig{Enable: true, MaxReplicas: 5},
			Resources:   res,
		},
	}
}

// TestGetCosmoGuardAutoscalingTargets verifies the default HPA metric follows the request the guard
// container actually has: defaulting to CPU when the container only requests memory would leave the
// HPA unable to compute utilization and unable to scale.
func TestGetCosmoGuardAutoscalingTargets(t *testing.T) {
	// No resources set -> defaults include a CPU request -> default CPU target.
	cpu, mem := autoscaledConfig(nil).GetCosmoGuardAutoscalingTargets()
	assert.Equal(t, DefaultCosmoGuardAutoscalingCPUTarget, *cpu)
	assert.Nil(t, mem)

	// Only a memory request -> default the metric to memory (no CPU request to measure).
	memOnly := &corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")}}
	cpu, mem = autoscaledConfig(memOnly).GetCosmoGuardAutoscalingTargets()
	assert.Nil(t, cpu)
	assert.Equal(t, DefaultCosmoGuardAutoscalingCPUTarget, *mem)

	// A CPU limit (Kubernetes copies it to the request) counts as a CPU request -> default CPU.
	cpuLimit := &corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")}}
	cpu, mem = autoscaledConfig(cpuLimit).GetCosmoGuardAutoscalingTargets()
	assert.Equal(t, DefaultCosmoGuardAutoscalingCPUTarget, *cpu)
	assert.Nil(t, mem)

	// Explicit user targets always win, regardless of requests.
	cfg := autoscaledConfig(memOnly)
	cfg.CosmoGuard.Autoscaling.TargetCPUUtilizationPercentage = ptr.To[int32](65)
	cpu, mem = cfg.GetCosmoGuardAutoscalingTargets()
	assert.Equal(t, int32(65), *cpu)
	assert.Nil(t, mem)
}
