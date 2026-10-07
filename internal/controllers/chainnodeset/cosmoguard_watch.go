package chainnodeset

import (
	"context"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	appsv1 "github.com/voluzi/cosmopilot/v5/api/v1"
)

func (r *Reconciler) cosmoGuardConfigMapRequests(ctx context.Context, obj client.Object) []reconcile.Request {
	nodes := &appsv1.ChainNodeSetList{}
	if err := r.List(ctx, nodes, client.InNamespace(obj.GetNamespace())); err != nil {
		log.FromContext(ctx).Error(err, "listing cosmoguard ConfigMap consumers")
		return nil
	}
	var requests []reconcile.Request
	for _, nodeSet := range nodes.Items {
		for _, group := range nodeSet.Spec.Nodes {
			cfg := group.GetServiceConfig()
			ref := cfg.GetCosmoGuardConfig()
			if group.GetInstances() != 0 && cfg.CosmoGuardEnabled() && ref != nil && ref.Name == obj.GetName() {
				requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&nodeSet)})
				break
			}
		}
	}
	return requests
}
