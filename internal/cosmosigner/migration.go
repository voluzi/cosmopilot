package cosmosigner

import (
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
)

// RetainedStateRequired reports whether an established signer must still have every locked Raft PVC.
// A different-key migration may discard old slash-protection state only after its persisted phase has
// reached ResettingState; unknown phases retain state so recovery fails closed.
func RetainedStateRequired(established bool, migration *appsv1.CosmosignerMigrationStatus) bool {
	if !established && migration == nil {
		return false
	}
	if migration == nil || !migration.ResetState {
		return true
	}
	switch migration.Phase {
	case appsv1.CosmosignerMigrationResettingState,
		appsv1.CosmosignerMigrationRetargeting,
		appsv1.CosmosignerMigrationRecreating,
		appsv1.CosmosignerMigrationRollingOut:
		return false
	default:
		return true
	}
}

// StatefulSetApplyGuard derives the apply-time state checks from persisted migration and replica
// locks. Migration phases before RollingOut are teardown/retarget stages and cannot recreate a
// StatefulSet, even when a different-key reset no longer needs the old claims.
func StatefulSetApplyGuard(established bool, migration *appsv1.CosmosignerMigrationStatus, lockedReplicas *int32, desiredReplicas int32) (applyGuard, error) {
	if migration != nil && migration.Phase != appsv1.CosmosignerMigrationRollingOut {
		return applyGuard{}, fmt.Errorf("cosmosigner StatefulSet cannot be applied during migration phase %q", migration.Phase)
	}
	guard := applyGuard{
		RequireRetainedState:  RetainedStateRequired(established, migration),
		RetainedStateReplicas: desiredReplicas,
	}
	if !guard.RequireRetainedState {
		return guard, nil
	}
	if lockedReplicas == nil || *lockedReplicas <= 0 {
		return applyGuard{}, fmt.Errorf("established cosmosigner retained raft-state replica lock is missing or invalid")
	}
	guard.RetainedStateReplicas = *lockedReplicas
	return guard, nil
}

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
