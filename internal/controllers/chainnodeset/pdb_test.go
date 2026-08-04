package chainnodeset

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
)

func newPdbTestReconciler(t *testing.T, nodeSet *appsv1.ChainNodeSet) *Reconciler {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, policyv1.AddToScheme(scheme))

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(nodeSet).
		Build()

	return &Reconciler{
		Client:   cl,
		Scheme:   scheme,
		recorder: record.NewFakeRecorder(100),
	}
}

func getPdb(t *testing.T, r *Reconciler, namespace, name string) *policyv1.PodDisruptionBudget {
	t.Helper()
	pdb := &policyv1.PodDisruptionBudget{}
	err := r.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: name}, pdb)
	if errors.IsNotFound(err) {
		return nil
	}
	require.NoError(t, err)
	return pdb
}

// TestEnsurePodDisruptionBudgetsGroupValidator verifies that a group validator PDB
// (.spec.nodes[].validator.pdb) is reconciled with a dedicated, non-colliding name and a
// selector scoped to the validators of that group.
func TestEnsurePodDisruptionBudgetsGroupValidator(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-nodeset",
			Namespace: "default",
			UID:       types.UID("test-uid"),
		},
		Spec: appsv1.ChainNodeSetSpec{
			Nodes: []appsv1.NodeGroupSpec{
				{
					Name:      "validators",
					Instances: ptr.To(2),
					Validator: &appsv1.NodeSetValidatorConfig{
						PDB: &appsv1.PdbConfig{Enabled: true, MinAvailable: ptr.To(1)},
					},
				},
			},
		},
		Status: appsv1.ChainNodeSetStatus{ChainID: "test-chain"},
	}

	r := newPdbTestReconciler(t, nodeSet)
	require.NoError(t, r.ensurePodDisruptionBudgets(context.Background(), nodeSet))

	pdb := getPdb(t, r, "default", "test-nodeset-validators-validator")
	require.NotNil(t, pdb, "group validator PDB should be created")
	assert.True(t, metav1.IsControlledBy(pdb, nodeSet))

	assert.Equal(t, 1, pdb.Spec.MinAvailable.IntValue())
	assert.Equal(t, map[string]string{
		controllers.LabelUpgrading:             controllers.StringValueFalse,
		controllers.LabelChainID:               "test-chain",
		controllers.LabelChainNodeSet:          "test-nodeset",
		controllers.LabelChainNodeSetGroup:     "validators",
		controllers.LabelChainNodeSetValidator: controllers.StringValueTrue,
	}, pdb.Spec.Selector.MatchLabels)

	// The regular group PDB must not be created for a validator-only group.
	assert.Nil(t, getPdb(t, r, "default", "test-nodeset-validators"), "regular group PDB should not exist")
}

// TestEnsurePodDisruptionBudgetsValidatorGroupWithGroupPdb verifies that, for a validator-only
// group with both .pdb.enabled and .validator.pdb.enabled, only the dedicated validator PDB is
// reconciled. The regular group PDB would select zero pods (every pod is a validator), so it must
// not be created, and any stale one must be removed.
func TestEnsurePodDisruptionBudgetsValidatorGroupWithGroupPdb(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-nodeset",
			Namespace: "default",
			UID:       types.UID("test-uid"),
		},
		Spec: appsv1.ChainNodeSetSpec{
			Nodes: []appsv1.NodeGroupSpec{
				{
					Name:      "validators",
					Instances: ptr.To(2),
					PDB:       &appsv1.PdbConfig{Enabled: true, MinAvailable: ptr.To(1)},
					Validator: &appsv1.NodeSetValidatorConfig{
						PDB: &appsv1.PdbConfig{Enabled: true, MinAvailable: ptr.To(1)},
					},
				},
			},
		},
		Status: appsv1.ChainNodeSetStatus{ChainID: "test-chain"},
	}

	r := newPdbTestReconciler(t, nodeSet)

	// Seed a stale regular group PDB as if the group had previously had regular nodes.
	stale := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-nodeset-validators",
			Namespace: "default",
		},
	}
	require.NoError(t, controllerutil.SetControllerReference(nodeSet, stale, r.Scheme))
	require.NoError(t, r.Create(context.Background(), stale))

	require.NoError(t, r.ensurePodDisruptionBudgets(context.Background(), nodeSet))

	// Dedicated validator PDB exists.
	require.NotNil(t, getPdb(t, r, "default", "test-nodeset-validators-validator"), "group validator PDB should be created")

	// The regular group PDB must not exist, even though .pdb.enabled is set on the group.
	assert.Nil(t, getPdb(t, r, "default", "test-nodeset-validators"), "regular group PDB should not exist for a validator group")
}

