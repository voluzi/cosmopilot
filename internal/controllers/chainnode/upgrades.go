package chainnode

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"sort"

	upgradetypes "github.com/cosmos/cosmos-sdk/x/upgrade/types"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	appsv1 "github.com/voluzi/cosmopilot/v3/api/v1"
	"github.com/voluzi/cosmopilot/v3/pkg/nodeutils"
)

func (r *Reconciler) ensureUpgrades(ctx context.Context, chainNode *appsv1.ChainNode, nodePodRunning bool) error {
	logger := log.FromContext(ctx)

	if chainNode.Status.Upgrades == nil {
		chainNode.Status.Upgrades = make([]appsv1.Upgrade, 0)
	}

	statusCopy := chainNode.Status.DeepCopy()

	if chainNode.Status.Phase == appsv1.PhaseChainNodeRunning && nodePodRunning && chainNode.Spec.App.ShouldQueryGovUpgrades() {
		// Get gov upgrades
		govUpgrades, err := r.getGovUpgrades(ctx, chainNode)
		if err != nil {
			logger.Error(err, "could not retrieve upgrade plans")
		} else if err := r.mergeGovUpgrades(ctx, chainNode, govUpgrades); err != nil {
			return err
		}
	}

	for _, upgrade := range chainNode.Spec.App.Upgrades {
		u := appsv1.Upgrade{
			Height: upgrade.Height,
			Name:   upgrade.Name,
			Image:  upgrade.Image,
			Status: appsv1.UpgradeScheduled,
			Source: appsv1.ManualUpgrade,
		}
		// Maybe set this upgrade as gov planned upgraded
		if upgrade.ForceGovUpgrade() {
			u.Source = appsv1.OnChainUpgrade
		}
		if isHistoricalBootstrapUpgrade(chainNode, nodePodRunning, upgrade.Height) {
			u.Status = appsv1.UpgradeSkipped
		}
		if hasCancelledUpgrade(chainNode.Status.Upgrades, u.Height) {
			// Only the user brings a cancelled upgrade back by listing it in their own spec. A
			// ChainNodeSet child's spec also carries entries propagated from the set's status, which
			// must not undo a cancellation.
			configured, err := r.userConfiguredUpgrade(ctx, chainNode, u.Height, upgrade.ForceGovUpgrade())
			if err != nil {
				return err
			}
			if !configured {
				continue
			}
			chainNode.Status.Upgrades = dropCancelledUpgrade(chainNode.Status.Upgrades, u.Height)
		}

		chainNode.Status.Upgrades = AddOrUpdateConfiguredUpgrade(
			chainNode.Status.Upgrades,
			u,
			chainNode.IsControlledByChainNodeSet(),
		)
		if u.Status == appsv1.UpgradeSkipped {
			for i := range chainNode.Status.Upgrades {
				if chainNode.Status.Upgrades[i].Height == u.Height && chainNode.Status.Upgrades[i].Status == appsv1.UpgradeScheduled {
					chainNode.Status.Upgrades[i].Status = appsv1.UpgradeSkipped
				}
			}
		}
	}

	r.cancelRemovedManualUpgrades(ctx, chainNode)

	// Sort upgrades by height
	sort.Slice(chainNode.Status.Upgrades, func(i, j int) bool {
		return chainNode.Status.Upgrades[i].Height < chainNode.Status.Upgrades[j].Height
	})

	if err := r.ensureUpgradesConfig(ctx, chainNode); err != nil {
		return err
	}

	if !reflect.DeepEqual(chainNode.Status.Upgrades, statusCopy.Upgrades) {
		logger.Info("updating .status.upgrades")
		return r.Status().Update(ctx, chainNode)
	}
	return nil
}

