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
// container actually has (a container the HPA can measure), and that an empty/all-zero resources
// block falls back to the default guard resources so the HPA never stalls with no request.
func TestGetCosmoGuardAutoscalingTargets(t *testing.T) {
	// No resources set -> defaults include a CPU request -> default CPU target.
	res, cpu, mem := autoscaledConfig(nil).GetCosmoGuardAutoscalingTargets()
	assert.Equal(t, DefaultCosmoGuardAutoscalingCPUTarget, *cpu)
	assert.Nil(t, mem)
	assert.False(t, res.Requests.Cpu().IsZero())

	// Only a memory request -> default the metric to memory (no CPU request to measure).
	memOnly := &corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("256Mi")}}
	_, cpu, mem = autoscaledConfig(memOnly).GetCosmoGuardAutoscalingTargets()
	assert.Nil(t, cpu)
	assert.Equal(t, DefaultCosmoGuardAutoscalingCPUTarget, *mem)

	// A CPU limit (Kubernetes copies it to the request) counts as a CPU request -> default CPU.
	cpuLimit := &corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")}}
	_, cpu, mem = autoscaledConfig(cpuLimit).GetCosmoGuardAutoscalingTargets()
	assert.Equal(t, DefaultCosmoGuardAutoscalingCPUTarget, *cpu)
	assert.Nil(t, mem)

	// A present-but-zero request counts as absent (HPA needs a positive request); with no other
	// positive request the resources fall back to defaults and the metric defaults to CPU.
	zeroCPU := &corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("0")}}
	res, cpu, mem = autoscaledConfig(zeroCPU).GetCosmoGuardAutoscalingTargets()
	assert.Equal(t, DefaultCosmoGuardAutoscalingCPUTarget, *cpu)
	assert.Nil(t, mem)
	assert.True(t, res.Requests.Cpu().Cmp(resource.MustParse(DefaultCosmoGuardCPU)) == 0, "injected default CPU request")

	// Explicit empty resources + autoscaling -> inject defaults so the HPA has a request to measure.
	res, cpu, mem = autoscaledConfig(&corev1.ResourceRequirements{}).GetCosmoGuardAutoscalingTargets()
	assert.Equal(t, DefaultCosmoGuardAutoscalingCPUTarget, *cpu)
	assert.Nil(t, mem)
	assert.False(t, res.Requests.Cpu().IsZero(), "default CPU request injected")
	assert.False(t, res.Requests.Memory().IsZero(), "default memory request injected")

	// Explicit user targets always win, regardless of requests.
	cfg := autoscaledConfig(memOnly)
	cfg.CosmoGuard.Autoscaling.TargetCPUUtilizationPercentage = ptr.To[int32](65)
	res, cpu, mem = cfg.GetCosmoGuardAutoscalingTargets()
	assert.Equal(t, int32(65), *cpu)
	assert.Nil(t, mem)
	// ...and the CPU request it measures is injected against the memory-only block so the HPA works.
	assert.False(t, res.Requests.Cpu().IsZero(), "CPU request injected for explicit CPU target")
	assert.False(t, res.Requests.Memory().IsZero(), "user memory request preserved")

	// Explicit CPU target against an empty block -> CPU request injected so the metric is measurable.
	cfgEmpty := autoscaledConfig(&corev1.ResourceRequirements{})
	cfgEmpty.CosmoGuard.Autoscaling.TargetCPUUtilizationPercentage = ptr.To[int32](70)
	res, cpu, mem = cfgEmpty.GetCosmoGuardAutoscalingTargets()
	assert.Equal(t, int32(70), *cpu)
	assert.Nil(t, mem)
	assert.False(t, res.Requests.Cpu().IsZero(), "CPU request injected for explicit CPU target on empty block")

	// Zero CPU request + a smaller positive CPU limit + explicit CPU target: the injected request must
	// be capped at the limit so requests.cpu never exceeds limits.cpu (which renders an invalid Pod).
	cappedLimit := resource.MustParse("100m")
	cfgCap := autoscaledConfig(&corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("0"), corev1.ResourceMemory: resource.MustParse("256Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: cappedLimit},
	})
	cfgCap.CosmoGuard.Autoscaling.TargetCPUUtilizationPercentage = ptr.To[int32](80)
	res, cpu, _ = cfgCap.GetCosmoGuardAutoscalingTargets()
	assert.Equal(t, int32(80), *cpu)
	assert.False(t, res.Requests.Cpu().IsZero(), "positive CPU request injected")
	assert.True(t, res.Requests.Cpu().Cmp(*res.Limits.Cpu()) <= 0, "injected CPU request must not exceed the CPU limit")
	assert.True(t, res.Requests.Cpu().Cmp(cappedLimit) == 0, "injected request capped at the smaller limit")
}
