package cosmosigner

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestSignerPodDisruptionBudgetTeardown(t *testing.T) {
	for _, owned := range []bool{true, false} {
		t.Run(map[bool]string{true: "owned", false: "foreign"}[owned], func(t *testing.T) {
			ctx := context.Background()
			owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: "default", UID: "owner-uid"}}
			pdb := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: "mychain-signer", Namespace: "default"}}
			if owned {
				pdb.OwnerReferences = []metav1.OwnerReference{ownerRef(owner)}
			}
			c := fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(pdb).Build()
			done, err := IsTornDown(ctx, c, owner, pdb.Namespace, pdb.Name)
			require.NoError(t, err)
			require.Equal(t, !owned, done)
			require.NoError(t, Undeploy(ctx, c, owner, pdb.Namespace, pdb.Name))
			err = c.Get(ctx, client.ObjectKeyFromObject(pdb), &policyv1.PodDisruptionBudget{})
			if owned {
				require.True(t, apierrors.IsNotFound(err))
			} else {
				require.NoError(t, err)
			}
			done, err = IsTornDown(ctx, c, owner, pdb.Namespace, pdb.Name)
			require.NoError(t, err)
			require.True(t, done)
		})
	}
}

func TestPodDisruptionBudgetShape(t *testing.T) {
	for _, replicas := range []int32{1, 3} {
		p := testParams()
		p.Replicas = replicas
		pdb := p.PodDisruptionBudget()
		require.Equal(t, p.Name, pdb.Name)
		require.Equal(t, p.Namespace, pdb.Namespace)
		require.Equal(t, intstr.FromInt32(1), *pdb.Spec.MaxUnavailable)
		require.Nil(t, pdb.Spec.MinAvailable)
		require.Equal(t, InstanceLabels(p.Name), pdb.Spec.Selector.MatchLabels)
		require.Equal(t, policyv1.AlwaysAllow, *pdb.Spec.UnhealthyPodEvictionPolicy)
		require.Equal(t, "mychain", pdb.Labels["chain-node-set"])
		require.Equal(t, "cosmosigner", pdb.Labels["app.kubernetes.io/name"])
		require.Equal(t, p.Name, pdb.Labels["app.kubernetes.io/instance"])
	}
}

func TestApplyOwnedPodDisruptionBudget(t *testing.T) {
	ctx := context.Background()
	scheme := lockScheme(t)
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: "default", UID: "owner-uid"}}
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	desired := testParams().PodDisruptionBudget()
	require.NoError(t, ApplyOwned(ctx, c, scheme, owner, desired))
	live := &policyv1.PodDisruptionBudget{}
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(desired), live))
	require.True(t, metav1.IsControlledBy(live, owner))
	require.Equal(t, intstr.FromInt32(1), *live.Spec.MaxUnavailable)

	desired = testParams().PodDisruptionBudget()
	desired.Spec.MaxUnavailable = ptr.To(intstr.FromInt32(2))
	require.NoError(t, ApplyOwned(ctx, c, scheme, owner, desired))
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(desired), live))
	require.Equal(t, intstr.FromInt32(2), *live.Spec.MaxUnavailable)
}

func TestApplyOwnedRefusesForeignPodDisruptionBudget(t *testing.T) {
	ctx := context.Background()
	scheme := lockScheme(t)
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: "default", UID: "owner-uid"}}
	pdb := testParams().PodDisruptionBudget()
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pdb).Build()
	require.EqualError(t, ApplyOwned(ctx, c, scheme, owner, testParams().PodDisruptionBudget()),
		`cosmosigner resource "mychain-signer" is managed by another owner; refusing to overwrite it — rename the ChainNode/ChainNodeSet to avoid the name collision`)
	live := &policyv1.PodDisruptionBudget{}
	require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(pdb), live))
	require.Empty(t, live.OwnerReferences)
	require.Equal(t, pdb.Spec, live.Spec)
}
