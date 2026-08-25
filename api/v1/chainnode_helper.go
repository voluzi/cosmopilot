package v1

import (
	"fmt"
	"reflect"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/kube-openapi/pkg/validation/strfmt"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	gwapiv1 "sigs.k8s.io/gateway-api/apis/v1"
)

const (
	// DefaultPersistenceSize is the default size of the data PVC.
	DefaultPersistenceSize = "50Gi"

	// DefaultAutoResize indicates whether auto-resize is enabled by default.
	DefaultAutoResize = true

	// DefaultAutoResizeThreshold is the usage percentage that triggers PVC auto-resize.
	DefaultAutoResizeThreshold = 80

	// DefaultAutoResizeIncrement is the amount by which the PVC grows when resized.
	DefaultAutoResizeIncrement = "50Gi"

	// DefaultAutoResizeMaxSize is the maximum size to which the PVC can auto-resize.
	DefaultAutoResizeMaxSize = "2Ti"
)

func (chainNode *ChainNode) Equal(n *ChainNode) bool {
	if !reflect.DeepEqual(chainNode.Labels, n.Labels) {
		return false
	}

	if !reflect.DeepEqual(chainNode.Spec, n.Spec) {
		return false
	}

	return true
}

func (chainNode *ChainNode) GetNamespacedName() string {
	return types.NamespacedName{Namespace: chainNode.GetNamespace(), Name: chainNode.GetName()}.String()
}

func (chainNode *ChainNode) GetReconcilePeriod() time.Duration {
	if chainNode.Spec.Config != nil && chainNode.Spec.Config.ReconcilePeriod != nil {
		if d, err := strfmt.ParseDuration(*chainNode.Spec.Config.ReconcilePeriod); err == nil {
			return d
		}
	}
	return DefaultReconcilePeriod
}

func (chainNode *ChainNode) GetNodeFQDN() string {
	return fmt.Sprintf("%s-internal.%s.svc.cluster.local", chainNode.GetName(), chainNode.GetNamespace())
}

func (chainNode *ChainNode) GetPersistenceSize() string {
	if chainNode.Spec.Persistence != nil && chainNode.Spec.Persistence.Size != nil {
		return *chainNode.Spec.Persistence.Size
	}
	return DefaultPersistenceSize
}

// GetPersistenceStorageClass returns the configured storage class name for the PVC, or nil if not specified.
func (chainNode *ChainNode) GetPersistenceStorageClass() *string {
	if chainNode.Spec.Persistence == nil {
		return nil
	}
	return chainNode.Spec.Persistence.StorageClassName
}

func (chainNode *ChainNode) GetPersistenceAutoResizeEnabled() bool {
	if chainNode.Spec.Persistence != nil && chainNode.Spec.Persistence.AutoResize != nil {
		return *chainNode.Spec.Persistence.AutoResize
	}
	return DefaultAutoResize
}

func (chainNode *ChainNode) GetPersistenceAutoResizeThreshold() int {
	if chainNode.Spec.Persistence != nil && chainNode.Spec.Persistence.AutoResizeThreshold != nil {
		return *chainNode.Spec.Persistence.AutoResizeThreshold
	}
	return DefaultAutoResizeThreshold
}

func (chainNode *ChainNode) GetPersistenceAutoResizeIncrement() string {
	if chainNode.Spec.Persistence != nil && chainNode.Spec.Persistence.AutoResizeIncrement != nil {
		return *chainNode.Spec.Persistence.AutoResizeIncrement
	}
	return DefaultAutoResizeIncrement
}

func (chainNode *ChainNode) GetPersistenceAutoResizeMaxSize() string {
	if chainNode.Spec.Persistence != nil && chainNode.Spec.Persistence.AutoResizeMaxSize != nil {
		return *chainNode.Spec.Persistence.AutoResizeMaxSize
	}
	return DefaultAutoResizeMaxSize
}

func (chainNode *ChainNode) GetPersistenceInitCommands() []InitCommand {
	if chainNode.Spec.Persistence != nil && chainNode.Spec.Persistence.AdditionalInitCommands != nil {
		return chainNode.Spec.Persistence.AdditionalInitCommands
	}
	return []InitCommand{}
}

