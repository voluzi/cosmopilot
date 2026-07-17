package cosmosigner

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
)

// ReconcileStatefulSetMigration advances one persisted break-before-make migration phase. A phase
// transition is returned to the caller for status persistence; ready is true only in Recreating.
func ReconcileStatefulSetMigration(
	ctx context.Context,
	c client.Client,
	owner client.Object,
	namespace, name string,
	phase appsv1.CosmosignerMigrationPhase,
	resetState bool,
) (ready bool, next appsv1.CosmosignerMigrationPhase, err error) {
	switch phase {
	case appsv1.CosmosignerMigrationQuiescing:
		retained, err := retainStatefulSetPVCs(ctx, c, owner, namespace, name)
		if err != nil || !retained {
			return false, phase, err
		}
		quiesced, err := ScaleDown(ctx, c, owner, namespace, name)
		if err != nil || !quiesced {
			return false, phase, err
		}
		return false, appsv1.CosmosignerMigrationDeleting, nil

	case appsv1.CosmosignerMigrationDeleting:
		deleted, err := DeleteStatefulSet(ctx, c, owner, namespace, name)
		if err != nil || !deleted {
			return false, phase, err
		}
		return false, appsv1.CosmosignerMigrationResettingState, nil

	case appsv1.CosmosignerMigrationResettingState:
		if resetState {
			if err := DeletePVCs(ctx, c, owner, namespace, name); err != nil {
				return false, phase, err
			}
			gone, err := OwnedPVCsGone(ctx, c, owner, namespace, name)
			if err != nil || !gone {
				return false, phase, err
			}
		}
		return false, appsv1.CosmosignerMigrationRecreating, nil

	case appsv1.CosmosignerMigrationRecreating:
		// Re-check both invariants immediately before recreation. This catches a stale controller
		// write or manual StatefulSet recreation that happened after the earlier deletion phase.
		gone, err := DeleteStatefulSet(ctx, c, owner, namespace, name)
		if err != nil || !gone {
			return false, phase, err
		}
		return true, phase, nil

	default:
		return false, phase, fmt.Errorf("invalid cosmosigner migration phase %q", phase)
	}
}
