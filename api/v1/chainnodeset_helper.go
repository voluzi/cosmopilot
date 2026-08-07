package v1

import (
	"fmt"
	"sort"
	"strings"

	"github.com/goccy/go-json"
	"github.com/voluzi/cosmoseed/pkg/cosmoseed"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"

	"github.com/voluzi/cosmopilot/v2/pkg/utils"
)

const (
	// DefaultGroupInstances is the default number of nodes in a group.
	DefaultGroupInstances = 1

	// DefaultCosmoseedLogLevel is the log level used by Cosmoseed when none is specified.
	DefaultCosmoseedLogLevel = "info"

	// DefaultCosmoseedAddrBookFile is the path to the Cosmoseed address book file.
	DefaultCosmoseedAddrBookFile = "data/addrbook.json"

	// DefaultIngressClass is the default ingress class name.
	DefaultIngressClass = "nginx"

	// ReservedValidatorGroupName is the group name reserved for the legacy
	// singleton .spec.validator. It cannot be used as a node group name.
	ReservedValidatorGroupName = "validator"
)

func (nodeSet *ChainNodeSet) GetNamespacedName() string {
	return types.NamespacedName{Namespace: nodeSet.GetNamespace(), Name: nodeSet.GetName()}.String()
}

// RecordedChildMatch describes how a ChainNode matches a ChainNodeSet's recorded child status.
// Ordered by increasing confidence that the object is the recorded child, so the strongest match
// across all status entries wins.
type RecordedChildMatch int

const (
	// RecordedChildNone means no status entry uses this child's name.
	RecordedChildNone RecordedChildMatch = iota
	// RecordedChildReplaced means the name is recorded against a different, known UID. This object
	// is provably not the recorded child — it only reuses a name a previous child held.
	RecordedChildReplaced
	// RecordedChildUnverified means the name is recorded with no UID, as written before the status
	// UID field existed. The object can be neither confirmed nor ruled out, so callers deciding
	// whether a ChainNodeSet still owns a child must treat it as owned.
	RecordedChildUnverified
	// RecordedChildExact means name and UID both match.
	RecordedChildExact
)

// MatchRecordedChild reports how child matches this ChainNodeSet's recorded node and validator
// status. Both the parent cleanup path and standalone ChainNode orphan detection authorize on this,
// and must agree.
func (nodeSet *ChainNodeSet) MatchRecordedChild(child *ChainNode) RecordedChildMatch {
	match := RecordedChildNone
	consider := func(name string, uid types.UID) {
		if name != child.GetName() {
			return
		}
		candidate := RecordedChildReplaced
		switch {
		case uid == "":
			candidate = RecordedChildUnverified
		case uid == child.GetUID():
			candidate = RecordedChildExact
		}
		if candidate > match {
			match = candidate
		}
	}
	for _, status := range nodeSet.Status.Nodes {
		consider(status.Name, status.UID)
	}
	for _, status := range nodeSet.Status.Validators {
		consider(status.Name, status.UID)
	}
	return match
}

// ClaimsChild reports whether this ChainNodeSet's status still claims child. An unverified record
// counts: a pre-upgrade status carries no UID, and treating that as unclaimed would let a child
// finalize outside its parent's root during the upgrade window.
func (nodeSet *ChainNodeSet) ClaimsChild(child *ChainNode) bool {
	switch nodeSet.MatchRecordedChild(child) {
	case RecordedChildExact, RecordedChildUnverified:
		return true
	default:
		return false
	}
}

// RecordedChildIdentity reports whether child's name appears in this ChainNodeSet's status, and
// whether a recorded UID proves it is this exact object.
func (nodeSet *ChainNodeSet) RecordedChildIdentity(child *ChainNode) (nameRecorded, uidMatches bool) {
	match := nodeSet.MatchRecordedChild(child)
	return match != RecordedChildNone, match == RecordedChildExact
}