// cancelRemovedManualUpgrades marks a scheduled manual upgrade that is no longer in the spec as
// cancelled. The removal is ignored, with a warning, once the node is at the height before the upgrade
// or the upgrade is ongoing, because node-utils may already be acting on it.
func (r *Reconciler) cancelRemovedManualUpgrades(ctx context.Context, chainNode *appsv1.ChainNode) {
	// The persisted height can trail the node, which node-utils may already have stopped one block
	// before the upgrade (the app container is then terminated but node-utils still runs). Read the
	// live height from node-utils whenever it runs before cancelling anything; if it cannot be read,
	// cancel nothing this time. Without node-utils the node is not advancing.
	latestHeight := chainNode.Status.LatestHeight
	liveHeightChecked := false
	for i := range chainNode.Status.Upgrades {
		u := &chainNode.Status.Upgrades[i]
		if u.Source != appsv1.ManualUpgrade || (u.Status != appsv1.UpgradeScheduled && u.Status != appsv1.UpgradeOnGoing) {
			continue
		}
		// A ChainNodeSet child's spec also carries history propagated from the set's status, so the
		// removal is judged against the user's own spec.
		manual, err := r.userConfiguredUpgrade(ctx, chainNode, u.Height, false)
		if err != nil {
			log.FromContext(ctx).Error(err, "not cancelling removed manual upgrades: could not read the user spec")
			return
		}
		forced, err := r.upgradeForcedOnChain(ctx, chainNode, u.Height)
		if err != nil {
			log.FromContext(ctx).Error(err, "not cancelling removed manual upgrades: could not read the user spec")
			return
		}
		if manual || forced {
			continue
		}
		if u.Status == appsv1.UpgradeScheduled && !liveHeightChecked {
			liveHeightChecked = true
			pod, err := r.getChainNodePod(ctx, chainNode)
			if err != nil {
				log.FromContext(ctx).Error(err, "not cancelling removed manual upgrades: could not read the node pod")
				return
			}
			if pod != nil && nodeUtilsIsRunning(pod) {
				status, err := r.getUpgradeStatus(ctx, chainNode)
				if err == nil && status.LatestHeight == nil {
					err = fmt.Errorf("node-utils has not observed a height yet")
				}
				if err != nil {
					log.FromContext(ctx).Error(err, "not cancelling removed manual upgrades: could not read the node height")
					return
				}
				if *status.LatestHeight > latestHeight {
					latestHeight = *status.LatestHeight
				}
			}
		}
		switch {
		case u.Status == appsv1.UpgradeOnGoing:
			r.recorder.Eventf(chainNode, corev1.EventTypeWarning, appsv1.ReasonUpgradeCancelIgnored,
				"Manual upgrade at height %d was removed from spec but is already ongoing; it will not be cancelled", u.Height)
		case u.Status != appsv1.UpgradeScheduled:
			continue
		case latestHeight >= u.Height-1:
			r.recorder.Eventf(chainNode, corev1.EventTypeWarning, appsv1.ReasonUpgradeCancelIgnored,
				"Manual upgrade at height %d was removed from spec but the node is already at height %d; it will not be cancelled",
				u.Height, latestHeight)
		default:
			u.Status = appsv1.UpgradeCancelled
			r.recorder.Eventf(chainNode, corev1.EventTypeNormal, appsv1.ReasonUpgradeCancelled,
				"Manual upgrade at height %d was removed from spec and has been cancelled", u.Height)
		}
	}
}

func hasCancelledUpgrade(upgrades []appsv1.Upgrade, height int64) bool {
	return slices.ContainsFunc(upgrades, func(u appsv1.Upgrade) bool {
		return u.Height == height && u.Status == appsv1.UpgradeCancelled
	})
}

func dropCancelledUpgrade(upgrades []appsv1.Upgrade, height int64) []appsv1.Upgrade {
	return slices.DeleteFunc(upgrades, func(u appsv1.Upgrade) bool {
		return u.Height == height && u.Status == appsv1.UpgradeCancelled
	})
}

