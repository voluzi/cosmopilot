package chainnodeset

import (
	"context"
	"reflect"
	"sort"

	"k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	appsv1 "github.com/voluzi/cosmopilot/v4/api/v1"
	"github.com/voluzi/cosmopilot/v4/internal/controllers"
)

func (r *Reconciler) ensureUpgrades(ctx context.Context, nodeSet *appsv1.ChainNodeSet) error {
	logger := log.FromContext(ctx)

	if nodeSet.Status.Upgrades == nil {
		nodeSet.Status.Upgrades = make([]appsv1.Upgrade, 0)
	}

	statusCopy := nodeSet.Status.DeepCopy()

	// Grab all nodes for this ChainNodeSet
	selector := labels.SelectorFromSet(map[string]string{
		controllers.LabelChainNodeSet: nodeSet.GetName(),
	})
	chainNodeList := &appsv1.ChainNodeList{}
	if err := r.List(ctx, chainNodeList, &client.ListOptions{
		LabelSelector: selector,
	}); err != nil {
		return err
	}

	for _, node := range chainNodeList.Items {
		if node.Status.LatestHeight > nodeSet.Status.LatestHeight {
			nodeSet.Status.LatestHeight = node.Status.LatestHeight
		}
	}
	observedUpgrades := aggregateChildUpgrades(chainNodeList.Items)
	for _, observed := range observedUpgrades {
		replaced := false
		for i := range nodeSet.Status.Upgrades {
			if nodeSet.Status.Upgrades[i].Height != observed.Height {
				continue
			}
			nodeSet.Status.Upgrades[i] = observed
			replaced = true
			break
		}
		if !replaced {
			nodeSet.Status.Upgrades = append(nodeSet.Status.Upgrades, observed)
		}
	}

	// Sort upgrades by height
	sort.Slice(nodeSet.Status.Upgrades, func(i, j int) bool {
		return nodeSet.Status.Upgrades[i].Height < nodeSet.Status.Upgrades[j].Height
	})

	if statusCopy.LatestHeight != nodeSet.Status.LatestHeight || !reflect.DeepEqual(nodeSet.Status.Upgrades, statusCopy.Upgrades) {
		logger.Info("updating .status.upgrades")
		return r.Status().Update(ctx, nodeSet)
	}
	return nil
}

func aggregateChildUpgrades(nodes []appsv1.ChainNode) []appsv1.Upgrade {
	planNames := make(map[int64]map[string]struct{})
	for _, node := range nodes {
		for _, upgrade := range node.Status.Upgrades {
			// A cancelled plan is no longer pending, so it cannot conflict with its replacement.
			if upgrade.Source != appsv1.OnChainUpgrade || upgrade.Status == appsv1.UpgradeConflict ||
				upgrade.Status == appsv1.UpgradeCancelled {
				continue
			}
			if upgrade.Name == "" {
				continue
			}
			if planNames[upgrade.Height] == nil {
				planNames[upgrade.Height] = make(map[string]struct{})
			}
			planNames[upgrade.Height][upgrade.Name] = struct{}{}
		}
	}

	upgrades := make([]appsv1.Upgrade, 0)
	for _, node := range nodes {
		for _, upgrade := range node.Status.Upgrades {
			names := planNames[upgrade.Height]
			if len(names) > 1 {
				continue
			}
			if len(names) == 1 {
				if upgrade.Source != appsv1.OnChainUpgrade {
					continue
				}
				if _, matches := names[upgrade.Name]; !matches {
					continue
				}
			}
			upgrades = AddOrUpdateUpgrade(upgrades, upgrade)
		}
	}
	for height, names := range planNames {
		if len(names) <= 1 {
			continue
		}
		upgrades = append(upgrades, appsv1.Upgrade{
			Height: height,
			Source: appsv1.OnChainUpgrade,
			Status: appsv1.UpgradeConflict,
		})
	}
	return upgrades
}

func AddOrUpdateUpgrade(upgrades []appsv1.Upgrade, upgrade appsv1.Upgrade) []appsv1.Upgrade {
	for i, u := range upgrades {
		if u.Height == upgrade.Height {
			// A cancellation only shows when every child agrees: children list in no fixed order, and
			// a lagging child that still has the entry scheduled must not make the aggregate flip.
			// Merging still applies the rule below, in either order: a child that skipped the upgrade
			// counts as done.
			if upgrade.Status == appsv1.UpgradeCancelled {
				if u.Status == appsv1.UpgradeSkipped {
					upgrades[i].Status = appsv1.UpgradeCompleted
				}
				return upgrades
			}
			if u.Status == appsv1.UpgradeCancelled {
				upgrades[i] = upgrade
				if upgrade.Status == appsv1.UpgradeSkipped {
					upgrades[i].Status = appsv1.UpgradeCompleted
				}
				return upgrades
			}
			if u.Source == appsv1.OnChainUpgrade && upgrade.Source == appsv1.OnChainUpgrade &&
				u.Name != "" && upgrade.Name != "" && u.Name != upgrade.Name &&
				(u.Status == appsv1.UpgradeScheduled || u.Status == appsv1.UpgradeImageMissing) &&
				(upgrade.Status == appsv1.UpgradeScheduled || upgrade.Status == appsv1.UpgradeImageMissing) {
				upgrades[i] = upgrade
				return upgrades
			}
			if upgrades[i].Name == "" && upgrade.Name != "" {
				upgrades[i].Name = upgrade.Name
			}
			// Backfill an image that was initially missing from an on-chain proposal once a child
			// ChainNode receives it from a matching manual upgrade.
			if upgrades[i].Image == "" && upgrade.Image != "" {
				upgrades[i].Image = upgrade.Image
				if u.Status == appsv1.UpgradeImageMissing {
					upgrades[i].Status = upgrade.Status
				}
			}

			// ChainNodeSet might contain nodes that actually did the upgrade and others that skipped it.
			// Mark the aggregate upgrade as completed in either case.
			if u.Status == appsv1.UpgradeSkipped || upgrade.Status == appsv1.UpgradeCompleted || upgrade.Status == appsv1.UpgradeSkipped {
				upgrades[i].Status = appsv1.UpgradeCompleted
			}
			return upgrades
		}
	}
	upgrades = append(upgrades, upgrade)
	return upgrades
}