// GeneratedValidatorNodeName returns the name of the child ChainNode this ChainNodeSet generates for
// one instance of a validator group. ReservedValidatorGroupName is the legacy singleton
// .spec.validator (rejected as a real group name by the webhook), which is a single node with no
// ordinal suffix. Callers outside the controller use it to resolve a recorded validator group back to
// the child that carries its consensus identity.
func (nodeSet *ChainNodeSet) GeneratedValidatorNodeName(group string, index int) string {
	if group == ReservedValidatorGroupName {
		return fmt.Sprintf("%s-validator", nodeSet.GetName())
	}
	return fmt.Sprintf("%s-%s-%d", nodeSet.GetName(), group, index)
}

func (nodeSet *ChainNodeSet) HasValidator() bool {
	if nodeSet.Spec.Validator != nil {
		return true
	}
	for _, group := range nodeSet.Spec.Nodes {
		if group.Validator != nil && group.GetInstances() > 0 {
			return true
		}
	}
	return false
}

func (nodeSet *ChainNodeSet) ShouldInitGenesis() bool {
	if nodeSet.Spec.Validator != nil && nodeSet.Spec.Validator.Init != nil {
		return true
	}
	for _, group := range nodeSet.Spec.Nodes {
		if group.Validator != nil && group.Validator.Init != nil && group.GetInstances() > 0 {
			return true
		}
	}
	return false
}

func (nodeSet *ChainNodeSet) GetLastUpgradeVersion() string {
	version := nodeSet.Spec.App.GetImageVersion()
	var h int64 = 0
	for _, u := range nodeSet.Status.Upgrades {
		if (u.Status == UpgradeCompleted || u.Status == UpgradeSkipped) && u.Height > h && u.Height <= nodeSet.Status.LatestHeight {
			h = u.Height
			version = u.GetVersion()
		}
	}
	return version
}

func (nodeSet *ChainNodeSet) GetAppSpecWithUpgrades() AppSpec {
	spec := nodeSet.Spec.App.DeepCopy()

	for _, u := range nodeSet.Status.Upgrades {
		upgradeSpec := UpgradeSpec{
			Height: u.Height,
			Image:  u.Image,
		}
		if u.Source == OnChainUpgrade {
			upgradeSpec.ForceOnChain = ptr.To(true)
		}

		if !utils.SliceContainsObj(spec.Upgrades, upgradeSpec, func(a UpgradeSpec, b UpgradeSpec) bool {
			return a.Height == b.Height
		}) {
			spec.Upgrades = append(spec.Upgrades, upgradeSpec)
		}
	}

	// Sort upgrades by height
	sort.Slice(spec.Upgrades, func(i, j int) bool {
		return spec.Upgrades[i].Height < spec.Upgrades[j].Height
	})

	return *spec
}

func (nodeSet *ChainNodeSet) GetValidatorMinimumGasPrices() string {
	validator := nodeSet.Spec.Validator
	if validator == nil {
		for _, group := range nodeSet.Spec.Nodes {
			// Skip zero-instance validator groups: they run no validator, so their gas price
			// must not shadow that of a later group that actually runs validators.
			if group.Validator != nil && group.GetInstances() > 0 {
				validator = group.Validator
				break
			}
		}
	}
	if validator != nil && validator.Config != nil && validator.Config.Override != nil {
		cfgOverride := *validator.Config.Override
		if cfgRaw, ok := cfgOverride["app.toml"]; ok {
			var cfg map[string]interface{}
			if err := json.Unmarshal(cfgRaw.Raw, &cfg); err != nil {
				return ""
			}
			if price, ok := cfg["minimum-gas-prices"]; ok {
				return price.(string)
			}
		}
	}
	return ""
}

// Node group methods

func (group *NodeGroupSpec) GetInstances() int {
	if group.Instances != nil {
		return *group.Instances
	}
	return DefaultGroupInstances
}

func (group *NodeGroupSpec) GetServiceName(owner client.Object) string {
	return fmt.Sprintf("%s-%s", owner.GetName(), group.Name)
}

// GetServiceConfig returns the Config that determines which endpoints (EVM, CosmoGuard) this
// group's pods expose. For a validator group the pods are configured from the validator config,
// so the group Services must build their ports from that same config to match the pods.
func (group *NodeGroupSpec) GetServiceConfig() *Config {
	if group.Validator != nil {
		return group.Validator.Config
	}
	return group.Config
}

