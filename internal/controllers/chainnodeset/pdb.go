package chainnodeset

import (
	"context"
	"fmt"
	"reflect"

	"golang.org/x/exp/maps"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
)

const podDisruptionBudgetFinalizer = "cosmopilot.voluzi.com/pdb-cleanup"

func (r *Reconciler) ensurePodDisruptionBudgets(ctx context.Context, nodeSet *appsv1.ChainNodeSet) error {
	desiredPdbNames := map[string]struct{}{}
	if nodeSet.Spec.Validator.HasPdbEnabled() {
		pdb := getPdbSpec(
			nodeSet,
			fmt.Sprintf("%s-validator", nodeSet.GetName()),
			nodeSet.Spec.Validator.GetPdbMinAvailable(1),
			// Scope to the legacy validator pod only. Without the nodeset and reserved-group labels
			// this selector would also match validator-group pods (they share validator=true and the
			// chain-id), overlapping their PDBs and protecting/evicting the wrong pod.
			map[string]string{
				controllers.LabelUpgrading:             controllers.StringValueFalse,
				controllers.LabelChainID:               nodeSet.Status.ChainID,
				controllers.LabelChainNodeSet:          nodeSet.GetName(),
				controllers.LabelChainNodeSetGroup:     validatorGroupName,
				controllers.LabelChainNodeSetValidator: controllers.StringValueTrue,
			},
		)
		desiredPdbNames[pdb.GetName()] = struct{}{}
		if err := r.ensurePodDisruptionBudget(ctx, nodeSet, pdb); err != nil {
			return err
		}
	} else {
		if err := r.maybeDeletePDB(ctx, nodeSet, fmt.Sprintf("%s-validator", nodeSet.GetName())); err != nil {
			return err
		}
	}

	// Regular group PDBs are named after the group's Service (<nodeset>-<group>). The validator-PDB
	// cleanup below targets <nodeset>-<group>-validator, which is also the Service name of a regular
	// group literally named "<group>-validator". Collect regular group Service names so that cleanup
	// skips a name owned by a regular group instead of deleting that group's live PDB every reconcile.
	regularGroupServiceNames := make(map[string]struct{}, len(nodeSet.Spec.Nodes))
	for _, group := range nodeSet.Spec.Nodes {
		if group.Validator == nil {
			regularGroupServiceNames[group.GetServiceName(nodeSet)] = struct{}{}
		}
	}

	for _, group := range nodeSet.Spec.Nodes {
		// Validator groups (.spec.nodes[].validator) have no regular nodes: every pod is a
		// validator reconciled below with the dedicated validator PDB. A regular group PDB
		// would select zero pods, so skip it and delete any stale one left behind.
		if group.Validator != nil {
			if err := r.maybeDeletePDB(ctx, nodeSet, group.GetServiceName(nodeSet)); err != nil {
				return err
			}
		} else if group.HasPdbEnabled() {
			labels := map[string]string{
				controllers.LabelUpgrading:    controllers.StringValueFalse,
				controllers.LabelChainID:      nodeSet.Status.ChainID,
				controllers.LabelChainNodeSet: nodeSet.GetName(),
			}

			// Respect IgnoreGroupOnDisruptionChecks
			if !group.ShouldIgnoreGroupLabelOnDisruptions() {
				labels[controllers.LabelChainNodeSetGroup] = group.Name
			}

			// Include global-ingresses labels
			maps.Copy(labels, GetGlobalIngressLabels(nodeSet, group.Name))

			pdb := getPdbSpec(nodeSet, group.GetServiceName(nodeSet), group.GetPdbMinAvailable(), labels)
			desiredPdbNames[pdb.GetName()] = struct{}{}
			if err := r.ensurePodDisruptionBudget(ctx, nodeSet, pdb); err != nil {
				return err
			}
		} else {
			if err := r.maybeDeletePDB(ctx, nodeSet, group.GetServiceName(nodeSet)); err != nil {
				return err
			}
		}

		// Group validators (.spec.nodes[].validator) carry their own PDB config and are
		// reconciled separately from regular group nodes, so they need a dedicated PDB
		// scoped to the validators of this group.
		validatorPdbName := fmt.Sprintf("%s-validator", group.GetServiceName(nodeSet))
		if group.Validator.HasPdbEnabled() {
			pdb := getPdbSpec(
				nodeSet,
				validatorPdbName,
				group.Validator.GetPdbMinAvailable(group.GetInstances()),
				map[string]string{
					controllers.LabelUpgrading:             controllers.StringValueFalse,
					controllers.LabelChainID:               nodeSet.Status.ChainID,
					controllers.LabelChainNodeSet:          nodeSet.GetName(),
					controllers.LabelChainNodeSetGroup:     group.Name,
					controllers.LabelChainNodeSetValidator: controllers.StringValueTrue,
				},
			)
			desiredPdbNames[pdb.GetName()] = struct{}{}
			if err := r.ensurePodDisruptionBudget(ctx, nodeSet, pdb); err != nil {
				return err
			}
		} else if _, ownedByRegularGroup := regularGroupServiceNames[validatorPdbName]; !ownedByRegularGroup {
			// Skip when validatorPdbName is actually a regular group's PDB (its Service name): that
			// group reconciles this PDB itself above, so deleting it here would remove a live,
			// correctly-configured PDB on every reconcile.
			if err := r.maybeDeletePDB(ctx, nodeSet, validatorPdbName); err != nil {
				return err
			}
		}
	}

	return r.deleteStalePodDisruptionBudgets(ctx, nodeSet, desiredPdbNames)
}

