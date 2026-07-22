package controllers

import (
	"strings"

	"github.com/voluzi/cosmopilot/v2/pkg/utils"
)

// CosmoGuardLabelDomain is the label-key domain CosmoGuard owns: the guard-private selector labels
// (cosmoguard.voluzi.com/…) and the per-route selector labels (route.cosmoguard.voluzi.com/…). Any
// owner label under this domain must never be inherited onto guard pods — a route.cosmoguard.voluzi.com
// key would make a global-route Service select the guard even for routes it should not serve.
const CosmoGuardLabelDomain = "cosmoguard.voluzi.com/"

// GuardInheritedLabels returns the owner (ChainNode/ChainNodeSet) metadata labels that are safe to
// propagate onto standalone CosmoGuard pods: the owner's labels minus every cosmopilot-managed
// selector key and any label under CosmoGuard's own domain. Those managed keys drive
// node/group/global-route Service and cosmosigner selection, so copying them onto guard pods would let
// a node/route-targeting selector match the guard (e.g. the raw group Service, which selects
// nodeset+group, or a global-route Service, which selects route.cosmoguard.voluzi.com/<route>). Genuine
// user labels (team, env, NetworkPolicy tiers, monitoring markers) are kept so the standalone guard is
// covered by the same NetworkPolicies and monitoring the in-pod sidecar was via the node pod. The
// guard-private selector labels remain authoritative — callers layer these underneath them.
func GuardInheritedLabels(ownerLabels map[string]string) map[string]string {
	out := utils.ExcludeMapKeys(ownerLabels,
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
	for k := range out {
		// Match the two owned prefixes at the start of the key (guard-private and per-route), not
		// anywhere in it — an unrelated user label whose DNS prefix merely ends in the domain (e.g.
		// "acme-cosmoguard.voluzi.com/tier") must be preserved.
		if strings.HasPrefix(k, CosmoGuardLabelDomain) || strings.HasPrefix(k, "route."+CosmoGuardLabelDomain) {
			delete(out, k)
		}
	}
	return out
}