func (group *NodeGroupSpec) ShouldInheritValidatorGasPrice() bool {
	if group.InheritValidatorGasPrice != nil {
		return *group.InheritValidatorGasPrice
	}
	return true
}

func (group *NodeGroupSpec) HasPdbEnabled() bool {
	if group.PDB != nil {
		return group.PDB.Enabled
	}
	return false
}

func (group *NodeGroupSpec) GetPdbMinAvailable() int {
	if group.PDB != nil && group.PDB.MinAvailable != nil {
		return *group.PDB.MinAvailable
	}
	return group.GetInstances() - 1
}

func (group *NodeGroupSpec) GetSnapshotNodeIndex() int {
	if group.SnapshotNodeIndex != nil {
		return *group.SnapshotNodeIndex
	}
	return 0
}

func (group *NodeGroupSpec) ShouldIgnoreGroupLabelOnDisruptions() bool {
	if group != nil && group.IgnoreGroupOnDisruptionChecks != nil {
		return *group.IgnoreGroupOnDisruptionChecks
	}
	return false
}

// MisplacedValidatorScopedFields returns the JSON names of group-level fields that are set on a
// validator group but never consulted for it: every instance of a validator group is reconciled
// from .validator.<field> instead (see getValidatorSpecWithBlockedSignerTargets and the validator
// PDB in ensurePodDisruptionBudgets). Returns nil for regular groups, where all of these apply.
func (group *NodeGroupSpec) MisplacedValidatorScopedFields() []string {
	if group == nil || group.Validator == nil {
		return nil
	}

	var fields []string
	if group.Config != nil {
		fields = append(fields, "config")
	}
	if group.Persistence != nil {
		fields = append(fields, "persistence")
	}
	if !isEmptyResourceRequirements(group.Resources) {
		fields = append(fields, "resources")
	}
	if len(group.NodeSelector) > 0 {
		fields = append(fields, "nodeSelector")
	}
	if group.Affinity != nil {
		fields = append(fields, "affinity")
	}
	if group.StateSyncRestore != nil {
		fields = append(fields, "stateSyncRestore")
	}
	if !isEmptyResourceRequirements(group.StateSyncResources) {
		fields = append(fields, "stateSyncResources")
	}
	if group.VPA != nil {
		fields = append(fields, "vpa")
	}
	if group.PDB != nil {
		fields = append(fields, "pdb")
	}
	if group.OverrideVersion != nil {
		fields = append(fields, "overrideVersion")
	}
	return fields
}

// IneffectiveValidatorGroupFlags returns the JSON names of group-level flags explicitly set on a
// validator group that have no effect there and no .validator.<field> counterpart:
//
//   - ignoreGroupOnDisruptionChecks: validator pods already coordinate disruptions chain-wide
//     ({chain-id, validator}), ignoring nodeset and group labels entirely.
//   - inheritValidatorGasPrice: a validator group is itself the gas-price source.
func (group *NodeGroupSpec) IneffectiveValidatorGroupFlags() []string {
	if group == nil || group.Validator == nil {
		return nil
	}

	var flags []string
	if group.IgnoreGroupOnDisruptionChecks != nil {
		flags = append(flags, "ignoreGroupOnDisruptionChecks")
	}
	if group.InheritValidatorGasPrice != nil {
		flags = append(flags, "inheritValidatorGasPrice")
	}
	return flags
}

// isEmptyResourceRequirements reports whether a ResourceRequirements value carries no user
// configuration. It is a non-pointer field, so "unset" cannot be distinguished by nil.
func isEmptyResourceRequirements(r corev1.ResourceRequirements) bool {
	return len(r.Requests) == 0 && len(r.Limits) == 0 && len(r.Claims) == 0
}

// Validator methods

func (val *NodeSetValidatorConfig) GetPrivKeySecretName(obj client.Object) string {
	if val != nil && val.PrivateKeySecret != nil {
		return *val.PrivateKeySecret
	}
	return fmt.Sprintf("%s-priv-key", obj.GetName())
}