func (r *Reconciler) deleteStalePodDisruptionBudgets(
	ctx context.Context,
	nodeSet *appsv1.ChainNodeSet,
	desiredNames map[string]struct{},
) error {
	pdbs := &policyv1.PodDisruptionBudgetList{}
	if err := r.List(ctx, pdbs, client.InNamespace(nodeSet.GetNamespace())); err != nil {
		return err
	}

	for i := range pdbs.Items {
		pdb := &pdbs.Items[i]
		if _, desired := desiredNames[pdb.GetName()]; desired {
			continue
		}
		if !metav1.IsControlledBy(pdb, nodeSet) && !isLegacyNodeSetPDB(nodeSet, pdb) {
			continue
		}
		if err := r.maybeDeletePDB(ctx, nodeSet, pdb.GetName()); err != nil {
			return err
		}
	}

	return nil
}

func isLegacyNodeSetPDB(nodeSet *appsv1.ChainNodeSet, pdb *policyv1.PodDisruptionBudget) bool {
	if pdb.Spec.Selector == nil {
		return false
	}

	labels := pdb.Spec.Selector.MatchLabels
	if labels[controllers.LabelUpgrading] != controllers.StringValueFalse ||
		labels[controllers.LabelChainID] == "" ||
		(nodeSet.Status.ChainID != "" && labels[controllers.LabelChainID] != nodeSet.Status.ChainID) {
		return false
	}

	if pdb.GetName() == fmt.Sprintf("%s-validator", nodeSet.GetName()) &&
		labels[controllers.LabelChainNodeSetValidator] == controllers.StringValueTrue {
		nodeSetLabel := labels[controllers.LabelChainNodeSet]
		group := labels[controllers.LabelChainNodeSetGroup]
		return (nodeSetLabel == "" || nodeSetLabel == nodeSet.GetName()) &&
			(group == "" || group == validatorGroupName)
	}

	if labels[controllers.LabelChainNodeSet] != nodeSet.GetName() {
		return false
	}
	group := labels[controllers.LabelChainNodeSetGroup]
	if group == "" {
		return false
	}

	name := fmt.Sprintf("%s-%s", nodeSet.GetName(), group)
	if labels[controllers.LabelChainNodeSetValidator] == controllers.StringValueTrue && group != validatorGroupName {
		name += "-validator"
	}

	return pdb.GetName() == name
}

func getPdbSpec(nodeSet *appsv1.ChainNodeSet, name string, min int, labels map[string]string) *policyv1.PodDisruptionBudget {
	minAvailable := intstr.FromInt32(int32(min))
	return &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: nodeSet.GetNamespace(),
		},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: &minAvailable,
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
		},
	}
}