// TestEnsurePodDisruptionBudgetsGroupValidatorDisabledRemovesStale verifies that a stale
// group validator PDB is removed when .spec.nodes[].validator.pdb is disabled/absent.
func TestEnsurePodDisruptionBudgetsGroupValidatorDisabledRemovesStale(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-nodeset",
			Namespace: "default",
			UID:       types.UID("test-uid"),
		},
		Spec: appsv1.ChainNodeSetSpec{
			Nodes: []appsv1.NodeGroupSpec{
				{
					Name:      "validators",
					Instances: ptr.To(2),
					Validator: &appsv1.NodeSetValidatorConfig{},
				},
			},
		},
		Status: appsv1.ChainNodeSetStatus{ChainID: "test-chain"},
	}

	r := newPdbTestReconciler(t, nodeSet)

	// Seed a stale validator PDB as if it had previously been enabled.
	stale := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-nodeset-validators-validator",
			Namespace: "default",
		},
	}
	require.NoError(t, controllerutil.SetControllerReference(nodeSet, stale, r.Scheme))
	require.NoError(t, r.Create(context.Background(), stale))

	require.NoError(t, r.ensurePodDisruptionBudgets(context.Background(), nodeSet))

	assert.Nil(t, getPdb(t, r, "default", "test-nodeset-validators-validator"), "stale validator PDB should be removed")
}

// TestEnsurePodDisruptionBudgetsValidatorSuffixDoesNotDeleteRegularGroupPdb verifies that the stale
// validator-PDB cleanup of one group does not delete the live regular group PDB of another group
// whose name ends in "-validator". A validator group "foo" cleans up <nodeset>-foo-validator, which
// is also the regular group PDB name of a regular group literally named "foo-validator".
func TestEnsurePodDisruptionBudgetsValidatorSuffixDoesNotDeleteRegularGroupPdb(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-nodeset",
			Namespace: "default",
			UID:       types.UID("test-uid"),
		},
		Spec: appsv1.ChainNodeSetSpec{
			Nodes: []appsv1.NodeGroupSpec{
				// Regular group whose name collides with the validator-PDB suffix of group "foo".
				{
					Name:      "foo-validator",
					Instances: ptr.To(2),
					PDB:       &appsv1.PdbConfig{Enabled: true, MinAvailable: ptr.To(1)},
				},
				// Validator group "foo" with its validator PDB disabled: it cleans up
				// <nodeset>-foo-validator, which is the regular group above's PDB.
				{
					Name:      "foo",
					Instances: ptr.To(1),
					Validator: &appsv1.NodeSetValidatorConfig{},
				},
			},
		},
		Status: appsv1.ChainNodeSetStatus{ChainID: "test-chain"},
	}

	r := newPdbTestReconciler(t, nodeSet)
	require.NoError(t, r.ensurePodDisruptionBudgets(context.Background(), nodeSet))

	// The regular group "foo-validator" PDB must survive: it is owned by that group, not a stale
	// validator PDB of group "foo".
	pdb := getPdb(t, r, "default", "test-nodeset-foo-validator")
	require.NotNil(t, pdb, "regular group PDB must not be deleted by another group's validator cleanup")
	assert.Equal(t, "foo-validator", pdb.Spec.Selector.MatchLabels[controllers.LabelChainNodeSetGroup])
}

func TestEnsurePodDisruptionBudgetsRegularGroupOwnedByNodeSet(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-nodeset",
			Namespace: "default",
			UID:       types.UID("test-uid"),
		},
		Spec: appsv1.ChainNodeSetSpec{
			Nodes: []appsv1.NodeGroupSpec{
				{
					Name:      "sentries",
					Instances: ptr.To(2),
					PDB:       &appsv1.PdbConfig{Enabled: true, MinAvailable: ptr.To(1)},
				},
			},
		},
		Status: appsv1.ChainNodeSetStatus{ChainID: "test-chain"},
	}

	r := newPdbTestReconciler(t, nodeSet)
	require.NoError(t, r.ensurePodDisruptionBudgets(context.Background(), nodeSet))

	pdb := getPdb(t, r, "default", "test-nodeset-sentries")
	require.NotNil(t, pdb, "regular group PDB should be created")
	assert.True(t, metav1.IsControlledBy(pdb, nodeSet))
}