func (val *NodeSetValidatorConfig) GetAccountHDPath() string {
	if val != nil {
		switch {
		case val.AccountHDPath != nil:
			return *val.AccountHDPath
		case val.Init != nil && val.Init.AccountHDPath != nil:
			return *val.Init.AccountHDPath
		case val.CreateValidator != nil && val.CreateValidator.AccountHDPath != nil:
			return *val.CreateValidator.AccountHDPath
		}
	}
	return DefaultHDPath
}

func (val *NodeSetValidatorConfig) GetAccountSecretName(obj client.Object) string {
	if val != nil && val.Init != nil && val.Init.AccountMnemonicSecret != nil {
		return *val.Init.AccountMnemonicSecret
	}

	return fmt.Sprintf("%s-account", obj.GetName())
}

func (val *NodeSetValidatorConfig) GetAccountPrefix() string {
	if val != nil {
		switch {
		case val.AccountPrefix != nil:
			return *val.AccountPrefix
		case val.Init != nil && val.Init.AccountPrefix != nil:
			return *val.Init.AccountPrefix
		case val.CreateValidator != nil && val.CreateValidator.AccountPrefix != nil:
			return *val.CreateValidator.AccountPrefix
		}
	}
	return DefaultAccountPrefix
}

func (val *NodeSetValidatorConfig) GetValPrefix() string {
	if val != nil {
		switch {
		case val.ValPrefix != nil:
			return *val.ValPrefix
		case val.Init != nil && val.Init.ValPrefix != nil:
			return *val.Init.ValPrefix
		case val.CreateValidator != nil && val.CreateValidator.ValPrefix != nil:
			return *val.CreateValidator.ValPrefix
		}
	}
	return DefaultValPrefix
}

func (val *NodeSetValidatorConfig) GetInitUnbondingTime() string {
	if val != nil && val.Init != nil && val.Init.UnbondingTime != nil {
		return *val.Init.UnbondingTime
	}
	return ""
}

func (val *NodeSetValidatorConfig) GetInitVotingPeriod() string {
	if val != nil && val.Init != nil && val.Init.VotingPeriod != nil {
		return *val.Init.VotingPeriod
	}
	return ""
}

func (val *NodeSetValidatorConfig) GetInitExpeditedVotingPeriod() string {
	if val != nil && val.Init != nil && val.Init.ExpeditedVotingPeriod != nil {
		return *val.Init.ExpeditedVotingPeriod
	}
	return ""
}

func (val *NodeSetValidatorConfig) HasPdbEnabled() bool {
	if val != nil && val.PDB != nil {
		return val.PDB.Enabled
	}
	return false
}

// GetPdbMinAvailable returns the PDB minAvailable for a validator group with the given number of
// instances. When not explicitly set it defaults to instances-1, matching PdbConfig's documented
// default of allowing a single disruption (0 for a single-instance validator).
func (val *NodeSetValidatorConfig) GetPdbMinAvailable(instances int) int {
	if val != nil && val.PDB != nil && val.PDB.MinAvailable != nil {
		return *val.PDB.MinAvailable
	}
	if instances < 1 {
		return 0
	}
	return instances - 1
}

// Global Ingress helper methods

func (gi *GlobalIngressConfig) GetName(owner client.Object) string {
	return fmt.Sprintf("%s-global-%s", owner.GetName(), gi.Name)
}

func (gi *GlobalIngressConfig) GetServiceName(owner client.Object) string {
	if gi.UseInternal() {
		return fmt.Sprintf("%s-global-%s-internal", owner.GetName(), gi.Name)
	}
	return fmt.Sprintf("%s-global-%s", owner.GetName(), gi.Name)
}

func (gi *GlobalIngressConfig) GetGrpcName(owner client.Object) string {
	return fmt.Sprintf("%s-global-%s-grpc", owner.GetName(), gi.Name)
}

func (gi *GlobalIngressConfig) GetTlsSecretName(owner client.Object) string {
	if gi.TlsSecretName != nil {
		return *gi.TlsSecretName
	}
	return fmt.Sprintf("%s-tls", gi.GetName(owner))
}

