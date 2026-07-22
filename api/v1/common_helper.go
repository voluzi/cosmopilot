package v1

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/kube-openapi/pkg/validation/strfmt"
	"k8s.io/utils/ptr"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/voluzi/cosmopilot/v2/internal/tmkms"
	"github.com/voluzi/cosmopilot/v2/pkg/dataexporter"
)

const (
	// DefaultReconcilePeriod is the default interval between reconciliation loops.
	DefaultReconcilePeriod = 15 * time.Second

	// DefaultImageVersion is the image tag used when none is specified.
	DefaultImageVersion = "latest"

	// DefaultBlockThreshold is the duration to wait for a block before marking the node unhealthy.
	DefaultBlockThreshold = "0s"

	// DefaultStartupTime is the time after which a node is restarted if it fails to start.
	DefaultStartupTime = time.Hour

	// DefaultNodeUtilsLogLevel is the log level for the node-utils container.
	DefaultNodeUtilsLogLevel = "info"

	// DefaultP2pExpose indicates whether to expose the P2P endpoint.
	DefaultP2pExpose = false

	// DefaultP2pServiceType is the default service type for exposing the P2P port.
	DefaultP2pServiceType = corev1.ServiceTypeNodePort

	// DefaultUnbondingTime is the default unbonding period for a validator.
	DefaultUnbondingTime = "1814400s"

	// DefaultVotingPeriod is the default duration of a voting period.
	DefaultVotingPeriod = "120h"

	// DefaultHDPath is the default derivation path for accounts.
	DefaultHDPath = "m/44'/118'/0'/0/0"

	// DefaultAccountPrefix is the default bech32 prefix for accounts.
	DefaultAccountPrefix = "cosmos"

	// DefaultValPrefix is the default bech32 prefix for validator operator accounts.
	DefaultValPrefix = "cosmosvaloper"

	// DefaultP2pPort is the default port used for P2P connections.
	DefaultP2pPort = 26656

	// DefaultStateSyncKeepRecent is the number of snapshots to keep for state sync.
	DefaultStateSyncKeepRecent = 2

	// DefaultSdkVersion is the default Cosmos SDK version.
	DefaultSdkVersion = V0_53

	// DefaultCommissionMaxChangeRate is the default maximum commission change rate.
	DefaultCommissionMaxChangeRate = "0.1"

	// DefaultCommissionMaxRate is the default maximum commission rate.
	DefaultCommissionMaxRate = "0.1"

	// DefaultCommissionRate is the default initial commission rate.
	DefaultCommissionRate = "0.1"

	// DefaultMinimumSelfDelegation is the default minimum self-delegation for validators.
	DefaultMinimumSelfDelegation = "1"

	// DefaultNodeUtilsCPU is the default CPU request for the node-utils container.
	DefaultNodeUtilsCPU = "300m"

	// DefaultNodeUtilsMemory is the default memory request for the node-utils container.
	DefaultNodeUtilsMemory = "100Mi"

	// DefaultCosmoGuardCPU is the default CPU request for the CosmoGuard container.
	DefaultCosmoGuardCPU = "200m"

	// DefaultCosmoGuardMemory is the default memory request for the CosmoGuard container.
	DefaultCosmoGuardMemory = "250Mi"

	// DefaultCosmoGuardDashboardPort is the default port the CosmoGuard dashboard listens on.
	DefaultCosmoGuardDashboardPort int32 = 8080

	// DefaultCosmoGuardAutoscalingCPUTarget is the default target CPU utilization for CosmoGuard autoscaling.
	DefaultCosmoGuardAutoscalingCPUTarget int32 = 80

	// DefaultVpaCooldown is the default cooldown period for VPA scaling actions.
	DefaultVpaCooldown = 5 * time.Minute

	// DefaultLimitPercentage is the default percentage used when applying limit-based scaling strategies.
	DefaultLimitPercentage = 150

	// DefaultSafetyMarginPercent is the default safety margin for VPA scale-down operations.
	DefaultSafetyMarginPercent = 15

	// DefaultHysteresisPercent is the default hysteresis for VPA scale-down thresholds.
	DefaultHysteresisPercent = 5

	// DefaultEmergencyScaleUpPercent is the default percentage to scale up on OOM detection.
	DefaultEmergencyScaleUpPercent = 25

	// DefaultMaxOOMRecoveries is the default maximum OOM recoveries allowed within the recovery window.
	DefaultMaxOOMRecoveries = 3

	// DefaultOOMRecoveryWindow is the default time window for counting OOM recoveries.
	DefaultOOMRecoveryWindow = 1 * time.Hour
)

// GetImage returns the versioned image to be used
func (app *AppSpec) GetImage() string {
	return fmt.Sprintf("%s:%s", app.Image, app.GetImageVersion())
}

// GetImageVersion returns the image version to be used
func (app *AppSpec) GetImageVersion() string {
	if app.Version != nil {
		return *app.Version
	}
	return DefaultImageVersion
}

// GetImagePullPolicy returns the pull policy to be used for the app image
func (app *AppSpec) GetImagePullPolicy() corev1.PullPolicy {
	if app.ImagePullPolicy != "" {
		return app.ImagePullPolicy
	}
	if app.Version != nil && *app.Version == DefaultImageVersion {
		return corev1.PullAlways
	}
	return corev1.PullIfNotPresent
}

func (app *AppSpec) GetSdkVersion() SdkVersion {
	if app.SdkVersion != nil {
		return *app.SdkVersion
	}
	return DefaultSdkVersion
}