func (chainNode *ChainNode) GetPersistenceInitTimeout() time.Duration {
	if chainNode.Spec.Persistence != nil && chainNode.Spec.Persistence.InitTimeout != nil {
		if d, err := strfmt.ParseDuration(*chainNode.Spec.Persistence.InitTimeout); err == nil {
			return d
		}
	}
	return 5 * time.Minute
}

func (chainNode *ChainNode) GetPersistenceAdditionalVolumes() []VolumeSpec {
	if chainNode.Spec.Persistence != nil && chainNode.Spec.Persistence.AdditionalVolumes != nil {
		return chainNode.Spec.Persistence.AdditionalVolumes
	}
	return []VolumeSpec{}
}

func (chainNode *ChainNode) SnapshotsEnabled() bool {
	return chainNode.Spec.Persistence != nil && chainNode.Spec.Persistence.Snapshots != nil
}

func (chainNode *ChainNode) ShouldRestoreFromSnapshot() bool {
	return chainNode.Spec.Persistence != nil && chainNode.Spec.Persistence.RestoreFromSnapshot != nil
}

func (chainNode *ChainNode) IsValidator() bool {
	return chainNode.Spec.Validator != nil
}

// IsReady reports whether this node is serving: it is running, catching up, or momentarily busy
// taking a snapshot. Every other phase (including the empty phase of a node that has not reported
// yet) means it is not usable.
//
// Snapshotting is only serving when the node keeps running through the snapshot. With
// .persistence.snapshots.stopNode the pod is deliberately deleted for the duration, so the node is
// down while holding that phase.
func (chainNode *ChainNode) IsReady() bool {
	switch chainNode.Status.Phase {
	case PhaseChainNodeRunning, PhaseChainNodeSyncing:
		return true
	case PhaseChainNodeSnapshotting:
		return chainNode.Spec.Persistence == nil ||
			!chainNode.Spec.Persistence.Snapshots.ShouldStopNode()
	default:
		return false
	}
}

func (chainNode *ChainNode) ShouldInitGenesis() bool {
	return chainNode.Spec.Validator != nil && chainNode.Spec.Validator.Init != nil
}

func (chainNode *ChainNode) UsesTmKms() bool {
	return chainNode.Spec.Validator != nil && chainNode.Spec.Validator.TmKMS != nil
}

// UsesCosmosigner reports whether this ChainNode declares its own managed cosmosigner deployment
// (standalone ChainNode usage).
func (chainNode *ChainNode) UsesCosmosigner() bool {
	return chainNode.Spec.Cosmosigner != nil
}

// IsSignerTarget reports whether this ChainNode signs through an external cosmosigner deployment,
// either because it declares one directly (.spec.cosmosigner) or because a parent ChainNodeSet
// marked it as a target group node (.spec.remoteSignerTarget).
func (chainNode *ChainNode) IsSignerTarget() bool {
	return chainNode.UsesCosmosigner() || chainNode.Spec.RemoteSignerTarget
}

// UsesRemoteSigner reports whether block signing for this node happens outside the node process,
// either via a TmKMS sidecar or via an external cosmosigner deployment. In both cases the node
// listens on priv_validator_laddr and the local priv-key secret is not mounted.
func (chainNode *ChainNode) UsesRemoteSigner() bool {
	return chainNode.UsesTmKms() || chainNode.IsSignerTarget()
}

func (chainNode *ChainNode) ShouldUploadVaultKey() bool {
	if chainNode.ShouldInitGenesis() {
		return true
	}

	if chainNode.Spec.Validator.TmKMS != nil && chainNode.Spec.Validator.TmKMS.Provider.Hashicorp != nil {
		return chainNode.Spec.Validator.TmKMS.Provider.Hashicorp.UploadGenerated
	}

	return false
}

func (chainNode *ChainNode) ShouldCreateValidator() bool {
	return chainNode.Spec.Validator != nil && chainNode.Spec.Validator.CreateValidator != nil && chainNode.Status.ValidatorStatus == ""
}