func TestEnsurePodDisruptionBudgetsAdoptsActiveGroupPDBWithoutGroupSelector(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: types.UID("test-uid")},
		Spec: appsv1.ChainNodeSetSpec{Nodes: []appsv1.NodeGroupSpec{{
			Name: "sentries", Instances: ptr.To(2),
			PDB:                           &appsv1.PdbConfig{Enabled: true, MinAvailable: ptr.To(1)},
			IgnoreGroupOnDisruptionChecks: ptr.To(true),
		}}},
		Status: appsv1.ChainNodeSetStatus{ChainID: "test-chain"},
	}
	legacy := getPdbSpec(nodeSet, "test-nodeset-sentries", 1, map[string]string{
		controllers.LabelUpgrading:    controllers.StringValueFalse,
		controllers.LabelChainID:      "test-chain",
		controllers.LabelChainNodeSet: "test-nodeset",
	})
	r := newPdbTestReconciler(t, nodeSet)
	require.NoError(t, r.Create(context.Background(), legacy))

	require.NoError(t, r.ensurePodDisruptionBudgets(context.Background(), nodeSet))

	pdb := getPdb(t, r, "default", legacy.Name)
	require.NotNil(t, pdb)
	assert.True(t, metav1.IsControlledBy(pdb, nodeSet))
}

// TestEnsurePodDisruptionBudgetsLegacyValidator verifies that the legacy singleton
// .spec.validator.pdb behavior remains intact.
func TestEnsurePodDisruptionBudgetsLegacyValidator(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-nodeset",
			Namespace: "default",
			UID:       types.UID("test-uid"),
		},
		Spec: appsv1.ChainNodeSetSpec{
			Validator: &appsv1.NodeSetValidatorConfig{
				PDB: &appsv1.PdbConfig{Enabled: true, MinAvailable: ptr.To(0)},
			},
		},
		Status: appsv1.ChainNodeSetStatus{ChainID: "test-chain"},
	}

	r := newPdbTestReconciler(t, nodeSet)
	require.NoError(t, r.ensurePodDisruptionBudgets(context.Background(), nodeSet))

	pdb := getPdb(t, r, "default", "test-nodeset-validator")
	require.NotNil(t, pdb, "legacy validator PDB should be created")
	assert.True(t, metav1.IsControlledBy(pdb, nodeSet))
	// The selector is scoped to the legacy validator pod via the nodeset name and the reserved
	// validator group, so it does not also match validator-group pods (which share validator=true).
	assert.Equal(t, map[string]string{
		controllers.LabelUpgrading:             controllers.StringValueFalse,
		controllers.LabelChainID:               "test-chain",
		controllers.LabelChainNodeSet:          "test-nodeset",
		controllers.LabelChainNodeSetGroup:     validatorGroupName,
		controllers.LabelChainNodeSetValidator: controllers.StringValueTrue,
	}, pdb.Spec.Selector.MatchLabels)
}

// TestEnsurePodDisruptionBudgetsLegacyValidatorScopedAwayFromGroupValidators verifies that, when
// both a legacy singleton validator and a validator group define PDBs, the legacy validator PDB
// selector is scoped to the legacy validator pod only (reserved validator group) and does not
// overlap with the group validator PDB (which targets the group's own validators).
func TestEnsurePodDisruptionBudgetsLegacyValidatorScopedAwayFromGroupValidators(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-nodeset",
			Namespace: "default",
			UID:       types.UID("test-uid"),
		},
		Spec: appsv1.ChainNodeSetSpec{
			Validator: &appsv1.NodeSetValidatorConfig{
				PDB: &appsv1.PdbConfig{Enabled: true, MinAvailable: ptr.To(0)},
			},
			Nodes: []appsv1.NodeGroupSpec{
				{
					Name:      "validators",
					Instances: ptr.To(2),
					Validator: &appsv1.NodeSetValidatorConfig{
						PDB: &appsv1.PdbConfig{Enabled: true, MinAvailable: ptr.To(1)},
					},
				},
			},
		},
		Status: appsv1.ChainNodeSetStatus{ChainID: "test-chain"},
	}

	r := newPdbTestReconciler(t, nodeSet)
	require.NoError(t, r.ensurePodDisruptionBudgets(context.Background(), nodeSet))

	legacy := getPdb(t, r, "default", "test-nodeset-validator")
	require.NotNil(t, legacy, "legacy validator PDB should be created")
	// The legacy validator PDB is pinned to the reserved validator group, so it never selects the
	// group's validator pods (which carry group=validators).
	assert.Equal(t, validatorGroupName, legacy.Spec.Selector.MatchLabels[controllers.LabelChainNodeSetGroup])
	assert.NotEqual(t, "validators", legacy.Spec.Selector.MatchLabels[controllers.LabelChainNodeSetGroup])

	groupPdb := getPdb(t, r, "default", "test-nodeset-validators-validator")
	require.NotNil(t, groupPdb, "group validator PDB should be created")
	assert.Equal(t, "validators", groupPdb.Spec.Selector.MatchLabels[controllers.LabelChainNodeSetGroup])
}