func (app *AppSpec) ShouldQueryGovUpgrades() bool {
	if app.CheckGovUpgrades != nil {
		return *app.CheckGovUpgrades
	}
	return true
}

// UseGenesisSubcommand returns whether genesis commands should use the "genesis" subcommand
// (e.g., "genesis gentx" vs "gentx"). Defaults to true for sdkVersion >= v0.47.
func (app *AppSpec) UseGenesisSubcommand() bool {
	if app.SdkOptions != nil && app.SdkOptions.GenesisSubcommand != nil {
		return *app.SdkOptions.GenesisSubcommand
	}
	// Default based on SDK version: v0.47+ uses genesis subcommand
	switch app.GetSdkVersion() {
	case V0_45:
		return false
	default:
		return true
	}
}

// GenesisConfig helper methods

func (g *GenesisConfig) ShouldUseDataVolume() bool {
	if g != nil && g.UseDataVolume != nil {
		return *g.UseDataVolume
	}
	return false
}

func (g *GenesisConfig) ShouldDownloadUsingContainer() bool {
	if g != nil && g.ChainID != nil {
		return g.ShouldUseDataVolume()
	}
	return false
}

func (g *GenesisConfig) HasConfigMapSource() bool {
	if g != nil && g.ConfigMap != nil {
		return true
	}
	return false
}

func (g *GenesisConfig) GetConfigMapName(chainID string) string {
	if g != nil && g.ConfigMap != nil {
		return *g.ConfigMap
	}
	return fmt.Sprintf("%s-genesis", chainID)
}

// GetSidecarImagePullPolicy returns the pull policy to be used for the sidecar container image
func (cfg *Config) GetSidecarImagePullPolicy(name string) corev1.PullPolicy {
	if cfg == nil || cfg.Sidecars == nil {
		return corev1.PullIfNotPresent
	}

	for _, c := range cfg.Sidecars {
		if c.Name == name {
			if c.ImagePullPolicy != "" {
				return c.ImagePullPolicy
			}
			if c.Image != nil {
				parts := strings.Split(*c.Image, ":")
				if len(parts) == 1 || parts[1] == DefaultImageVersion {
					return corev1.PullAlways
				}
			}
			return corev1.PullIfNotPresent
		}
	}
	return corev1.PullIfNotPresent
}

func (cfg *Config) SeedModeEnabled() bool {
	if cfg != nil && cfg.SeedMode != nil {
		return *cfg.SeedMode
	}
	return false
}

func (cfg *Config) ShouldIgnoreSyncing() bool {
	if cfg != nil && cfg.IgnoreSyncing != nil {
		return *cfg.IgnoreSyncing
	}
	return false
}

func (cfg *Config) GetEnv() []corev1.EnvVar {
	if cfg != nil && cfg.Env != nil {
		return cfg.Env
	}
	return []corev1.EnvVar{}
}

func (cfg *Config) GetNodeUtilsResources() corev1.ResourceRequirements {
	if cfg != nil && cfg.NodeUtilsResources != nil {
		return *cfg.NodeUtilsResources
	}
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(DefaultNodeUtilsCPU),
			corev1.ResourceMemory: resource.MustParse(DefaultNodeUtilsMemory),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(DefaultNodeUtilsCPU),
			corev1.ResourceMemory: resource.MustParse(DefaultNodeUtilsMemory),
		},
	}
}

func (cfg *Config) IsEvmEnabled() bool {
	if cfg != nil && cfg.EvmEnabled != nil {
		return *cfg.EvmEnabled
	}
	return false
}

func (cfg *Config) UseDashedConfigToml() bool {
	if cfg != nil && cfg.DashedConfigToml != nil {
		return *cfg.DashedConfigToml
	}
	return false
}

func (cfg *Config) GetBlockThreshold() string {
	if cfg != nil && cfg.BlockThreshold != nil {
		return *cfg.BlockThreshold
	}
	return DefaultBlockThreshold
}

func (cfg *Config) GetStartupTime() time.Duration {
	if cfg != nil && cfg.StartupTime != nil {
		if d, err := strfmt.ParseDuration(*cfg.StartupTime); err == nil {
			return d
		}
	}
	return DefaultStartupTime
}

func (cfg *Config) GetNodeUtilsLogLevel() string {
	if cfg != nil && cfg.NodeUtilsLogLevel != nil {
		return *cfg.NodeUtilsLogLevel
	}
	return DefaultNodeUtilsLogLevel
}

func (cfg *Config) GetNodeUtilsEnv() []corev1.EnvVar {
	if cfg != nil {
		return cfg.NodeUtilsEnv
	}
	return nil
}

func (cfg *Config) ShouldPersistAddressBook() bool {
	if cfg != nil && cfg.PersistAddressBook != nil {
		return *cfg.PersistAddressBook
	}
	return true
}

func (cfg *Config) GetTerminationGracePeriodSeconds() *int64 {
	if cfg != nil {
		return cfg.TerminationGracePeriodSeconds
	}
	return nil
}

func (cfg *Config) CosmoGuardEnabled() bool {
	if cfg != nil && cfg.CosmoGuard != nil {
		return cfg.CosmoGuard.Enable
	}
	return false
}

func (cfg *Config) GetCosmoGuardConfig() *corev1.ConfigMapKeySelector {
	if cfg != nil && cfg.CosmoGuard != nil {
		return cfg.CosmoGuard.Config
	}
	return nil
}

