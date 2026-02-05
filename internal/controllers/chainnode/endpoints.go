package chainnode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"

	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
)

// endpointSliceHandler maps EndpointSlice changes to ChainNode reconciliation requests.
// When peer endpoints change, this triggers reconciliation of ChainNodes that depend on those peers.
type endpointSliceHandler struct {
	client client.Client
}

// newEndpointSliceHandler creates a new handler for EndpointSlice events.
func newEndpointSliceHandler(c client.Client) handler.EventHandler {
	return handler.EnqueueRequestsFromMapFunc((&endpointSliceHandler{client: c}).mapEndpointSliceToChainNodes)
}

// mapEndpointSliceToChainNodes finds ChainNodes that should be reconciled when an EndpointSlice changes.
// It looks for ChainNodes with the same chain-id as the peer service that owns the EndpointSlice.
func (h *endpointSliceHandler) mapEndpointSliceToChainNodes(ctx context.Context, obj client.Object) []reconcile.Request {
	logger := log.FromContext(ctx).WithValues("endpointslice", obj.GetName(), "namespace", obj.GetNamespace())

	endpointSlice, ok := obj.(*discoveryv1.EndpointSlice)
	if !ok {
		return nil
	}

	// Get the service name from the EndpointSlice's owner label
	serviceName, ok := endpointSlice.Labels[discoveryv1.LabelServiceName]
	if !ok {
		return nil
	}

	// Check if this EndpointSlice belongs to a peer service
	// EndpointSlices inherit labels from their parent Service
	if endpointSlice.Labels[controllers.LabelPeer] != controllers.StringValueTrue {
		return nil
	}

	// Get the chain-id from the EndpointSlice labels
	chainID, ok := endpointSlice.Labels[controllers.LabelChainID]
	if !ok {
		logger.V(1).Info("peer endpointslice has no chain-id label", "service", serviceName)
		return nil
	}

	// Get the node-id of the peer whose endpoints changed
	peerNodeID := endpointSlice.Labels[controllers.LabelNodeID]

	logger.V(1).Info("peer endpoint change detected",
		"service", serviceName,
		"chainID", chainID,
		"peerNodeID", peerNodeID,
	)

	// Find all ChainNodes with the same chain-id that should be notified
	chainNodeList := &appsv1.ChainNodeList{}
	if err := h.client.List(ctx, chainNodeList,
		client.InNamespace(endpointSlice.Namespace),
		client.MatchingLabels{controllers.LabelChainID: chainID},
	); err != nil {
		logger.Error(err, "failed to list chainnodes for peer endpoint change")
		return nil
	}

	requests := make([]reconcile.Request, 0, len(chainNodeList.Items))
	for _, cn := range chainNodeList.Items {
		// Skip the ChainNode that owns this EndpointSlice (it's the one whose endpoints changed)
		if cn.Status.NodeID == peerNodeID {
			continue
		}

		// Skip if auto-discover peers is disabled
		if !cn.AutoDiscoverPeersEnabled() {
			continue
		}

		logger.V(1).Info("enqueueing chainnode for reconciliation due to peer endpoint change",
			"chainnode", cn.Name,
			"peerNodeID", peerNodeID,
		)

		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      cn.Name,
				Namespace: cn.Namespace,
			},
		})
	}

	return requests
}

// getPeerEndpointsHash calculates a hash of all peer endpoint addresses for a ChainNode.
// This hash is used to detect when peer endpoints change, triggering a pod restart.
func (r *Reconciler) getPeerEndpointsHash(ctx context.Context, chainNode *appsv1.ChainNode) (string, error) {
	if !chainNode.AutoDiscoverPeersEnabled() {
		return "", nil
	}

	if chainNode.Status.ChainID == "" {
		return "", nil
	}

	// List all EndpointSlices for peer services with the same chain-id
	endpointSliceList := &discoveryv1.EndpointSliceList{}
	if err := r.List(ctx, endpointSliceList,
		client.InNamespace(chainNode.Namespace),
		client.MatchingLabels{
			controllers.LabelPeer:    controllers.StringValueTrue,
			controllers.LabelChainID: chainNode.Status.ChainID,
		},
	); err != nil {
		return "", fmt.Errorf("failed to list peer endpointslices: %w", err)
	}

	// Collect all endpoint addresses, excluding self
	var addresses []string
	for _, es := range endpointSliceList.Items {
		// Skip self
		if es.Labels[controllers.LabelNodeID] == chainNode.Status.NodeID {
			continue
		}

		for _, endpoint := range es.Endpoints {
			for _, addr := range endpoint.Addresses {
				addresses = append(addresses, addr)
			}
		}
	}

	// Sort for deterministic hash
	sort.Strings(addresses)

	// Calculate hash
	if len(addresses) == 0 {
		return "", nil
	}

	hashInput := strings.Join(addresses, ",")
	hash := sha256.Sum256([]byte(hashInput))
	return hex.EncodeToString(hash[:8]), nil // Use first 8 bytes for shorter hash
}
