package chainnode

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
	"github.com/voluzi/cosmopilot/v2/pkg/utils"
)

func WithChainNodeLabels(chainNode *appsv1.ChainNode, additional ...map[string]string) map[string]string {
	// Exclude the cosmosigner-target discovery selector from inherited labels: it must only be set on
	// genuinely-targeted node pods (added explicitly below), never inherited from a user label, or
	// the signer would dial pods that are not listening for it (including its own pods).
	labels := utils.ExcludeMapKeys(chainNode.ObjectMeta.Labels, controllers.LabelWorkerName, controllers.LabelCosmosignerTarget)
	for _, m := range additional {
		labels = utils.MergeMaps(labels, m, controllers.LabelWorkerName)
	}
	// A standalone signer-target pod must not inherit a user-supplied ChainNodeSet discovery scope.
	// Genuine ChainNodeSet children retain the label required by their signer's two-label selector.
	if !chainNode.IsControlledByChainNodeSet() && labels[controllers.LabelCosmosignerTarget] != "" {
		delete(labels, controllers.LabelChainNodeSet)
	}
	return labels
}

func (r *Reconciler) filterNonWorkingPeers(ctx context.Context, chainNode *appsv1.ChainNode, peers []appsv1.Peer) []appsv1.Peer {
	logger := log.FromContext(ctx)
	workingPeers := make([]appsv1.Peer, 0)

	for _, peer := range peers {
		peerName := strings.TrimSuffix(peer.Address, "-internal")
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      peerName,
				Namespace: chainNode.Namespace,
			},
		}

		err := r.Get(ctx, client.ObjectKeyFromObject(pod), pod)
		if err != nil {
			logger.Info("excluding peer from rpc-servers list",
				"id", peer.ID,
				"peer", peerName,
				"reason", fmt.Errorf("error retrieving pod: %v", err),
			)
			continue
		}

		if !IsPodReady(pod) {
			logger.Info("excluding peer from rpc-servers list",
				"id", peer.ID,
				"peer", peerName,
				"reason", "pod is not ready",
			)
			continue
		}

		c, err := r.getChainNodeClientByHost(fmt.Sprintf("%s.%s.svc.cluster.local", peer.Address, chainNode.GetNamespace()))
		if err != nil {
			logger.Info("excluding peer from rpc-servers list",
				"id", peer.ID,
				"peer", peerName,
				"reason", fmt.Errorf("error creating chainnode client: %v", err),
			)
			continue
		}
		_, err = c.GetNodeStatus(ctx)
		if err != nil {
			logger.Info("excluding peer from rpc-servers list",
				"id", peer.ID,
				"peer", peerName,
				"reason", fmt.Errorf("error retrieving node status: %v", err),
			)
			continue
		}
		workingPeers = append(workingPeers, peer)
	}

	return workingPeers
}

func IsPodReady(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}