func TestEnsurePodDisruptionBudgetsPreservesOwnerReferencesOnUpdate(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-nodeset",
			Namespace: "default",
			UID:       types.UID("test-uid"),
		},
		Spec: appsv1.ChainNodeSetSpec{
			Nodes: []appsv1.NodeGroupSpec{
				{
					Name:      "sentries",
					Instances: ptr.To(2),
					PDB:       &appsv1.PdbConfig{Enabled: true, MinAvailable: ptr.To(0)},
				},
			},
		},
		Status: appsv1.ChainNodeSetStatus{ChainID: "test-chain"},
	}

	r := newPdbTestReconciler(t, nodeSet)
	require.NoError(t, r.ensurePodDisruptionBudgets(context.Background(), nodeSet))

	pdb := getPdb(t, r, "default", "test-nodeset-sentries")
	require.NotNil(t, pdb)
	secondaryOwnerReference := metav1.OwnerReference{
		APIVersion: "v1",
		Kind:       "ConfigMap",
		Name:       "secondary-owner",
		UID:        types.UID("secondary-owner-uid"),
	}
	pdb.OwnerReferences = []metav1.OwnerReference{secondaryOwnerReference}
	pdb.Annotations = map[string]string{"preserve": "true"}
	require.NoError(t, r.Update(context.Background(), pdb))

	nodeSet.Spec.Nodes[0].PDB.MinAvailable = ptr.To(1)
	require.NoError(t, r.ensurePodDisruptionBudgets(context.Background(), nodeSet))

	updated := getPdb(t, r, "default", "test-nodeset-sentries")
	require.NotNil(t, updated)
	assert.Equal(t, 1, updated.Spec.MinAvailable.IntValue())
	assert.True(t, metav1.IsControlledBy(updated, nodeSet))
	assert.Contains(t, updated.OwnerReferences, secondaryOwnerReference)
	assert.Equal(t, map[string]string{"preserve": "true"}, updated.Annotations)
}

func TestEnsurePodDisruptionBudgetsAdoptsMatchingOrphan(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: types.UID("test-uid")},
		Spec: appsv1.ChainNodeSetSpec{Nodes: []appsv1.NodeGroupSpec{{
			Name: "sentries", Instances: ptr.To(2), PDB: &appsv1.PdbConfig{Enabled: true, MinAvailable: ptr.To(1)},
		}}},
		Status: appsv1.ChainNodeSetStatus{ChainID: "test-chain"},
	}
	r := newPdbTestReconciler(t, nodeSet)
	require.NoError(t, r.ensurePodDisruptionBudgets(context.Background(), nodeSet))

	pdb := getPdb(t, r, "default", "test-nodeset-sentries")
	pdb.OwnerReferences = nil
	require.NoError(t, r.Update(context.Background(), pdb))
	require.NoError(t, r.ensurePodDisruptionBudgets(context.Background(), nodeSet))

	assert.True(t, metav1.IsControlledBy(getPdb(t, r, "default", "test-nodeset-sentries"), nodeSet))
}