func (cfg *Config) GetCosmoGuardResources() corev1.ResourceRequirements {
	if cfg != nil && cfg.CosmoGuard != nil && cfg.CosmoGuard.Resources != nil {
		return *cfg.CosmoGuard.Resources
	}
	return defaultCosmoGuardResources()
}

// defaultCosmoGuardResources returns the operator's default guard container resources (positive CPU
// and memory requests + matching limits).
func defaultCosmoGuardResources() corev1.ResourceRequirements {
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(DefaultCosmoGuardCPU),
			corev1.ResourceMemory: resource.MustParse(DefaultCosmoGuardMemory),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(DefaultCosmoGuardCPU),
			corev1.ResourceMemory: resource.MustParse(DefaultCosmoGuardMemory),
		},
	}
}

// GetCosmoGuardAutoscalingTargets resolves the guard container resources AND the HPA utilization
// targets together, keeping them consistent: an HPA can only measure a resource the container
// positively requests. Explicit user targets always win. When the user sets neither target, the
// default metric follows whichever positive request the container has (CPU preferred). It then
// guarantees every SELECTED metric — explicit or defaulted — has a positive matching request,
// injecting the operator default for any that is missing or zero (e.g. an empty resources block, or
// an explicit CPU target against a memory-only block). A namespace LimitRange might otherwise supply
// the request at admission, but we cannot see that here, so injecting a known request is the safe,
// self-contained choice. Returns the resources the container should use.
func (cfg *Config) GetCosmoGuardAutoscalingTargets() (resources corev1.ResourceRequirements, targetCPU, targetMemory *int32) {
	resources = cfg.GetCosmoGuardResources()
	as := cfg.GetCosmoGuardAutoscaling()
	if as == nil {
		return resources, nil, nil
	}
	targetCPU = as.TargetCPUUtilizationPercentage
	targetMemory = as.TargetMemoryUtilizationPercentage

	hasCPU := cosmoGuardRequests(resources, corev1.ResourceCPU)
	hasMemory := cosmoGuardRequests(resources, corev1.ResourceMemory)

	// Container requests neither CPU nor memory (empty/all-zero block): fall back to the full default
	// guard resources so it has sensible requests+limits rather than a single injected request.
	if !hasCPU && !hasMemory {
		resources = defaultCosmoGuardResources()
		hasCPU, hasMemory = true, true
	}

	// Default the metric when the user set neither: follow whichever positive request the container has.
	if targetCPU == nil && targetMemory == nil {
		if hasMemory && !hasCPU {
			targetMemory = ptr.To(DefaultCosmoGuardAutoscalingCPUTarget)
		} else {
			targetCPU = ptr.To(DefaultCosmoGuardAutoscalingCPUTarget)
		}
	}

	// Every selected metric needs a positive corresponding request or the HPA cannot scale.
	resources = ensureCosmoGuardRequests(resources, targetCPU != nil, targetMemory != nil)
	return resources, targetCPU, targetMemory
}

// ensureCosmoGuardRequests guarantees the guard container positively requests each resource its
// selected HPA metrics measure, injecting the operator default for any that is missing or zero. The
// input is not mutated.
func ensureCosmoGuardRequests(res corev1.ResourceRequirements, needCPU, needMemory bool) corev1.ResourceRequirements {
	injectCPU := needCPU && !cosmoGuardRequests(res, corev1.ResourceCPU)
	injectMemory := needMemory && !cosmoGuardRequests(res, corev1.ResourceMemory)
	if !injectCPU && !injectMemory {
		return res
	}
	defaults := defaultCosmoGuardResources()
	out := *res.DeepCopy()
	if out.Requests == nil {
		out.Requests = corev1.ResourceList{}
	}
	if injectCPU {
		out.Requests[corev1.ResourceCPU] = cosmoGuardInjectedRequest(res, corev1.ResourceCPU, defaults.Requests[corev1.ResourceCPU])
	}
	if injectMemory {
		out.Requests[corev1.ResourceMemory] = cosmoGuardInjectedRequest(res, corev1.ResourceMemory, defaults.Requests[corev1.ResourceMemory])
	}
	return out
}

// cosmoGuardInjectedRequest returns the request quantity to inject for a resource: the operator
// default, capped at a smaller positive configured limit so the resulting requests never exceed
// limits (which would render an invalid Pod).
func cosmoGuardInjectedRequest(res corev1.ResourceRequirements, name corev1.ResourceName, def resource.Quantity) resource.Quantity {
	if limit, ok := res.Limits[name]; ok && !limit.IsZero() && limit.Cmp(def) < 0 {
		return limit.DeepCopy()
	}
	return def
}

// cosmoGuardRequests reports whether the guard container effectively requests a positive amount of the
// given resource — via an explicit request, or a limit (Kubernetes copies a limit to the request when
// the request is unset). A present-but-zero quantity counts as absent: utilization-based autoscaling
// needs a positive request to measure against.
func cosmoGuardRequests(res corev1.ResourceRequirements, name corev1.ResourceName) bool {
	if request, ok := res.Requests[name]; ok {
		return !request.IsZero()
	}
	limit, ok := res.Limits[name]
	return ok && !limit.IsZero()
}

// GetCosmoGuardReplicas returns the desired CosmoGuard replica count. Defaults to 1.
func (cfg *Config) GetCosmoGuardReplicas() int32 {
	if cfg != nil && cfg.CosmoGuard != nil && cfg.CosmoGuard.Replicas != nil {
		return *cfg.CosmoGuard.Replicas
	}
	return 1
}