func (chainNode *ChainNode) RequiresPrivKey() bool {
	if !chainNode.IsValidator() {
		return false
	}

	if chainNode.Status.PubKey == "" && chainNode.ShouldInitGenesis() {
		// For key upload when we are initializing a chain

		if chainNode.Spec.Validator.TmKMS != nil && chainNode.Spec.Validator.TmKMS.Provider.Hashicorp != nil {
			chainNode.Spec.Validator.TmKMS.Provider.Hashicorp.UploadGenerated = true
		}
		return true
	}

	if chainNode.Status.PubKey == "" && chainNode.ShouldCreateValidator() {
		return true
	}

	return false
}

func (chainNode *ChainNode) RequiresAccount() bool {
	// If we already have an account lets ignore this
	if chainNode.Status.AccountAddress != "" {
		return false
	}

	if !chainNode.IsValidator() {
		return false
	}

	if chainNode.ShouldInitGenesis() {
		return true
	}

	if chainNode.ShouldCreateValidator() {
		return true
	}

	return false
}

func (chainNode *ChainNode) AutoDiscoverPeersEnabled() bool {
	if chainNode.Spec.AutoDiscoverPeers != nil {
		return *chainNode.Spec.AutoDiscoverPeers
	}
	return true
}

func (chainNode *ChainNode) StateSyncRestoreEnabled() bool {
	if chainNode.Spec.StateSyncRestore != nil {
		return *chainNode.Spec.StateSyncRestore
	}
	return false
}

func (chainNode *ChainNode) GetMoniker() string {
	if chainNode.IsValidator() && chainNode.Spec.Validator.Info != nil && chainNode.Spec.Validator.Info.Moniker != nil {
		return *chainNode.Spec.Validator.Info.Moniker
	}
	return chainNode.GetName()
}

// appliedUpgradeImage returns the image of the highest upgrade whose height the node has already
// reached. Upgrades without an image (an on-chain plan that did not carry one) are ignored, so the
// last known good image is kept instead of falling back to the initial one.
func (chainNode *ChainNode) appliedUpgradeImage() string {
	var h int64
	var image string
	for _, u := range chainNode.Status.Upgrades {
		if u.Image == "" {
			continue
		}
		if (u.Status == UpgradeCompleted || u.Status == UpgradeSkipped || u.Status == UpgradeOnGoing) &&
			u.Height > h && u.Height <= chainNode.Status.LatestHeight {
			h = u.Height
			image = u.Image
		}
	}
	return image
}

// latestUpgradeImage returns the image of the highest processed upgrade, whether or not the node has
// reached its height.
func (chainNode *ChainNode) latestUpgradeImage() string {
	var h int64
	var image string
	for _, u := range chainNode.Status.Upgrades {
		if u.Image == "" {
			continue
		}
		if (u.Status == UpgradeCompleted || u.Status == UpgradeSkipped) && u.Height > h {
			h = u.Height
			image = u.Image
		}
	}
	return image
}

// resolveAppImage applies the image precedence: an explicit image override wins, then a version
// override against the configured repository, then the given upgrade image, and finally the initial
// image from `.spec.app`.
//
// An upgrade image is used verbatim: it replaces the repository as well as the tag, so an upgrade
// may move a node to a different registry.
func (chainNode *ChainNode) resolveAppImage(upgradeImage string) string {
	if chainNode.Spec.OverrideImage != nil {
		return *chainNode.Spec.OverrideImage
	}
	if chainNode.Spec.OverrideVersion != nil {
		return JoinImageRef(chainNode.Spec.App.Image, *chainNode.Spec.OverrideVersion)
	}
	if upgradeImage != "" {
		return upgradeImage
	}
	return chainNode.Spec.App.GetImage()
}

// GetAppImage returns the image this node should currently be running.
func (chainNode *ChainNode) GetAppImage() string {
	return chainNode.resolveAppImage(chainNode.appliedUpgradeImage())
}

// GetLatestAppImage returns the image of the most recent upgrade known for this node.
func (chainNode *ChainNode) GetLatestAppImage() string {
	return chainNode.resolveAppImage(chainNode.latestUpgradeImage())
}

// GetAppVersion returns the tag or digest of the image this node should currently be running.
func (chainNode *ChainNode) GetAppVersion() string {
	return ImageRefVersion(chainNode.GetAppImage())
}

// GetLatestVersion returns the tag or digest of the most recent upgrade known for this node.
func (chainNode *ChainNode) GetLatestVersion() string {
	return ImageRefVersion(chainNode.GetLatestAppImage())
}