// mergeGovUpgrades records the plans the chain reported. A plan scheduled again at a cancelled height
// brings that entry back; a pending governance entry the chain no longer schedules is cancelled.
func (r *Reconciler) mergeGovUpgrades(ctx context.Context, chainNode *appsv1.ChainNode, plans []appsv1.Upgrade) error {
	for _, upgrade := range plans {
		chainNode.Status.Upgrades = dropCancelledUpgrade(chainNode.Status.Upgrades, upgrade.Height)
		chainNode.Status.Upgrades = AddOrUpdateUpgrade(chainNode.Status.Upgrades, upgrade)
	}
	return r.retireStaleGovUpgrades(ctx, chainNode, plans)
}

// retireStaleGovUpgrades marks a pending governance upgrade above the current height as cancelled when
// the chain, queried successfully, no longer schedules a plan at that height. An entry the user forced
// on-chain in the spec (of this node, or of its ChainNodeSet) is kept.
func (r *Reconciler) retireStaleGovUpgrades(ctx context.Context, chainNode *appsv1.ChainNode, plans []appsv1.Upgrade) error {
	for i := range chainNode.Status.Upgrades {
		u := &chainNode.Status.Upgrades[i]
		if u.Source != appsv1.OnChainUpgrade || u.Height <= chainNode.Status.LatestHeight ||
			(u.Status != appsv1.UpgradeScheduled && u.Status != appsv1.UpgradeImageMissing) ||
			slices.ContainsFunc(plans, func(p appsv1.Upgrade) bool { return p.Height == u.Height }) {
			continue
		}
		forced, err := r.upgradeForcedOnChain(ctx, chainNode, u.Height)
		if err != nil {
			return err
		}
		if forced {
			continue
		}
		u.Status = appsv1.UpgradeCancelled
		r.recorder.Eventf(chainNode, corev1.EventTypeNormal, appsv1.ReasonUpgradeRetired,
			"Governance upgrade %q at height %d is no longer scheduled on chain; cancelled", u.Name, u.Height)
	}
	return nil
}

// upgradeForcedOnChain reports whether the user configured a forceOnChain upgrade at height.
func (r *Reconciler) upgradeForcedOnChain(ctx context.Context, chainNode *appsv1.ChainNode, height int64) (bool, error) {
	return r.userConfiguredUpgrade(ctx, chainNode, height, true)
}

// userConfiguredUpgrade reports whether the user's own spec lists an upgrade at height, forced on-chain
// or manual as requested. A ChainNodeSet child's spec also carries entries propagated from the set's
// status, so for a child only the set's own spec expresses user intent. If the set cannot be found the
// entry is treated as configured, which keeps it.
func (r *Reconciler) userConfiguredUpgrade(ctx context.Context, chainNode *appsv1.ChainNode, height int64, forceOnChain bool) (bool, error) {
	upgrades := chainNode.Spec.App.Upgrades
	if owner := metav1.GetControllerOf(chainNode); owner != nil && chainNode.IsControlledByChainNodeSet() {
		nodeSet := &appsv1.ChainNodeSet{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: chainNode.GetNamespace(), Name: owner.Name}, nodeSet); err != nil {
			if errors.IsNotFound(err) {
				return true, nil
			}
			return false, err
		}
		upgrades = nodeSet.Spec.App.Upgrades
	}
	for _, upgrade := range upgrades {
		if upgrade.Height == height && upgrade.ForceGovUpgrade() == forceOnChain {
			return true, nil
		}
	}
	return false, nil
}

func isHistoricalBootstrapUpgrade(chainNode *appsv1.ChainNode, nodePodRunning bool, height int64) bool {
	if nodePodRunning || height > chainNode.Status.LatestHeight {
		return false
	}
	// PvcSize is persisted before an initialized PVC reaches upgrade reconciliation. A new empty
	// volume enters InitData instead, so the empty phase identifies adopted or restored chain data.
	if chainNode.Status.Phase == "" && chainNode.Status.PvcSize != "" {
		return true
	}
	if chainNode.ShouldRestoreFromSnapshot() {
		return chainNode.Status.Phase == ""
	}
	return chainNode.StateSyncRestoreEnabled() &&
		(chainNode.Status.Phase == "" || chainNode.Status.Phase == appsv1.PhaseChainNodeInitData)
}