// GetCosmoGuardImage returns the CosmoGuard image, using the per-CR override when set,
// otherwise the provided operator-wide default.
func (cfg *Config) GetCosmoGuardImage(defaultImage string) string {
	if cfg != nil && cfg.CosmoGuard != nil && cfg.CosmoGuard.Image != nil && *cfg.CosmoGuard.Image != "" {
		return *cfg.CosmoGuard.Image
	}
	return defaultImage
}

// GetCosmoGuardAutoscaling returns the CosmoGuard autoscaling config, or nil.
func (cfg *Config) GetCosmoGuardAutoscaling() *CosmoGuardAutoscalingConfig {
	if cfg != nil && cfg.CosmoGuard != nil {
		return cfg.CosmoGuard.Autoscaling
	}
	return nil
}

// CosmoGuardAutoscalingEnabled reports whether horizontal autoscaling is enabled for CosmoGuard.
func (cfg *Config) CosmoGuardAutoscalingEnabled() bool {
	as := cfg.GetCosmoGuardAutoscaling()
	return as != nil && as.Enable
}

// GetCosmoGuardDashboard returns the CosmoGuard dashboard config, or nil.
func (cfg *Config) GetCosmoGuardDashboard() *CosmoGuardDashboardConfig {
	if cfg != nil && cfg.CosmoGuard != nil {
		return cfg.CosmoGuard.Dashboard
	}
	return nil
}

// CosmoGuardDashboardEnabled reports whether the CosmoGuard dashboard is enabled.
func (cfg *Config) CosmoGuardDashboardEnabled() bool {
	d := cfg.GetCosmoGuardDashboard()
	return d != nil && d.Enable
}

// GetCosmoGuardDashboardPort returns the configured dashboard port or the default.
func (cfg *Config) GetCosmoGuardDashboardPort() int32 {
	if d := cfg.GetCosmoGuardDashboard(); d != nil && d.Port != nil {
		return *d.Port
	}
	return DefaultCosmoGuardDashboardPort
}

// cosmoGuardReservedPorts are ports the guard uses regardless of EVM: the public API Service ports
// (RPC/LCD/gRPC), the metrics port, the always-bound olric cluster listener ports (bind/peer-API/
// gossip), the guard's own API listener container ports (RPC/LCD/gRPC listeners) and both the public
// EVM Service ports and the EVM listener ports. Every EVM port is reserved even for non-EVM groups: an
// EVM route (an individual node via the apiServiceName() flip, a per-group route, or a ChainNodeSet
// global route) retargets to the guard Service by port NUMBER regardless of any group's evmEnabled, so
// a dashboard bound to a public EVM Service port (8545/8546) would be served on the external EVM
// hostname, and one bound to an EVM listener port (18545/18546) would receive misrouted EVM traffic.
// The dashboard must not reuse any rendered port, or a Service would carry two entries for the same
// port (rejected by the API server) or the container would bind one port twice (crash-loop). These
// mirror values in internal/chainutils, internal/controllers and internal/cosmoguard, which api/v1
// cannot import (import cycle).
var cosmoGuardReservedPorts = map[int32]string{
	26657: "RPC",
	1317:  "LCD",
	9090:  "gRPC",
	9001:  "metrics",
	3320:  "cluster bind",
	3321:  "cluster peer API",
	3322:  "cluster gossip",
	16657: "RPC listener",
	11317: "LCD listener",
	19090: "gRPC listener",
	8545:  "EVM RPC",
	8546:  "EVM RPC WS",
	18545: "EVM RPC listener",
	18546: "EVM RPC WS listener",
}

// ValidateCosmoGuardDashboard checks the CosmoGuard dashboard config is renderable: its port must not
// collide with a port the guard already uses (Service or container listener — a duplicate Service port
// is rejected by the API server, and a duplicate container port crash-loops the pod), and when basic
// auth is configured both credential selectors must reference a Secret name and key (an empty name
// renders an unresolvable env var). Every EVM port (public Service ports 8545/8546 and listener ports
// 18545/18546) is reserved unconditionally in cosmoGuardReservedPorts, because an EVM route retargets to
// the guard Service by port number regardless of a group's evmEnabled. Returns nil when the dashboard is
// disabled.
func (cfg *Config) ValidateCosmoGuardDashboard() error {
	if !cfg.CosmoGuardDashboardEnabled() {
		return nil
	}
	port := cfg.GetCosmoGuardDashboardPort()
	if name, ok := cosmoGuardReservedPorts[port]; ok {
		return fmt.Errorf("cosmoGuard.dashboard.port %d collides with the guard's %s port; choose a different port", port, name)
	}

	if auth := cfg.GetCosmoGuardDashboard().BasicAuth; auth != nil {
		if auth.Username.Name == "" || auth.Username.Key == "" {
			return fmt.Errorf("cosmoGuard.dashboard.basicAuth.username must reference both a Secret name and key")
		}
		if auth.Password.Name == "" || auth.Password.Key == "" {
			return fmt.Errorf("cosmoGuard.dashboard.basicAuth.password must reference both a Secret name and key")
		}
	}
	return nil
}

func (cfg *Config) GetHaltHeight() int64 {
	if cfg != nil && cfg.HaltHeight != nil {
		return *cfg.HaltHeight
	}
	return 0
}

// GetSecurityContext returns the custom security context for the main app container if specified, or nil otherwise.
func (cfg *Config) GetSecurityContext() *corev1.SecurityContext {
	if cfg != nil {
		return cfg.SecurityContext
	}
	return nil
}

