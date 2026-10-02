package chainnodeset

import (
	"context"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1 "github.com/voluzi/cosmopilot/v5/api/v1"
	"github.com/voluzi/cosmopilot/v5/internal/controllers"
)

func (r *Reconciler) refuseLegacyTmKMS(ctx context.Context, nodeSet *appsv1.ChainNodeSet) error {
	nodes := &appsv1.ChainNodeList{}
	if err := r.uncachedReader().List(ctx, nodes, client.InNamespace(nodeSet.Namespace)); err != nil {
		return err
	}
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if !metav1.IsControlledBy(node, nodeSet) || !node.DeletionTimestamp.IsZero() {
			continue
		}
		if err := controllers.RefuseLegacyTmKMS(ctx, r.uncachedReader(), r.recorder, node); err != nil {
			return err
		}
	}
	return nil
}