func (r *Reconciler) ensureUpgradesConfig(ctx context.Context, chainNode *appsv1.ChainNode) error {
	logger := log.FromContext(ctx)

	upgrades := struct {
		Upgrades []appsv1.Upgrade `json:"upgrades"`
	}{
		Upgrades: chainNode.Status.Upgrades,
	}
	b, err := json.Marshal(upgrades)
	if err != nil {
		return fmt.Errorf("marshaling upgrades config: %w", err)
	}

	spec := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-upgrades", chainNode.GetName()),
			Namespace: chainNode.GetNamespace(),
		},
		Data: map[string]string{upgradesConfigFile: string(b)},
	}
	if err := controllerutil.SetControllerReference(chainNode, spec, r.Scheme); err != nil {
		return fmt.Errorf("setting controller reference: %w", err)
	}

	cm := &corev1.ConfigMap{}
	err = r.Get(ctx, client.ObjectKeyFromObject(spec), cm)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("creating configmap with upgrades", "configmap", cm.GetName())
			return r.Create(ctx, spec)
		}
		return err
	}

	// Update when config changes
	if cm.Data[upgradesConfigFile] != string(b) {
		logger.Info("updating configmap with upgrades", "configmap", cm.GetName())
		cm.Data[upgradesConfigFile] = string(b)
		return r.Update(ctx, cm)
	}
	return nil
}

func (r *Reconciler) getUpgradeStatus(ctx context.Context, chainNode *appsv1.ChainNode) (nodeutils.UpgradeStatus, error) {
	factory := r.upgradeClientFactory
	if factory == nil {
		factory = defaultUpgradeStatusClientFactory
	}
	return factory(chainNode.GetNodeFQDN()).GetUpgradeStatus(ctx)
}

func (r *Reconciler) applyUpgradeStatus(ctx context.Context, chainNode *appsv1.ChainNode, status nodeutils.UpgradeStatus) error {
	statusChanged := false
	upgradesChanged := false
	if status.LatestHeight != nil && *status.LatestHeight > chainNode.Status.LatestHeight {
		chainNode.Status.LatestHeight = *status.LatestHeight
		statusChanged = true
	}
	if required := status.RequiredUpgrade; required != nil && required.Source == nodeutils.OnChainUpgrade &&
		governanceMarkerRecoveryAllowed(chainNode, *required) &&
		!structuredOnChainUpgradeIsStale(chainNode, *required) &&
		required.Height > 0 && required.Name != "" {
		authoritative := *required
		configuredImage := configuredGovernanceImage(chainNode, authoritative)
		if configuredImage != "" && hasCancelledUpgrade(chainNode.Status.Upgrades, authoritative.Height) {
			// A child's forceOnChain entry for a cancelled plan may only be propagated from the set's
			// status and carry the withdrawn image; only the user's own entry overrides the marker.
			forced, err := r.upgradeForcedOnChain(ctx, chainNode, authoritative.Height)
			if err != nil {
				return err
			}
			if !forced {
				configuredImage = ""
			}
		}
		if configuredImage != "" {
			authoritative.Image = configuredImage
		}
		chainNode.Status.Upgrades, upgradesChanged = recordRequiredGovernanceUpgrade(
			chainNode.Status.Upgrades,
			authoritative,
			configuredImage != "",
		)
		statusChanged = statusChanged || upgradesChanged
	}
	if !statusChanged {
		return nil
	}
	if err := r.Status().Update(ctx, chainNode); err != nil {
		return err
	}
	if upgradesChanged {
		return r.ensureUpgradesConfig(ctx, chainNode)
	}
	return nil
}