// GetPodSecurityContext returns the custom pod security context if specified, or nil otherwise.
func (cfg *Config) GetPodSecurityContext() *corev1.PodSecurityContext {
	if cfg != nil {
		return cfg.PodSecurityContext
	}
	return nil
}

// GetServiceAccountName returns the service account name if specified, or empty string otherwise.
func (cfg *Config) GetServiceAccountName() string {
	if cfg != nil && cfg.ServiceAccountName != nil {
		return *cfg.ServiceAccountName
	}
	return ""
}

func (exp *ExposeConfig) Enabled() bool {
	if exp != nil && exp.P2P != nil {
		return *exp.P2P
	}
	return DefaultP2pExpose
}

func (exp *ExposeConfig) GetServiceType() corev1.ServiceType {
	if exp != nil && exp.P2pServiceType != nil {
		return *exp.P2pServiceType
	}
	return DefaultP2pServiceType
}

func (exp *ExposeConfig) GetAnnotations() map[string]string {
	if exp != nil && exp.Annotations != nil {
		return exp.Annotations
	}
	return nil
}

func (exp *ExposeConfig) UsesGateway() bool {
	return exp != nil && exp.Gateway != nil
}

func (exp *ExposeConfig) GetGatewayParentRef() gwapiv1.ParentReference {
	ref := gwapiv1.ParentReference{
		Name: gwapiv1.ObjectName(exp.Gateway.Name),
		Port: ptr.To(gwapiv1.PortNumber(exp.GetGatewayPort())),
	}
	if exp.Gateway.Namespace != nil {
		ns := gwapiv1.Namespace(*exp.Gateway.Namespace)
		ref.Namespace = &ns
	}
	return ref
}

func (exp *ExposeConfig) GetGatewayPort() int32 {
	if exp.Gateway != nil && exp.Gateway.Port != nil {
		return *exp.Gateway.Port
	}
	return 26656
}

// TmKMS helper methods

func (kms *TmKMS) GetKeyFormat() *TmKmsKeyFormat {
	if kms.KeyFormat != nil {
		return kms.KeyFormat
	}
	return &TmKmsKeyFormat{
		Type:               tmkms.DefaultKeyType,
		AccountKeyPrefix:   tmkms.DefaultAccountPrefix,
		ConsensusKeyPrefix: tmkms.DefaultConsensusPrefix,
	}
}

func (kms *TmKMS) GetProtocolVersion() tmkms.ProtocolVersion {
	if kms.ValidatorProtocol != nil {
		return *kms.ValidatorProtocol
	}
	return tmkms.ProtocolVersionV0_34
}

func (kms *TmKMS) ShouldPersistState() bool {
	if kms != nil && kms.PersistState != nil {
		return *kms.PersistState
	}
	return true
}

func (kms *TmKMS) GetResources() corev1.ResourceRequirements {
	if kms != nil && kms.Resources != nil {
		return *kms.Resources
	}
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(tmkms.DefaultTmkmsCpu),
			corev1.ResourceMemory: resource.MustParse(tmkms.DefaultTmkmsMemory),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(tmkms.DefaultTmkmsCpu),
			corev1.ResourceMemory: resource.MustParse(tmkms.DefaultTmkmsMemory),
		},
	}
}

// StateSync methods

func (ss *StateSyncConfig) Enabled() bool {
	return ss != nil && ss.SnapshotInterval > 0
}

func (ss *StateSyncConfig) GetKeepRecent() int {
	if ss != nil && ss.SnapshotKeepRecent != nil {
		return *ss.SnapshotKeepRecent
	}
	return DefaultStateSyncKeepRecent
}

// GetGenesisFromRPCUrl transforms struct into url string

func (gg *FromNodeRPCConfig) GetGenesisFromRPCUrl() string {
	if gg == nil {
		return ""
	}

	port := 26657
	protocol := "http://"
	hostname := gg.Hostname

	if gg.Secure {
		protocol = "https://"
		port = 443
	}

	if gg.Port != nil {
		port = *gg.Port
	}

	return fmt.Sprintf("%s%s:%d/genesis", protocol, hostname, port)
}

// VolumeSnapshotsConfig helper methods

func (s *VolumeSnapshotsConfig) ShouldStopNode() bool {
	if s != nil && s.StopNode != nil {
		return *s.StopNode
	}
	return false
}

func (s *VolumeSnapshotsConfig) ShouldExportTarballs() bool {
	if s != nil && s.ExportTarball != nil {
		return true
	}
	return false
}

func (s *VolumeSnapshotsConfig) ShouldVerify() bool {
	if s != nil && s.Verify != nil {
		return *s.Verify
	}
	return false
}

func (s *VolumeSnapshotsConfig) ShouldDisableWhileSyncing() bool {
	if s != nil && s.DisableWhileSyncing != nil {
		return *s.DisableWhileSyncing
	}
	return true
}

func (s *VolumeSnapshotsConfig) ShouldDisableWhileUnhealthy() bool {
	if s != nil && s.DisableWhileUnhealthy != nil {
		return *s.DisableWhileUnhealthy
	}
	return true
}

func (s *VolumeSnapshotsConfig) ShouldPreserveLastSnapshot() bool {
	if s != nil && s.PreserveLastSnapshot != nil {
		return *s.PreserveLastSnapshot
	}
	return true
}

func (s *VolumeSnapshotsConfig) GetRetainCount() *int32 {
	if s != nil {
		return s.Retain
	}
	return nil
}

// ExportTarballConfig helper methods

func (e *ExportTarballConfig) GetSuffix() string {
	if e != nil && e.Suffix != nil {
		return *e.Suffix
	}
	return ""
}

