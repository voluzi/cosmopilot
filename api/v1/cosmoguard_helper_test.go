package v1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"
)

// TestValidateCosmoGuardDashboard verifies the dashboard port is rejected when it collides with a
// port the guard Service already exposes (which would render an invalid duplicate-port Service).
func TestValidateCosmoGuardDashboard(t *testing.T) {
	dash := func(enable bool, port *int32) *Config {
		return &Config{CosmoGuard: &CosmoGuardConfig{
			Enable:    true,
			Dashboard: &CosmoGuardDashboardConfig{Enable: enable, Port: port},
		}}
	}

	assert.NoError(t, dash(false, ptr.To[int32](26657)).ValidateCosmoGuardDashboard(), "disabled dashboard is never invalid")
	assert.NoError(t, dash(true, nil).ValidateCosmoGuardDashboard(), "default port 8080 does not collide")
	assert.NoError(t, dash(true, ptr.To[int32](8090)).ValidateCosmoGuardDashboard(), "non-colliding explicit port ok")

	err := dash(true, ptr.To[int32](26657)).ValidateCosmoGuardDashboard()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "collides")
	assert.Error(t, dash(true, ptr.To[int32](9001)).ValidateCosmoGuardDashboard(), "metrics port collision rejected")
	assert.Error(t, dash(true, ptr.To[int32](9090)).ValidateCosmoGuardDashboard(), "gRPC port collision rejected")
	// Always-bound olric cluster listener ports must be rejected too (container-level collision).
	assert.Error(t, dash(true, ptr.To[int32](3320)).ValidateCosmoGuardDashboard(), "cluster bind port collision rejected")
	assert.Error(t, dash(true, ptr.To[int32](3322)).ValidateCosmoGuardDashboard(), "cluster gossip port collision rejected")

	// The guard's own API listener container ports must be rejected (container binds them too).
	assert.Error(t, dash(true, ptr.To[int32](16657)).ValidateCosmoGuardDashboard(), "RPC listener port collision rejected")
	assert.Error(t, dash(true, ptr.To[int32](11317)).ValidateCosmoGuardDashboard(), "LCD listener port collision rejected")
	assert.Error(t, dash(true, ptr.To[int32](19090)).ValidateCosmoGuardDashboard(), "gRPC listener port collision rejected")

	// The public EVM Service ports are reserved even without EVM: an EVM route (individual/group/global)
	// retargets to the guard Service by port number, so a dashboard bound to 8545/8546 would be served on
	// the external EVM hostname. Regression for S2n5O.
	assert.Error(t, dash(true, ptr.To[int32](8545)).ValidateCosmoGuardDashboard(), "8545 EVM RPC Service port reserved even without EVM")
	assert.Error(t, dash(true, ptr.To[int32](8546)).ValidateCosmoGuardDashboard(), "8546 EVM RPC WS Service port reserved even without EVM")
	evm := dash(true, ptr.To[int32](8545))
	evm.EvmEnabled = ptr.To(true)
	assert.Error(t, evm.ValidateCosmoGuardDashboard(), "8545 collides with EVM enabled too")

	// The EVM LISTENER ports are reserved even without EVM: a flipped global route Service targets them
	// regardless of the group's evmEnabled, so a dashboard there would receive misrouted EVM traffic.
	assert.Error(t, dash(true, ptr.To[int32](18545)).ValidateCosmoGuardDashboard(), "18545 EVM listener reserved even without EVM")
	assert.Error(t, dash(true, ptr.To[int32](18546)).ValidateCosmoGuardDashboard(), "18546 EVM listener reserved even without EVM")
}

// TestValidateCosmoGuardDashboardBasicAuth verifies basic-auth selectors must reference both a Secret
// name and a key, so the guard's auth env vars resolve.
func TestValidateCosmoGuardDashboardBasicAuth(t *testing.T) {
	withAuth := func(auth *CosmoGuardDashboardAuth) *Config {
		return &Config{CosmoGuard: &CosmoGuardConfig{
			Enable:    true,
			Dashboard: &CosmoGuardDashboardConfig{Enable: true, Port: ptr.To[int32](8090), BasicAuth: auth},
		}}
	}
	sel := func(name, key string) corev1.SecretKeySelector {
		return corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: name}, Key: key}
	}

	// Both selectors fully specified -> ok.
	assert.NoError(t, withAuth(&CosmoGuardDashboardAuth{Username: sel("creds", "user"), Password: sel("creds", "pass")}).ValidateCosmoGuardDashboard())

	// Missing Secret name (only key set) -> rejected.
	err := withAuth(&CosmoGuardDashboardAuth{Username: sel("", "user"), Password: sel("creds", "pass")}).ValidateCosmoGuardDashboard()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "username")

	// Missing key on the password selector -> rejected.
	assert.Error(t, withAuth(&CosmoGuardDashboardAuth{Username: sel("creds", "user"), Password: sel("creds", "")}).ValidateCosmoGuardDashboard())

	// No basic auth (no-auth internal dashboard) -> ok.
	assert.NoError(t, withAuth(nil).ValidateCosmoGuardDashboard())
}

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