func (r *Reconciler) ensurePodDisruptionBudget(
	ctx context.Context,
	nodeSet *appsv1.ChainNodeSet,
	pdb *policyv1.PodDisruptionBudget,
) error {
	logger := log.FromContext(ctx)

	currentPdb := &policyv1.PodDisruptionBudget{}
	err := r.Get(ctx, client.ObjectKeyFromObject(pdb), currentPdb)
	if err != nil {
		if errors.IsNotFound(err) {
			if err := controllerutil.SetControllerReference(nodeSet, pdb, r.Scheme); err != nil {
				return err
			}
			logger.Info("creating pod disruption budget", "pdb", pdb.GetName())
			return r.Create(ctx, pdb)
		}
		return err
	}
	if !metav1.IsControlledBy(currentPdb, nodeSet) && !isLegacyNodeSetPDB(nodeSet, currentPdb) {
		return fmt.Errorf("refusing to adopt pod disruption budget %s/%s without controller ownership or an identifiable legacy Cosmopilot selector", currentPdb.GetNamespace(), currentPdb.GetName())
	}

	desiredMinAvailable := pdb.Spec.MinAvailable
	desiredSelector := pdb.Spec.Selector
	mustUpdate := !reflect.DeepEqual(currentPdb.Spec.MinAvailable, desiredMinAvailable) ||
		currentPdb.Spec.MaxUnavailable != nil ||
		!reflect.DeepEqual(currentPdb.Spec.Selector, desiredSelector) ||
		!metav1.IsControlledBy(currentPdb, nodeSet)

	if !mustUpdate {
		*pdb = *currentPdb
		return nil
	}

	currentPdb.Spec.MinAvailable = desiredMinAvailable
	currentPdb.Spec.MaxUnavailable = nil
	currentPdb.Spec.Selector = desiredSelector
	if err := controllerutil.SetControllerReference(nodeSet, currentPdb, r.Scheme); err != nil {
		return fmt.Errorf("cannot manage pod disruption budget %s/%s: %w", currentPdb.GetNamespace(), currentPdb.GetName(), err)
	}

	logger.Info("updating pod disruption budget", "pdb", pdb.GetName())
	if err := r.Update(ctx, currentPdb); err != nil {
		return err
	}

	*pdb = *currentPdb
	return nil
}

func (r *Reconciler) maybeDeletePDB(ctx context.Context, nodeSet *appsv1.ChainNodeSet, name string) error {
	logger := log.FromContext(ctx)

	pdb := &policyv1.PodDisruptionBudget{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: nodeSet.GetNamespace(), Name: name}, pdb); err != nil {
		return client.IgnoreNotFound(err)
	}

	if controller := metav1.GetControllerOf(pdb); controller != nil && !metav1.IsControlledBy(pdb, nodeSet) {
		logger.Info("skipping pod disruption budget owned by another controller", "pdb", pdb.GetName(), "owner", controller.Name)
		return nil
	}
	if !metav1.IsControlledBy(pdb, nodeSet) && !isLegacyNodeSetPDB(nodeSet, pdb) {
		logger.Info("skipping ambiguous pod disruption budget", "pdb", pdb.GetName())
		return nil
	}

	_, err := r.deletePodDisruptionBudgetExact(ctx, pdb)

	if err == nil {
		logger.Info("deleted pod disruption budget", "pdb", pdb.GetName())
		return nil
	}
	if errors.IsNotFound(err) {
		return nil
	}

	return err
}

func (r *Reconciler) finalizePodDisruptionBudgets(ctx context.Context, nodeSet *appsv1.ChainNodeSet) (bool, error) {
	pdbs := &policyv1.PodDisruptionBudgetList{}
	if err := r.List(ctx, pdbs, client.InNamespace(nodeSet.GetNamespace())); err != nil {
		return false, err
	}
	allDone := true
	for i := range pdbs.Items {
		pdb := &pdbs.Items[i]
		if !metav1.IsControlledBy(pdb, nodeSet) && !isLegacyNodeSetPDB(nodeSet, pdb) {
			continue
		}
		done, err := r.deletePodDisruptionBudgetExact(ctx, pdb)
		if err != nil {
			return false, err
		}
		allDone = allDone && done
	}
	if !allDone {
		return false, nil
	}
	if controllerutil.ContainsFinalizer(nodeSet, podDisruptionBudgetFinalizer) {
		controllerutil.RemoveFinalizer(nodeSet, podDisruptionBudgetFinalizer)
		if err := r.Update(ctx, nodeSet); err != nil {
			return false, err
		}
	}
	return true, nil
}

func (r *Reconciler) deletePodDisruptionBudgetExact(ctx context.Context, pdb *policyv1.PodDisruptionBudget) (bool, error) {
	uid := pdb.GetUID()
	if pdb.GetDeletionTimestamp().IsZero() {
		if err := r.Delete(ctx, pdb, client.Preconditions{UID: &uid}); err != nil && !errors.IsNotFound(err) {
			return false, err
		}
	}
	remaining := &policyv1.PodDisruptionBudget{}
	if err := r.Get(ctx, client.ObjectKeyFromObject(pdb), remaining); err != nil {
		if errors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}
	return false, nil
}
