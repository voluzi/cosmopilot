package chainnode

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
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
		} else {
			for _, upgrade := range govUpgrades {
				chainNode.Status.Upgrades = AddOrUpdateUpgrade(chainNode.Status.Upgrades, upgrade)
			}
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
		!structuredOnChainUpgradeIsStale(chainNode, *required) &&
		required.Height > 0 && required.Name != "" {
		chainNode.Status.Upgrades, upgradesChanged = recordRequiredGovernanceUpgrade(chainNode.Status.Upgrades, *required)
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
		if existing.Name != required.Name || existing.Name == "" {
			upgrades[i] = incoming
			return upgrades, true
		}

		updated := existing
		updated.Source = appsv1.OnChainUpgrade
		if updated.Image == "" && required.Image != "" {
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
		if status.RequiredUpgrade.Source == nodeutils.OnChainUpgrade &&
			structuredOnChainUpgradeIsStale(chainNode, *status.RequiredUpgrade) {
			return nil, nil
		}
		required := *status.RequiredUpgrade
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
	for i := range chainNode.Status.Upgrades {
		upgrade := &chainNode.Status.Upgrades[i]
		if upgrade.Height > maxEligibleHeight ||
			(upgrade.Status != appsv1.UpgradeScheduled && upgrade.Status != appsv1.UpgradeOnGoing) {
			continue
		}
		if selected == nil || upgrade.Height < selected.Height {
			selected = upgrade
		}
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

func structuredOnChainUpgradeIsStale(chainNode *appsv1.ChainNode, required nodeutils.RequiredUpgrade) bool {
	return required.Height > 0 && chainNode.Status.LatestHeight >= required.Height
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