func (chainNode *ChainNode) GetAppImageWithVersion(version string) string {
	return JoinImageRef(chainNode.Spec.App.Image, version)
}

// GetAppImagePullPolicy returns the pull policy for the resolved app image. It is derived from the
// image actually being run, so an upgrade or an override onto `latest` still pulls on every start.
func (chainNode *ChainNode) GetAppImagePullPolicy() corev1.PullPolicy {
	if chainNode.Spec.App.ImagePullPolicy != "" {
		return chainNode.Spec.App.ImagePullPolicy
	}
	if ImageRefVersion(chainNode.GetAppImage()) == DefaultImageVersion {
		return corev1.PullAlways
	}
	return corev1.PullIfNotPresent
}

func (chainNode *ChainNode) GetAdditionalRunFlags() []string {
	if chainNode.Spec.Config != nil && chainNode.Spec.Config.RunFlags != nil {
		return chainNode.Spec.Config.RunFlags
	}
	return []string{}
}

func (chainNode *ChainNode) GetLastUpgradeHeight() int64 {
	var h int64 = 0
	for _, u := range chainNode.Status.Upgrades {
		if (u.Status == UpgradeCompleted || u.Status == UpgradeSkipped) && u.Height > h {
			h = u.Height
		}
	}
	return h
}

func (chainNode *ChainNode) ShouldIgnoreGroupOnDisruption() bool {
	return chainNode.Spec.IgnoreGroupOnDisruptionChecks != nil && *chainNode.Spec.IgnoreGroupOnDisruptionChecks
}

func (chainNode *ChainNode) MustStop() (bool, string) {
	if chainNode.Spec.Config != nil && chainNode.Spec.Config.HaltHeight != nil {
		return *chainNode.Spec.Config.HaltHeight == chainNode.Status.LatestHeight, fmt.Sprintf("halt height %d", *chainNode.Spec.Config.HaltHeight)
	}
	return false, ""
}

func (chainNode *ChainNode) GetResources() corev1.ResourceRequirements {
	if len(chainNode.Spec.StateSyncResources.Limits) == 0 && len(chainNode.Spec.StateSyncResources.Requests) == 0 {
		return chainNode.Spec.Resources
	}

	if chainNode.StateSyncRestoreEnabled() &&
		!chainNode.ShouldRestoreFromSnapshot() &&
		(chainNode.Status.Phase == PhaseChainNodeInitData ||
			chainNode.Status.Phase == PhaseChainNodeStarting ||
			chainNode.Status.Phase == PhaseChainNodeStateSyncing) {
		return chainNode.Spec.StateSyncResources
	}

	return chainNode.Spec.Resources
}

// Validator methods

func (val *ValidatorConfig) GetPrivKeySecretName(obj client.Object) string {
	if val.PrivateKeySecret != nil {
		return *val.PrivateKeySecret
	}
	return fmt.Sprintf("%s-priv-key", obj.GetName())
}