func TestEnsurePodDisruptionBudgetsDoesNotAdoptAmbiguousSameNamePDB(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: types.UID("test-uid")},
		Spec: appsv1.ChainNodeSetSpec{Nodes: []appsv1.NodeGroupSpec{{
			Name: "sentries", Instances: ptr.To(2), PDB: &appsv1.PdbConfig{Enabled: true, MinAvailable: ptr.To(1)},
		}}},
		Status: appsv1.ChainNodeSetStatus{ChainID: "test-chain"},
	}
	r := newPdbTestReconciler(t, nodeSet)
	maxUnavailable := intstr.FromInt32(1)
	require.NoError(t, r.Create(context.Background(), &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset-sentries", Namespace: "default"},
		Spec:       policyv1.PodDisruptionBudgetSpec{MaxUnavailable: &maxUnavailable},
	}))

	require.Error(t, r.ensurePodDisruptionBudgets(context.Background(), nodeSet))
	pdb := getPdb(t, r, "default", "test-nodeset-sentries")
	assert.Nil(t, metav1.GetControllerOf(pdb))
	assert.NotNil(t, pdb.Spec.MaxUnavailable)
	assert.Nil(t, pdb.Spec.MinAvailable)
}

func TestEnsurePodDisruptionBudgetsDoesNotDeleteForeignPDB(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: types.UID("test-uid")},
		Spec:       appsv1.ChainNodeSetSpec{Nodes: []appsv1.NodeGroupSpec{{Name: "sentries", Instances: ptr.To(2)}}},
		Status:     appsv1.ChainNodeSetStatus{ChainID: "test-chain"},
	}
	r := newPdbTestReconciler(t, nodeSet)
	foreignController := true
	require.NoError(t, r.Create(context.Background(), &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-nodeset-sentries", Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "Deployment", Name: "foreign", UID: types.UID("foreign-uid"), Controller: &foreignController,
			}},
		},
	}))

	require.NoError(t, r.ensurePodDisruptionBudgets(context.Background(), nodeSet))
	assert.NotNil(t, getPdb(t, r, "default", "test-nodeset-sentries"))
}

func TestEnsurePodDisruptionBudgetsDeletesRemovedGroupPDB(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: types.UID("test-uid")},
		Status:     appsv1.ChainNodeSetStatus{ChainID: "test-chain"},
	}
	r := newPdbTestReconciler(t, nodeSet)
	stale := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset-removed", Namespace: "default"},
		Spec: policyv1.PodDisruptionBudgetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{
			controllers.LabelChainNodeSet:      nodeSet.Name,
			controllers.LabelChainNodeSetGroup: "removed",
		}}},
	}
	require.NoError(t, controllerutil.SetControllerReference(nodeSet, stale, r.Scheme))
	require.NoError(t, r.Create(context.Background(), stale))

	require.NoError(t, r.ensurePodDisruptionBudgets(context.Background(), nodeSet))
	assert.Nil(t, getPdb(t, r, "default", "test-nodeset-removed"))
}

func TestEnsurePodDisruptionBudgetsDoesNotDeleteRemovedGroupPDBOwnedByAnotherController(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: types.UID("test-uid")},
		Status:     appsv1.ChainNodeSetStatus{ChainID: "test-chain"},
	}
	r := newPdbTestReconciler(t, nodeSet)
	foreignController := true
	require.NoError(t, r.Create(context.Background(), &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-nodeset-removed", Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "Deployment", Name: "foreign", UID: types.UID("foreign-uid"), Controller: &foreignController,
			}},
		},
		Spec: policyv1.PodDisruptionBudgetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{
			controllers.LabelChainNodeSet:      nodeSet.Name,
			controllers.LabelChainNodeSetGroup: "removed",
		}}},
	}))

	require.NoError(t, r.ensurePodDisruptionBudgets(context.Background(), nodeSet))
	assert.NotNil(t, getPdb(t, r, "default", "test-nodeset-removed"))
}

func TestEnsurePodDisruptionBudgetsDoesNotDeleteSupplementalPDB(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: types.UID("test-uid")},
		Status:     appsv1.ChainNodeSetStatus{ChainID: "test-chain"},
	}
	r := newPdbTestReconciler(t, nodeSet)
	require.NoError(t, r.Create(context.Background(), &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "custom-supplemental", Namespace: "default"},
		Spec: policyv1.PodDisruptionBudgetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{
			controllers.LabelChainNodeSet:      nodeSet.Name,
			controllers.LabelChainNodeSetGroup: "removed",
		}}},
	}))

	require.NoError(t, r.ensurePodDisruptionBudgets(context.Background(), nodeSet))
	assert.NotNil(t, getPdb(t, r, "default", "custom-supplemental"))
}

