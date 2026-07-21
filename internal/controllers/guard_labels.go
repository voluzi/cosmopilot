package controllers

import "github.com/voluzi/cosmopilot/v2/pkg/utils"

// GuardInheritedLabels returns the owner (ChainNode/ChainNodeSet) metadata labels that are safe to
// propagate onto standalone CosmoGuard pods: the owner's labels minus every cosmopilot-managed
// selector key. Those managed keys drive node/group/global-route Service and cosmosigner selection,
// so copying them onto guard pods would let a node-targeting selector match the guard (e.g. the raw
// group Service, which selects nodeset+group). Genuine user labels (team, env, NetworkPolicy tiers,
// monitoring markers) are kept so the standalone guard is covered by the same NetworkPolicies and
// monitoring the in-pod sidecar was via the node pod. The guard-private selector labels remain
// authoritative — callers layer these underneath them.
func GuardInheritedLabels(ownerLabels map[string]string) map[string]string {
	return utils.ExcludeMapKeys(ownerLabels,
		LabelNodeID,
		LabelChainID,
		LabelValidator,
		LabelChainNode,
		LabelChainNodeSet,
		LabelChainNodeSetGroup,
		LabelChainNodeSetValidator,
		LabelGlobalIngress,
		LabelScope,
		LabelApp,
		LabelSeed,
		LabelPeer,
		LabelUpgrading,
		LabelCosmosignerTarget,
		LabelWorkerName,
	)
}