func recordRequiredGovernanceUpgrade(
	upgrades []appsv1.Upgrade,
	required nodeutils.RequiredUpgrade,
	configuredImage bool,
) ([]appsv1.Upgrade, bool) {
	status := appsv1.UpgradeImageMissing
	if required.Image != "" {
		status = appsv1.UpgradeScheduled
	}
	incoming := appsv1.Upgrade{
		Height: required.Height,
		Name:   required.Name,
		Image:  required.Image,
		Source: appsv1.OnChainUpgrade,
		Status: status,
	}
	for i, existing := range upgrades {
		if existing.Height != required.Height {
			continue
		}
		if existing.Status == appsv1.UpgradeCompleted || existing.Status == appsv1.UpgradeSkipped ||
			existing.Status == appsv1.UpgradeOnGoing {
			return upgrades, false
		}
		// The chain requires a plan at a cancelled height: take the marker's plan, not the withdrawn one.
		if existing.Name != required.Name || existing.Name == "" || existing.Status == appsv1.UpgradeCancelled {
			upgrades[i] = incoming
			return upgrades, true
		}

		updated := existing
		updated.Source = appsv1.OnChainUpgrade
		if required.Image != "" && (updated.Image == "" || configuredImage) {
			updated.Image = required.Image
		}
		if updated.Image == "" {
			updated.Status = appsv1.UpgradeImageMissing
		} else {
			updated.Status = appsv1.UpgradeScheduled
		}
		if updated == existing {
			return upgrades, false
		}
		upgrades[i] = updated
		return upgrades, true
	}
	return append(upgrades, incoming), true
}

func resolveRequiredUpgrade(chainNode *appsv1.ChainNode, status nodeutils.UpgradeStatus) (*nodeutils.RequiredUpgrade, error) {
	if status.RequiredUpgrade != nil {
		required := *status.RequiredUpgrade
		if finishedUpgradeMatches(chainNode, required) {
			return nil, nil
		}
		if required.Source == nodeutils.OnChainUpgrade {
			if structuredOnChainUpgradeIsStale(chainNode, required) {
				return nil, nil
			}
			if !governanceMarkerRecoveryAllowed(chainNode, required) {
				return nil, fmt.Errorf("governance upgrade marker at height %d cannot be recovered while .spec.app.checkGovUpgrades is false; configure a forceOnChain upgrade at this height or enable governance upgrade discovery", required.Height)
			}
		}
		return &required, nil
	}
	if !status.LegacyUpgradeRequired {
		return nil, nil
	}
	if status.LatestHeight == nil {
		return nil, fmt.Errorf("legacy node-utils requires an upgrade without reporting a height")
	}
	maxEligibleHeight := *status.LatestHeight
	if maxEligibleHeight < 1<<63-1 {
		maxEligibleHeight++
	}

	var selected *appsv1.Upgrade
	cancelledEligible := false
	for i := range chainNode.Status.Upgrades {
		upgrade := &chainNode.Status.Upgrades[i]
		if upgrade.Height <= maxEligibleHeight && upgrade.Status == appsv1.UpgradeCancelled {
			cancelledEligible = true
		}
		if upgrade.Height > maxEligibleHeight ||
			(upgrade.Status != appsv1.UpgradeScheduled && upgrade.Status != appsv1.UpgradeOnGoing) {
			continue
		}
		if selected == nil || upgrade.Height < selected.Height {
			selected = upgrade
		}
	}
	if selected == nil && cancelledEligible {
		// A legacy sidecar latched an upgrade that was cancelled before it saw the new config. Nothing is
		// applied; replacing the Pod (with the current sidecar) clears the latch.
		return nil, nil
	}
	if selected == nil {
		return nil, fmt.Errorf("legacy node-utils requires an upgrade at height %d but no pending upgrade is eligible", *status.LatestHeight)
	}
	return &nodeutils.RequiredUpgrade{
		Height: selected.Height,
		Source: nodeutils.UpgradeSource(selected.Source),
		Name:   selected.Name,
	}, nil
}