func (e *ExportTarballConfig) DeleteWhenExpired() bool {
	if e != nil && e.DeleteOnExpire != nil {
		return *e.DeleteOnExpire
	}
	return false
}

func (e *ExportTarballConfig) GetCompression() dataexporter.Compression {
	if e != nil && e.Compression != nil {
		return dataexporter.Compression(*e.Compression)
	}
	return dataexporter.CompressionGzip
}

// Validate ensures one destination and a supported compression format are configured.
func (e *ExportTarballConfig) Validate(path string) error {
	if e == nil {
		return nil
	}
	switch {
	case e.GCS != nil && e.S3 != nil:
		return fmt.Errorf("%s: gcs and s3 are mutually exclusive", path)
	case e.GCS == nil && e.S3 == nil:
		return fmt.Errorf("%s: one of gcs or s3 must be set", path)
	}
	if _, err := dataexporter.ParseCompression(string(e.GetCompression())); err != nil {
		return fmt.Errorf("%s.compression: %w", path, err)
	}
	if e.GCS != nil {
		return e.GCS.Validate(path + ".gcs")
	}
	return e.S3.Validate(path + ".s3")
}

// GcsExporter helper methods

// Validate ensures exactly one authentication method is configured for uploading to GCS: either a
// credentials secret (`credentialsSecret`) or a Kubernetes ServiceAccount for Workload Identity / ADC
// (`serviceAccountName`). Having both set or neither set is rejected. path is the field path reported
// in the returned error.
func (gcs *GcsExportConfig) Validate(path string) error {
	if gcs == nil {
		return nil
	}
	hasCredentialsSecret := gcs.CredentialsSecret != nil
	hasServiceAccount := gcs.ServiceAccountName != nil
	switch {
	case hasCredentialsSecret && hasServiceAccount:
		return fmt.Errorf("%s: credentialsSecret and serviceAccountName are mutually exclusive", path)
	case !hasCredentialsSecret && !hasServiceAccount:
		return fmt.Errorf("%s: one of credentialsSecret or serviceAccountName must be set", path)
	case hasServiceAccount && *gcs.ServiceAccountName == "":
		return fmt.Errorf("%s.serviceAccountName must not be empty", path)
	}
	return nil
}

func (gcs *GcsExportConfig) GetSizeLimit() string {
	if gcs != nil && gcs.SizeLimit != nil {
		return *gcs.SizeLimit
	}
	return dataexporter.DefaultSizeLimit
}

func (gcs *GcsExportConfig) GetPartSize() string {
	if gcs != nil && gcs.PartSize != nil {
		return *gcs.PartSize
	}
	return dataexporter.DefaultPartSize
}

func (gcs *GcsExportConfig) GetChunkSize() string {
	if gcs != nil && gcs.ChunkSize != nil {
		return *gcs.ChunkSize
	}
	return dataexporter.DefaultChunkSize
}

func (gcs *GcsExportConfig) GetBufferSize() string {
	if gcs != nil && gcs.BufferSize != nil {
		return *gcs.BufferSize
	}
	return dataexporter.DefaultBufferSize
}

func (gcs *GcsExportConfig) GetConcurrentJobs() int {
	if gcs != nil && gcs.ConcurrentJobs != nil {
		return *gcs.ConcurrentJobs
	}
	return dataexporter.DefaultConcurrentJobs
}