func (val *ValidatorConfig) GetAccountHDPath() string {
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

func (val *ValidatorConfig) GetAccountSecretName(obj client.Object) string {
	if val.Init != nil && val.Init.AccountMnemonicSecret != nil {
		return *val.Init.AccountMnemonicSecret
	}

	// A create-validator node joining an existing chain signs its create-validator tx from the
	// configured (already funded) operator account. Honor that secret here so all ChainNode paths
	// resolve to it instead of falling back to the generated <chainnode>-account default.
	if val.CreateValidator != nil && val.CreateValidator.AccountMnemonicSecret != nil {
		return *val.CreateValidator.AccountMnemonicSecret
	}

	return fmt.Sprintf("%s-account", obj.GetName())
}

func (val *ValidatorConfig) GetAccountPrefix() string {
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

func (val *ValidatorConfig) GetValPrefix() string {
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

func (val *ValidatorConfig) GetInitUnbondingTime() string {
	if val.Init != nil && val.Init.UnbondingTime != nil {
		return *val.Init.UnbondingTime
	}
	return ""
}

func (val *ValidatorConfig) GetInitVotingPeriod() string {
	if val.Init != nil && val.Init.VotingPeriod != nil {
		return *val.Init.VotingPeriod
	}
	return ""
}

func (val *ValidatorConfig) GetInitExpeditedVotingPeriod() string {
	if val.Init != nil && val.Init.ExpeditedVotingPeriod != nil {
		return *val.Init.ExpeditedVotingPeriod
	}
	return ""
}

func (val *ValidatorConfig) GetCommissionMaxChangeRate() string {
	if val.Init != nil && val.Init.CommissionMaxChangeRate != nil {
		return *val.Init.CommissionMaxChangeRate
	}
	if val.CreateValidator != nil && val.CreateValidator.CommissionMaxChangeRate != nil {
		return *val.CreateValidator.CommissionMaxChangeRate
	}
	return DefaultCommissionMaxChangeRate
}

func (val *ValidatorConfig) GetCommissionMaxRate() string {
	if val.Init != nil && val.Init.CommissionMaxRate != nil {
		return *val.Init.CommissionMaxRate
	}
	if val.CreateValidator != nil && val.CreateValidator.CommissionMaxRate != nil {
		return *val.CreateValidator.CommissionMaxRate
	}
	return DefaultCommissionMaxRate
}

func (val *ValidatorConfig) GetCommissionRate() string {
	if val.Init != nil && val.Init.CommissionRate != nil {
		return *val.Init.CommissionRate
	}
	if val.CreateValidator != nil && val.CreateValidator.CommissionRate != nil {
		return *val.CreateValidator.CommissionRate
	}
	return DefaultCommissionRate
}

func (val *ValidatorConfig) GetMinSelfDelegation() *string {
	if val.Init != nil && val.Init.MinSelfDelegation != nil {
		if *val.Init.MinSelfDelegation == "" {
			return nil
		}
		return val.Init.MinSelfDelegation
	}
	if val.CreateValidator != nil && val.CreateValidator.MinSelfDelegation != nil {
		if *val.CreateValidator.MinSelfDelegation == "" {
			return nil
		}
		return val.CreateValidator.MinSelfDelegation
	}
	return ptr.To(DefaultMinimumSelfDelegation)
}

// ChainNode ingress helper methods

func (chainNode *ChainNode) GetIngressSecretName() string {
	if chainNode.Spec.Ingress != nil && chainNode.Spec.Ingress.TlsSecretName != nil {
		return *chainNode.Spec.Ingress.TlsSecretName
	}
	return fmt.Sprintf("%s-tls", chainNode.GetName())
}

func (chainNode *ChainNode) GetIngressClass() string {
	if chainNode.Spec.Ingress != nil && chainNode.Spec.Ingress.IngressClass != nil {
		return *chainNode.Spec.Ingress.IngressClass
	}
	return DefaultIngressClass
}

func (chainNode *ChainNode) GetGrpcAnnotations() map[string]string {
	if chainNode.Spec.Ingress != nil && chainNode.Spec.Ingress.GrpcAnnotations != nil {
		return chainNode.Spec.Ingress.GrpcAnnotations
	}
	if strings.Contains(chainNode.GetIngressClass(), DefaultIngressClass) {
		return map[string]string{
			"nginx.ingress.kubernetes.io/backend-protocol": "GRPC",
		}
	}
	return nil
}

func (chainNode *ChainNode) UseInternal() bool {
	if chainNode.Spec.Ingress != nil && chainNode.Spec.Ingress.UseInternalServices != nil {
		return *chainNode.Spec.Ingress.UseInternalServices
	}
	if chainNode.Spec.Gateway != nil && chainNode.Spec.Gateway.UseInternalServices != nil {
		return *chainNode.Spec.Gateway.UseInternalServices
	}
	return false
}

func (chainNode *ChainNode) GetServiceName() string {
	if chainNode.UseInternal() {
		return fmt.Sprintf("%s-internal", chainNode.GetName())
	}
	return chainNode.GetName()
}

// CosmoGuardName returns the name of the standalone CosmoGuard resources (Deployment/Service)
// created for a standalone ChainNode.
func (chainNode *ChainNode) CosmoGuardName() string {
	return fmt.Sprintf("%s-cg", chainNode.GetName())
}

func (chainNode *ChainNode) GetGatewayParentRef() gwapiv1.ParentReference {
	return chainNode.Spec.Gateway.Gateway.GetParentRef()
}