func configuredGovernanceImage(chainNode *appsv1.ChainNode, required nodeutils.RequiredUpgrade) string {
	for i := range chainNode.Spec.App.Upgrades {
		upgrade := &chainNode.Spec.App.Upgrades[i]
		if upgrade.Height != required.Height || !upgrade.ForceGovUpgrade() || upgrade.Image == "" {
			continue
		}
		if upgrade.Name == "" || upgrade.Name == required.Name {
			return upgrade.Image
		}
	}
	return ""
}

func finishedUpgradeMatches(chainNode *appsv1.ChainNode, required nodeutils.RequiredUpgrade) bool {
	for _, upgrade := range chainNode.Status.Upgrades {
		if upgrade.Height == required.Height && string(upgrade.Source) == string(required.Source) &&
			upgrade.Name == required.Name &&
			(upgrade.Status == appsv1.UpgradeCompleted || upgrade.Status == appsv1.UpgradeSkipped) {
			return true
		}
	}
	return false
}

func (r *Reconciler) getUpgrade(chainNode *appsv1.ChainNode, required nodeutils.RequiredUpgrade) *appsv1.Upgrade {
	for _, upgrade := range chainNode.Status.Upgrades {
		if upgrade.Height != required.Height ||
			(upgrade.Status != appsv1.UpgradeScheduled &&
				upgrade.Status != appsv1.UpgradeOnGoing &&
				upgrade.Status != appsv1.UpgradeImageMissing) {
			continue
		}
		if required.Source != "" && string(upgrade.Source) != string(required.Source) {
			continue
		}
		if required.Name != "" && upgrade.Name != required.Name {
			continue
		}
		return &upgrade
	}
	return nil
}

func governanceMarkerRecoveryAllowed(chainNode *appsv1.ChainNode, required nodeutils.RequiredUpgrade) bool {
	if chainNode.Spec.App.ShouldQueryGovUpgrades() {
		return true
	}
	for _, upgrade := range chainNode.Status.Upgrades {
		if upgrade.Height != required.Height || upgrade.Source != appsv1.OnChainUpgrade {
			continue
		}
		if upgrade.Status == appsv1.UpgradeScheduled || upgrade.Status == appsv1.UpgradeImageMissing ||
			upgrade.Status == appsv1.UpgradeOnGoing {
			return true
		}
	}
	for i := range chainNode.Spec.App.Upgrades {
		upgrade := &chainNode.Spec.App.Upgrades[i]
		if upgrade.Height == required.Height && upgrade.ForceGovUpgrade() {
			return true
		}
	}
	return false
}

func structuredOnChainUpgradeIsStale(chainNode *appsv1.ChainNode, required nodeutils.RequiredUpgrade) bool {
	return required.Height > 0 && chainNode.Status.LatestHeight >= required.Height
}

func sanitizeUnknownGovernanceRequirement(
	chainNode *appsv1.ChainNode,
	pod *corev1.Pod,
	status nodeutils.UpgradeStatus,
) nodeutils.UpgradeStatus {
	required := status.RequiredUpgrade
	if required == nil || required.Source != nodeutils.OnChainUpgrade || chainNode.Status.LatestHeight > 0 ||
		status.LatestHeight != nil && *status.LatestHeight > 0 {
		return status
	}
	if knownPendingUpgradeMatches(chainNode, *required) || forcedUpgradeMatches(chainNode, *required) ||
		containerHasTerminated(pod, chainNode.Spec.App.App) || pod != nil &&
		(pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded) {
		return status
	}
	status.RequiredUpgrade = nil
	return status
}

