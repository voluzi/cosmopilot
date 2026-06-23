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
	"sigs.k8s.io/controller-runtime/pkg/log"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
)

func (r *Reconciler) ensurePodDisruptionBudgets(ctx context.Context, nodeSet *appsv1.ChainNodeSet) error {
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
		if err := r.ensurePodDisruptionBudget(ctx, pdb); err != nil {
			return err
		}
	} else {
		if err := r.maybeDeletePDB(ctx, fmt.Sprintf("%s-validator", nodeSet.GetName()), nodeSet.GetNamespace()); err != nil {
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
			if err := r.maybeDeletePDB(ctx, group.GetServiceName(nodeSet), nodeSet.GetNamespace()); err != nil {
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
			if err := r.ensurePodDisruptionBudget(ctx, pdb); err != nil {
				return err
			}
		} else {
			if err := r.maybeDeletePDB(ctx, group.GetServiceName(nodeSet), nodeSet.GetNamespace()); err != nil {
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
			if err := r.ensurePodDisruptionBudget(ctx, pdb); err != nil {
				return err
			}
		} else if _, ownedByRegularGroup := regularGroupServiceNames[validatorPdbName]; !ownedByRegularGroup {
			// Skip when validatorPdbName is actually a regular group's PDB (its Service name): that
			// group reconciles this PDB itself above, so deleting it here would remove a live,
			// correctly-configured PDB on every reconcile.
			if err := r.maybeDeletePDB(ctx, validatorPdbName, nodeSet.GetNamespace()); err != nil {
				return err
			}
		}
	}

	return nil
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

func (r *Reconciler) ensurePodDisruptionBudget(ctx context.Context, pdb *policyv1.PodDisruptionBudget) error {
	logger := log.FromContext(ctx)

	currentPdb := &policyv1.PodDisruptionBudget{}
	err := r.Get(ctx, client.ObjectKeyFromObject(pdb), currentPdb)
	if err != nil {
		if errors.IsNotFound(err) {
			logger.Info("creating pod disruption budget", "pdb", pdb.GetName())
			return r.Create(ctx, pdb)
		}
		return err
	}

	mustUpdate := currentPdb.Spec.MinAvailable.IntValue() != pdb.Spec.MinAvailable.IntValue() ||
		!reflect.DeepEqual(currentPdb.Spec.Selector.MatchLabels, pdb.Spec.Selector.MatchLabels)

	if mustUpdate {
		logger.Info("updating pod disruption budget", "pdb", pdb.GetName())

		pdb.ObjectMeta.ResourceVersion = currentPdb.ObjectMeta.ResourceVersion
		if err := r.Update(ctx, pdb); err != nil {
			return err
		}
	}

	*pdb = *currentPdb
	return nil
}

func (r *Reconciler) maybeDeletePDB(ctx context.Context, name, namespace string) error {
	logger := log.FromContext(ctx)

	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
	}
	err := r.Delete(ctx, pdb)

	if err == nil {
		logger.Info("deleted pod disruption budget", "pdb", pdb.GetName())
		return nil
	}
	if errors.IsNotFound(err) {
		return nil
	}

	return err
}