func TestEnsurePodDisruptionBudgetsPreservesAmbiguousRemovedGroupPDBWithoutGroupSelector(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: types.UID("test-uid")},
		Status:     appsv1.ChainNodeSetStatus{ChainID: "test-chain"},
	}
	r := newPdbTestReconciler(t, nodeSet)
	require.NoError(t, r.Create(context.Background(), &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset-removed", Namespace: "default"},
		Spec: policyv1.PodDisruptionBudgetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{
			controllers.LabelChainNodeSet: nodeSet.Name,
		}}},
	}))

	require.NoError(t, r.ensurePodDisruptionBudgets(context.Background(), nodeSet))
	assert.NotNil(t, getPdb(t, r, "default", "test-nodeset-removed"))
}

func TestFinalizePodDisruptionBudgetsDeletesOwnedAndHistoricalSingleton(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-nodeset", Namespace: "default", UID: types.UID("test-uid"),
			Finalizers: []string{podDisruptionBudgetFinalizer},
		},
		Status: appsv1.ChainNodeSetStatus{ChainID: "test-chain"},
	}
	r := newPdbTestReconciler(t, nodeSet)
	owned := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset-removed", Namespace: "default", UID: "owned-uid"}}
	require.NoError(t, controllerutil.SetControllerReference(nodeSet, owned, r.Scheme))
	require.NoError(t, r.Create(context.Background(), owned))
	require.NoError(t, r.Create(context.Background(), &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset-validator", Namespace: "default", UID: "legacy-uid"},
		Spec: policyv1.PodDisruptionBudgetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{
			controllers.LabelUpgrading:             controllers.StringValueFalse,
			controllers.LabelChainID:               nodeSet.Status.ChainID,
			controllers.LabelChainNodeSetValidator: controllers.StringValueTrue,
		}}},
	}))
	supplemental := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset-supplemental", Namespace: "default", UID: "supplemental-uid"}}
	require.NoError(t, r.Create(context.Background(), supplemental))
	r.Client = &pdbUIDGuardClient{Client: r.Client}

	done, err := r.finalizePodDisruptionBudgets(context.Background(), nodeSet)
	require.NoError(t, err)
	assert.True(t, done)
	assert.Nil(t, getPdb(t, r, "default", "test-nodeset-removed"))
	assert.Nil(t, getPdb(t, r, "default", "test-nodeset-validator"))
	assert.NotNil(t, getPdb(t, r, "default", supplemental.Name))
	current := &appsv1.ChainNodeSet{}
	require.NoError(t, r.Get(context.Background(), client.ObjectKeyFromObject(nodeSet), current))
	assert.NotContains(t, current.Finalizers, podDisruptionBudgetFinalizer)
}

func TestFinalizePodDisruptionBudgetsPreservesForeignHistoricalSingleton(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: types.UID("test-uid"), Finalizers: []string{podDisruptionBudgetFinalizer}},
		Status:     appsv1.ChainNodeSetStatus{ChainID: "test-chain"},
	}
	r := newPdbTestReconciler(t, nodeSet)
	foreignController := true
	foreign := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset-validator", Namespace: "default", UID: "foreign-uid", OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "apps/v1", Kind: "Deployment", Name: "foreign", UID: "foreign-controller-uid", Controller: &foreignController,
		}}},
		Spec: policyv1.PodDisruptionBudgetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{
			controllers.LabelUpgrading:             controllers.StringValueFalse,
			controllers.LabelChainID:               nodeSet.Status.ChainID,
			controllers.LabelChainNodeSetValidator: controllers.StringValueTrue,
		}}},
	}
	require.NoError(t, r.Create(context.Background(), foreign))

	done, err := r.finalizePodDisruptionBudgets(context.Background(), nodeSet)
	require.NoError(t, err)
	assert.True(t, done)
	assert.NotNil(t, getPdb(t, r, "default", foreign.Name))
}

type pdbUIDGuardClient struct {
	client.Client
}

func (c *pdbUIDGuardClient) Delete(ctx context.Context, object client.Object, opts ...client.DeleteOption) error {
	if _, ok := object.(*policyv1.PodDisruptionBudget); ok {
		options := &client.DeleteOptions{}
		for _, option := range opts {
			option.ApplyToDelete(options)
		}
		if options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != object.GetUID() {
			return fmt.Errorf("PDB deletion requires an exact UID precondition")
		}
	}
	return c.Client.Delete(ctx, object, opts...)
}