func knownPendingUpgradeMatches(chainNode *appsv1.ChainNode, required nodeutils.RequiredUpgrade) bool {
	for _, upgrade := range chainNode.Status.Upgrades {
		if upgrade.Height != required.Height || string(upgrade.Source) != string(required.Source) ||
			upgrade.Name != required.Name {
			continue
		}
		if upgrade.Status == appsv1.UpgradeScheduled || upgrade.Status == appsv1.UpgradeImageMissing ||
			upgrade.Status == appsv1.UpgradeOnGoing {
			return true
		}
	}
	return false
}

func forcedUpgradeMatches(chainNode *appsv1.ChainNode, required nodeutils.RequiredUpgrade) bool {
	for i := range chainNode.Spec.App.Upgrades {
		upgrade := &chainNode.Spec.App.Upgrades[i]
		if upgrade.Height == required.Height && upgrade.ForceGovUpgrade() &&
			(upgrade.Name == "" || upgrade.Name == required.Name) {
			return true
		}
	}
	return false
}

func (r *Reconciler) setUpgradeStatus(ctx context.Context, chainNode *appsv1.ChainNode, upgrade *appsv1.Upgrade, status appsv1.UpgradePhase) error {
	logger := log.FromContext(ctx)

	for i, u := range chainNode.Status.Upgrades {
		if u.Height == upgrade.Height {
			chainNode.Status.Upgrades[i].Status = status
			if status == appsv1.UpgradeCompleted {
				addUpgradeStatusCondition(chainNode, upgrade)
			}
			logger.Info("setting upgrade status", "height", upgrade.Height, "status", status)
			if err := r.Status().Update(ctx, chainNode); err != nil {
				return err
			}
			// always update upgrades configmap so node-utils is aware of upgrade status
			return r.ensureUpgradesConfig(ctx, chainNode)
		}
	}
	return fmt.Errorf("cant update upgrade phase: upgrade not found")
}

func (r *Reconciler) getGovUpgrades(ctx context.Context, chainNode *appsv1.ChainNode) ([]appsv1.Upgrade, error) {
	c, err := r.getChainNodeClient(chainNode)
	if err != nil {
		return nil, fmt.Errorf("creating chain client: %w", err)
	}

	plannedUpgrade, err := c.GetNextUpgrade(ctx)
	if err != nil {
		return nil, fmt.Errorf("querying next upgrade: %w", err)
	}

	upgrades := make([]appsv1.Upgrade, 0)
	if plannedUpgrade != nil {
		upgrades = append(upgrades, upgradeFromPlan(plannedUpgrade))
	}
	return upgrades, nil
}

func upgradeFromPlan(plan *upgradetypes.Plan) appsv1.Upgrade {
	upgrade := appsv1.Upgrade{
		Height: plan.Height,
		Name:   plan.Name,
		Status: appsv1.UpgradeScheduled,
		Source: appsv1.OnChainUpgrade,
	}
	info := struct {
		Binaries struct {
			Docker string `json:"docker"`
		} `json:"binaries"`
	}{}
	if err := json.Unmarshal([]byte(plan.Info), &info); err == nil && info.Binaries.Docker != "" {
		upgrade.Image = info.Binaries.Docker
	} else {
		upgrade.Status = appsv1.UpgradeImageMissing
	}
	return upgrade
}

func AddOrUpdateUpgrade(upgrades []appsv1.Upgrade, upgrade appsv1.Upgrade) []appsv1.Upgrade {
	for i, u := range upgrades {
		if u.Height == upgrade.Height {
			if u.Status == appsv1.UpgradeCompleted || u.Status == appsv1.UpgradeSkipped || u.Status == appsv1.UpgradeOnGoing {
				return upgrades
			}

			switch {
			case upgrade.Source == appsv1.OnChainUpgrade && upgrade.Name != "":
				if u.Source == appsv1.ManualUpgrade {
					return upgrades
				}
				upgrades[i] = upgrade
			case upgrade.Source == appsv1.OnChainUpgrade && upgrade.Name == "":
				if u.Source == appsv1.OnChainUpgrade && u.Name != "" {
					if u.Image == "" && upgrade.Image != "" {
						upgrades[i].Image = upgrade.Image
						upgrades[i].Status = upgrade.Status
					}
					return upgrades
				}
				upgrade.Name = u.Name
				upgrades[i] = upgrade
			case u.Source == appsv1.OnChainUpgrade && upgrade.Source == appsv1.ManualUpgrade:
				if u.Name != "" {
					upgrades[i].Image = upgrade.Image
					upgrades[i].Status = upgrade.Status
				} else {
					upgrades[i] = upgrade
				}
			default:
				upgrades[i] = upgrade
			}
			return upgrades
		}
	}
	upgrades = append(upgrades, upgrade)
	return upgrades
}