func (s3 *S3ExportConfig) Validate(path string) error {
	if s3 == nil {
		return nil
	}
	if s3.Bucket == "" {
		return fmt.Errorf("%s.bucket must not be empty", path)
	}
	if s3.Region == "" {
		return fmt.Errorf("%s.region must not be empty", path)
	}
	if s3.CredentialsSecret != nil && s3.ServiceAccountName != nil {
		return fmt.Errorf("%s: credentialsSecret and serviceAccountName are mutually exclusive", path)
	}
	if s3.ServiceAccountName != nil && *s3.ServiceAccountName == "" {
		return fmt.Errorf("%s.serviceAccountName must not be empty", path)
	}
	if s3.CredentialsSecret != nil && s3.CredentialsSecret.Name == "" {
		return fmt.Errorf("%s.credentialsSecret.name must not be empty", path)
	}
	if s3.Endpoint != nil {
		endpoint, err := url.ParseRequestURI(*s3.Endpoint)
		if err != nil || endpoint.Host == "" {
			return fmt.Errorf("%s.endpoint is invalid", path)
		}
		if endpoint.Scheme != "http" && endpoint.Scheme != "https" {
			return fmt.Errorf("%s.endpoint must use http or https", path)
		}
	}
	if err := dataexporter.ValidateS3UploadOptions(
		dataexporter.WithChunkSize(s3.GetChunkSize()),
		dataexporter.WithPartSize(s3.GetPartSize()),
		dataexporter.WithSizeLimit(s3.GetSizeLimit()),
		dataexporter.WithBufferSize(s3.GetBufferSize()),
		dataexporter.WithConcurrentUploadJobs(s3.GetConcurrentJobs()),
	); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func (s3 *S3ExportConfig) GetEndpoint() string {
	if s3 != nil && s3.Endpoint != nil {
		return *s3.Endpoint
	}
	return ""
}

func (s3 *S3ExportConfig) ShouldForcePathStyle() bool {
	return s3 != nil && s3.ForcePathStyle != nil && *s3.ForcePathStyle
}

func (s3 *S3ExportConfig) GetSizeLimit() string {
	if s3 != nil && s3.SizeLimit != nil {
		return *s3.SizeLimit
	}
	return dataexporter.DefaultSizeLimit
}

func (s3 *S3ExportConfig) GetPartSize() string {
	if s3 != nil && s3.PartSize != nil {
		return *s3.PartSize
	}
	return dataexporter.DefaultPartSize
}

func (s3 *S3ExportConfig) GetChunkSize() string {
	if s3 != nil && s3.ChunkSize != nil {
		return *s3.ChunkSize
	}
	return dataexporter.DefaultS3ChunkSize
}

func (s3 *S3ExportConfig) GetBufferSize() string {
	if s3 != nil && s3.BufferSize != nil {
		return *s3.BufferSize
	}
	return dataexporter.DefaultBufferSize
}

func (s3 *S3ExportConfig) GetConcurrentJobs() int {
	if s3 != nil && s3.ConcurrentJobs != nil {
		return *s3.ConcurrentJobs
	}
	return dataexporter.DefaultConcurrentJobs
}

// Upgrade helper methods

func (u *UpgradeSpec) GetVersion() string {
	if parts := strings.Split(u.Image, ":"); len(parts) == 2 {
		return parts[1]
	}
	return DefaultImageVersion
}

func (u *UpgradeSpec) ForceGovUpgrade() bool {
	if u != nil && u.ForceOnChain != nil {
		return *u.ForceOnChain
	}
	return false
}

func (u *Upgrade) GetVersion() string {
	if parts := strings.Split(u.Image, ":"); len(parts) == 2 {
		return parts[1]
	}
	return DefaultImageVersion
}

// Sidecar helper methods

func (s *SidecarSpec) ShouldRestartPodOnFailure() bool {
	if s.RestartPodOnFailure != nil {
		return *s.RestartPodOnFailure
	}
	return false
}

func (s *SidecarSpec) ShouldRunBeforeNode() bool {
	if s.RunBeforeNode != nil {
		return *s.RunBeforeNode
	}
	return false
}

func (s *SidecarSpec) GetImage(chainNode *ChainNode) string {
	if s.Image != nil {
		return *s.Image
	}
	return chainNode.GetAppImage()
}

func (s *SidecarSpec) DeferUntilHealthyEnabled() bool {
	if s != nil && s.DeferUntilHealthy != nil {
		return *s.DeferUntilHealthy
	}
	return false
}

// VolumeSpec methods

func (v *VolumeSpec) ShouldDeleteWithNode() bool {
	if v.DeleteWithNode != nil {
		return *v.DeleteWithNode
	}
	return false
}

// GetStorageClass returns the storage class for this volume.
// If not explicitly set, it falls back to the provided fallback value (typically the main persistence storage class).
// If the fallback is also nil, the cluster default storage class will be used.
func (v *VolumeSpec) GetStorageClass(fallback *string) *string {
	if v.StorageClassName != nil {
		return v.StorageClassName
	}
	return fallback
}

// Vertical Pod Autoscaling

func (vpa *VerticalAutoscalingConfig) IsEnabled() bool {
	return vpa != nil && vpa.Enabled
}

func (vpam *VerticalAutoscalingMetricConfig) GetCooldownDuration() time.Duration {
	if vpam != nil && vpam.Cooldown != nil {
		if d, err := strfmt.ParseDuration(*vpam.Cooldown); err == nil {
			return d
		}
	}
	return DefaultVpaCooldown
}

func (vpam *VerticalAutoscalingMetricConfig) GetLimitUpdateStrategy() LimitUpdateStrategy {
	if vpam != nil && vpam.LimitStrategy != nil {
		return *vpam.LimitStrategy
	}
	return LimitRetain
}

func (vpam *VerticalAutoscalingMetricConfig) GetLimitPercentage() int {
	if vpam != nil && vpam.LimitPercentage != nil {
		return *vpam.LimitPercentage
	}
	return DefaultLimitPercentage
}

func (vpam *VerticalAutoscalingMetricConfig) GetSafetyMarginPercent() int {
	if vpam != nil && vpam.SafetyMarginPercent != nil {
		return *vpam.SafetyMarginPercent
	}
	return DefaultSafetyMarginPercent
}

func (vpam *VerticalAutoscalingMetricConfig) GetHysteresisPercent() int {
	if vpam != nil && vpam.HysteresisPercent != nil {
		return *vpam.HysteresisPercent
	}
	return DefaultHysteresisPercent
}

func (vpam *VerticalAutoscalingMetricConfig) GetEmergencyScaleUpPercent() int {
	if vpam != nil && vpam.EmergencyScaleUpPercent != nil {
		return *vpam.EmergencyScaleUpPercent
	}
	return DefaultEmergencyScaleUpPercent
}

func (vpam *VerticalAutoscalingMetricConfig) GetMaxOOMRecoveries() int {
	if vpam != nil && vpam.MaxOOMRecoveries != nil {
		return *vpam.MaxOOMRecoveries
	}
	return DefaultMaxOOMRecoveries
}

func (vpam *VerticalAutoscalingMetricConfig) GetOOMRecoveryWindow() time.Duration {
	if vpam != nil && vpam.OOMRecoveryWindow != nil {
		if d, err := strfmt.ParseDuration(*vpam.OOMRecoveryWindow); err == nil {
			return d
		}
	}
	return DefaultOOMRecoveryWindow
}

func (vpar *VerticalAutoscalingRule) GetDuration() time.Duration {
	if vpar != nil && vpar.Duration != nil {
		if d, err := strfmt.ParseDuration(*vpar.Duration); err == nil {
			return d
		}
	}
	return DefaultVpaCooldown
}

// GetCooldownDuration returns the cooldown duration for this rule.
// If the rule has a cooldown specified, it uses that.
// Otherwise, it falls back to the provided metricCooldown.
func (vpar *VerticalAutoscalingRule) GetCooldownDuration(metricCooldown time.Duration) time.Duration {
	if vpar != nil && vpar.Cooldown != nil {
		if d, err := strfmt.ParseDuration(*vpar.Cooldown); err == nil {
			return d
		}
	}
	return metricCooldown
}

// Peer helper methods

func (peer *Peer) String() string {
	if peer == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%s@%s:%d", peer.ID, peer.Address, peer.GetPort())
}

func (peer *Peer) GetPort() int {
	if peer.Port != nil {
		return *peer.Port
	}
	return DefaultP2pPort
}

func (peer *Peer) IsUnconditional() bool {
	if peer.Unconditional != nil {
		return *peer.Unconditional
	}
	return false
}

func (peer *Peer) IsPrivate() bool {
	if peer.Private != nil {
		return *peer.Private
	}
	return false
}

func (peer *Peer) IsSeed() bool {
	if peer.Seed != nil {
		return *peer.Seed
	}
	return false
}

func (p PeerList) String() string {
	stringPeers := make([]string, len(p))
	for i, peer := range p {
		stringPeers[i] = peer.String()
	}
	return strings.Join(stringPeers, ",")
}

func (p PeerList) ExcludeSeeds() PeerList {
	l := make(PeerList, 0)
	for _, peer := range p {
		if !peer.IsSeed() {
			l = append(l, peer)
		}
	}
	return l
}

func (p PeerList) Append(l PeerList) PeerList {
	return append(p, l...)
}

const (
	defaultSubdomainRPC      = "rpc"
	defaultSubdomainGRPC     = "grpc"
	defaultSubdomainLCD      = "lcd"
	defaultSubdomainEvmRPC   = "evm-rpc"
	defaultSubdomainEvmRpcWs = "evm-rpc-ws"
)

// GetRPC returns the RPC subdomain prefix or the default ("rpc") when unset.
// Safe to call on a nil receiver.
func (s *SubdomainsConfig) GetRPC() string {
	if s != nil && s.RPC != nil && *s.RPC != "" {
		return *s.RPC
	}
	return defaultSubdomainRPC
}

// GetGRPC returns the gRPC subdomain prefix or the default ("grpc") when unset.
// Safe to call on a nil receiver.
func (s *SubdomainsConfig) GetGRPC() string {
	if s != nil && s.GRPC != nil && *s.GRPC != "" {
		return *s.GRPC
	}
	return defaultSubdomainGRPC
}

// GetLCD returns the LCD subdomain prefix or the default ("lcd") when unset.
// Safe to call on a nil receiver.
func (s *SubdomainsConfig) GetLCD() string {
	if s != nil && s.LCD != nil && *s.LCD != "" {
		return *s.LCD
	}
	return defaultSubdomainLCD
}

// GetEvmRPC returns the EVM RPC subdomain prefix or the default ("evm-rpc") when unset.
// Safe to call on a nil receiver.
func (s *SubdomainsConfig) GetEvmRPC() string {
	if s != nil && s.EvmRPC != nil && *s.EvmRPC != "" {
		return *s.EvmRPC
	}
	return defaultSubdomainEvmRPC
}

// GetEvmRpcWs returns the EVM RPC WS subdomain prefix or the default ("evm-rpc-ws") when unset.
// Safe to call on a nil receiver.
func (s *SubdomainsConfig) GetEvmRpcWs() string {
	if s != nil && s.EvmRpcWs != nil && *s.EvmRpcWs != "" {
		return *s.EvmRpcWs
	}
	return defaultSubdomainEvmRpcWs
}

// ValidateSubdomainPrefixes returns an error if any enabled endpoint resolves
// to an invalid DNS label (RFC 1123) or if two enabled endpoints share the
// same prefix, which would produce two routes or ingresses for the same
// hostname but different backend ports — ambiguous at runtime.
func ValidateSubdomainPrefixes(path string, sub *SubdomainsConfig, enableRPC, enableGRPC, enableLCD, enableEvmRPC, enableEvmRpcWs bool) error {
	type ep struct{ name, prefix string }
	endpoints := make([]ep, 0, 5)
	if enableRPC {
		endpoints = append(endpoints, ep{"rpc", sub.GetRPC()})
	}
	if enableGRPC {
		endpoints = append(endpoints, ep{"grpc", sub.GetGRPC()})
	}
	if enableLCD {
		endpoints = append(endpoints, ep{"lcd", sub.GetLCD()})
	}
	if enableEvmRPC {
		endpoints = append(endpoints, ep{"evmRPC", sub.GetEvmRPC()})
	}
	if enableEvmRpcWs {
		endpoints = append(endpoints, ep{"evmRpcWS", sub.GetEvmRpcWs()})
	}
	seen := make(map[string]string, len(endpoints))
	for _, e := range endpoints {
		if errs := validation.IsDNS1123Label(e.prefix); len(errs) > 0 {
			return fmt.Errorf("%s.subdomains.%s: %q is not a valid DNS label: %s", path, e.name, e.prefix, strings.Join(errs, "; "))
		}
		if prev, ok := seen[e.prefix]; ok {
			return fmt.Errorf("%s.subdomains: prefix %q is used by both %q and %q endpoints", path, e.prefix, prev, e.name)
		}
		seen[e.prefix] = e.name
	}
	return nil
}