func (gi *GlobalIngressConfig) ShouldUseCosmoGuard(nodeSet *ChainNodeSet) bool {
	for _, groupName := range gi.Groups {
		// The reserved "validator" group name refers to the legacy singleton .spec.validator, which is
		// not in .spec.nodes. Check its config directly so a CosmoGuard-enabled legacy validator targeted
		// by this global route still selects the CosmoGuard ports.
		if groupName == ReservedValidatorGroupName {
			if v := nodeSet.Spec.Validator; v != nil && v.Config != nil && v.Config.CosmoGuardEnabled() {
				return true
			}
			continue
		}
		for _, group := range nodeSet.Spec.Nodes {
			if group.Name == groupName {
				if cfg := group.GetServiceConfig(); cfg != nil && cfg.CosmoGuardEnabled() {
					return true
				}
			}
		}
	}
	return false
}

func (gi *GlobalIngressConfig) HasGroup(name string) bool {
	for _, groupName := range gi.Groups {
		if groupName == name {
			return true
		}
	}
	return false
}

func (gi *GlobalIngressConfig) GetIngressClass() string {
	if gi != nil && gi.IngressClass != nil {
		return *gi.IngressClass
	}
	return DefaultIngressClass
}

func (gi *GlobalIngressConfig) GetGrpcAnnotations() map[string]string {
	if gi != nil && gi.GrpcAnnotations != nil {
		return gi.GrpcAnnotations
	}

	if strings.Contains(gi.GetIngressClass(), DefaultIngressClass) {
		return map[string]string{
			"nginx.ingress.kubernetes.io/backend-protocol": "GRPC",
		}
	}

	return nil
}

func (gi *GlobalIngressConfig) UseInternal() bool {
	if gi != nil && gi.UseInternalServices != nil {
		return *gi.UseInternalServices
	}
	return false
}

func (gi *GlobalIngressConfig) CreateServicesOnly() bool {
	if gi != nil && gi.ServicesOnly != nil {
		return *gi.ServicesOnly
	}
	return false
}

// Cosmoseed Helper Methods

func (cs *CosmoseedConfig) IsEnabled() bool {
	if cs != nil && cs.Enabled != nil {
		return *cs.Enabled
	}
	return false
}

func (cs *CosmoseedConfig) GetInstances() int {
	if !cs.IsEnabled() {
		return 0
	}
	if cs != nil && cs.Instances != nil {
		return *cs.Instances
	}
	return 1
}

func (cs *CosmoseedConfig) GetMaxInboundPeers() int {
	if cs != nil && cs.MaxInboundPeers != nil {
		return *cs.MaxInboundPeers
	}
	return 2000
}

func (cs *CosmoseedConfig) GetMaxOutboundPeers() int {
	if cs != nil && cs.MaxOutboundPeers != nil {
		return *cs.MaxOutboundPeers
	}
	return 20
}

func (cs *CosmoseedConfig) GetMaxPacketMsgPayloadSize() int {
	if cs != nil && cs.MaxPacketMsgPayloadSize != nil {
		return *cs.MaxPacketMsgPayloadSize
	}
	return 1024
}

func (cs *CosmoseedConfig) GetPeerQueueSize() int {
	if cs != nil && cs.PeerQueueSize != nil {
		return *cs.PeerQueueSize
	}
	return 1000
}

func (cs *CosmoseedConfig) GetDialWorkers() int {
	if cs != nil && cs.DialWorkers != nil {
		return *cs.DialWorkers
	}
	return 20
}

func (cs *CosmoseedConfig) GetLogLevel() string {
	if cs != nil && cs.LogLevel != nil {
		return *cs.LogLevel
	}
	return DefaultCosmoseedLogLevel
}

func (cs *CosmoseedConfig) GetAllowNonRoutable() bool {
	if cs != nil && cs.AllowNonRoutable != nil {
		return *cs.AllowNonRoutable
	}
	return false
}