// AddOrUpdateConfiguredUpgrade merges a spec-originated entry without allowing a stale propagated
// plan identity to replace a named governance plan queried directly by a managed child.
func AddOrUpdateConfiguredUpgrade(
	upgrades []appsv1.Upgrade,
	upgrade appsv1.Upgrade,
	managedByChainNodeSet bool,
) []appsv1.Upgrade {
	for i, existing := range upgrades {
		if existing.Height != upgrade.Height {
			continue
		}
		if existing.Status == appsv1.UpgradeCompleted || existing.Status == appsv1.UpgradeSkipped ||
			existing.Status == appsv1.UpgradeOnGoing {
			return upgrades
		}
		if existing.Source != appsv1.OnChainUpgrade ||
			upgrade.Source != appsv1.OnChainUpgrade || existing.Name == "" {
			continue
		}
		if (upgrade.Name == "" && managedByChainNodeSet) ||
			(upgrade.Name != "" && upgrade.Name != existing.Name) {
			return upgrades
		}
		if upgrade.Image != "" {
			upgrades[i].Image = upgrade.Image
			upgrades[i].Status = upgrade.Status
		}
		return upgrades
	}
	return AddOrUpdateUpgrade(upgrades, upgrade)
}

func addUpgradeStatusCondition(chainNode *appsv1.ChainNode, upgrade *appsv1.Upgrade) {
	if chainNode.Status.Conditions == nil {
		chainNode.Status.Conditions = make([]metav1.Condition, 0)
	}
	chainNode.Status.Conditions = append(chainNode.Status.Conditions, metav1.Condition{
		Type:               appsv1.ConditionUpgrade,
		Status:             metav1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
		Reason:             appsv1.ReasonUpgradeSuccess,
		Message:            fmt.Sprintf("Successfully upgraded node to image %s", upgrade.Image),
	})
}

// skipUpgradeForOverride marks only the explicitly required upgrade as skipped and republishes the
// config consumed by node-utils. The target identity prevents an unrelated upgrade from being
// discarded when committed progress and the required upgrade height differ.
func (r *Reconciler) skipUpgradeForOverride(ctx context.Context, chainNode *appsv1.ChainNode, required nodeutils.RequiredUpgrade) error {
	logger := log.FromContext(ctx)
	skipped := make([]int64, 0, 1)
	for i, u := range chainNode.Status.Upgrades {
		if (u.Status == appsv1.UpgradeScheduled || u.Status == appsv1.UpgradeOnGoing || u.Status == appsv1.UpgradeImageMissing) &&
			u.Height == required.Height &&
			(required.Source == "" || string(u.Source) == string(required.Source)) &&
			(required.Name == "" || u.Name == "" || u.Name == required.Name) {
			chainNode.Status.Upgrades[i].Status = appsv1.UpgradeSkipped
			skipped = append(skipped, u.Height)
		}
	}

	if len(skipped) == 0 {
		return nil
	}

	logger.Info("skipping upgrades on pinned node", "heights", skipped)
	if err := r.Status().Update(ctx, chainNode); err != nil {
		return err
	}
	return r.ensureUpgradesConfig(ctx, chainNode)
}