func (cs *CosmoseedConfig) GetCosmoseedConfig(chainID, seeds string) (*cosmoseed.Config, error) {
	cfg, err := cosmoseed.DefaultConfig()
	if err != nil {
		return nil, err
	}

	cfg.ChainID = chainID
	cfg.Seeds = seeds

	cfg.AllowNonRoutable = cs.GetAllowNonRoutable()
	cfg.MaxOutboundPeers = cs.GetMaxOutboundPeers()
	cfg.MaxInboundPeers = cs.GetMaxInboundPeers()
	cfg.MaxPacketMsgPayloadSize = cs.GetMaxPacketMsgPayloadSize()
	cfg.PeerQueueSize = cs.GetPeerQueueSize()
	cfg.DialWorkers = cs.GetDialWorkers()
	cfg.LogLevel = cs.GetLogLevel()
	cfg.AddrBookFile = DefaultCosmoseedAddrBookFile
	return cfg, nil
}

func (csi *CosmoseedIngressConfig) GetIngressClass() string {
	if csi != nil && csi.IngressClass != nil {
		return *csi.IngressClass
	}
	return DefaultIngressClass
}

// Gateway helper methods

func (gc *GatewayConfig) UseInternal() bool {
	return gc != nil && gc.UseInternalServices != nil && *gc.UseInternalServices
}

func (gg *GlobalGatewayConfig) UseInternal() bool {
	return gg != nil && gg.UseInternalServices != nil && *gg.UseInternalServices
}

func (gg *GlobalGatewayConfig) GetName(owner client.Object) string {
	return fmt.Sprintf("%s-%s-gw", owner.GetName(), gg.Name)
}

func (gg *GlobalGatewayConfig) GetGrpcName(owner client.Object) string {
	return fmt.Sprintf("%s-%s-gw-grpc", owner.GetName(), gg.Name)
}

func (gg *GlobalGatewayConfig) GetServiceName(owner client.Object) string {
	if gg.UseInternal() {
		return fmt.Sprintf("%s-global-%s-internal", owner.GetName(), gg.Name)
	}
	return fmt.Sprintf("%s-global-%s", owner.GetName(), gg.Name)
}

// ShouldUseCosmoGuard returns true if any group in this gateway config has CosmoGuard enabled.
// When a gateway spans multiple groups, CosmoGuard ports are used for the shared service if at least
// one group enables it — this matches the ingress behavior where all traffic goes through the guard.
func (gg *GlobalGatewayConfig) ShouldUseCosmoGuard(nodeSet *ChainNodeSet) bool {
	for _, groupName := range gg.Groups {
		// The reserved "validator" group name refers to the legacy singleton .spec.validator, which is
		// not in .spec.nodes. Check its config directly so a CosmoGuard-enabled legacy validator targeted
		// by this global route still selects the CosmoGuard ports.
		if groupName == ReservedValidatorGroupName {
			if v := nodeSet.Spec.Validator; v != nil && v.Config != nil && v.Config.CosmoGuardEnabled() {
				return true
			}
			continue
		}
		for _, group := range nodeSet.Spec.Nodes {
			if group.Name == groupName {
				if cfg := group.GetServiceConfig(); cfg != nil && cfg.CosmoGuardEnabled() {
					return true
				}
			}
		}
	}
	return false
}

func (gg *GlobalGatewayConfig) HasGroup(name string) bool {
	for _, g := range gg.Groups {
		if g == name {
			return true
		}
	}
	return false
}

func (gg *GlobalGatewayConfig) CreateServicesOnly() bool {
	return gg.ServicesOnly != nil && *gg.ServicesOnly
}

func (gg *GlobalGatewayConfig) GetGatewayParentRef() gwapiv1.ParentReference {
	return gg.Gateway.GetParentRef()
}

// GetParentRef converts the API's compact Gateway reference into a Gateway API parent reference.
func (g GatewayRef) GetParentRef() gwapiv1.ParentReference {
	ref := gwapiv1.ParentReference{Name: gwapiv1.ObjectName(g.Name)}
	if g.Namespace != nil {
		ns := gwapiv1.Namespace(*g.Namespace)
		ref.Namespace = &ns
	}
	if g.SectionName != nil {
		section := gwapiv1.SectionName(*g.SectionName)
		ref.SectionName = &section
	}
	return ref
}
