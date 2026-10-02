package cosmosigner

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
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

func TestEnsurePodDisruptionBudgetSkipsForeignOverlap(t *testing.T) {
	for _, tc := range []struct {
		name     string
		selector *metav1.LabelSelector
		skipped  bool
	}{
		{"mychain-signer", &metav1.LabelSelector{MatchLabels: map[string]string{"other": "pods"}}, true},
		{"external", &metav1.LabelSelector{MatchLabels: InstanceLabels("mychain-signer")}, true},
		{"expression", &metav1.LabelSelector{MatchExpressions: []metav1.LabelSelectorRequirement{{Key: "app.kubernetes.io/name", Operator: metav1.LabelSelectorOpIn, Values: []string{"cosmosigner"}}}}, true},
		{"unrelated", &metav1.LabelSelector{MatchLabels: InstanceLabels("other-signer")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			scheme := lockScheme(t)
			owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: "default", UID: "owner-uid"}}
			foreign := &policyv1.PodDisruptionBudget{ObjectMeta: metav1.ObjectMeta{Name: tc.name, Namespace: "default"}, Spec: policyv1.PodDisruptionBudgetSpec{Selector: tc.selector}}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(foreign).Build()
			skipped, err := EnsurePodDisruptionBudget(ctx, c, scheme, owner, testParams().PodDisruptionBudget())
			require.NoError(t, err)
			require.Equal(t, tc.skipped, skipped)
			live := &policyv1.PodDisruptionBudget{}
			require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(foreign), live))
			require.Empty(t, live.OwnerReferences)
			require.Equal(t, foreign.Spec, live.Spec)
			if tc.skipped && tc.name != "mychain-signer" {
				require.True(t, apierrors.IsNotFound(c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "mychain-signer"}, &policyv1.PodDisruptionBudget{})))
			}
			if !tc.skipped {
				require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "mychain-signer"}, live))
				require.True(t, metav1.IsControlledBy(live, owner))
				desired := testParams().PodDisruptionBudget()
				desired.Spec.MaxUnavailable = ptr.To(intstr.FromInt32(2))
				skipped, err = EnsurePodDisruptionBudget(ctx, c, scheme, owner, desired)
				require.NoError(t, err)
				require.False(t, skipped)
				require.NoError(t, c.Get(ctx, client.ObjectKeyFromObject(desired), live))
				require.Equal(t, intstr.FromInt32(2), *live.Spec.MaxUnavailable)
			}
		})
	}
}

func TestEnsurePodDisruptionBudgetUnavailable(t *testing.T) {
	for _, op := range []string{"list", "create"} {
		for _, denied := range []error{
			apierrors.NewForbidden(schema.GroupResource{Group: "policy", Resource: "poddisruptionbudgets"}, "mychain-signer", fmt.Errorf("denied")),
			&meta.NoKindMatchError{GroupKind: schema.GroupKind{Group: "policy", Kind: "PodDisruptionBudget"}},
			fmt.Errorf("connection lost"),
		} {
			t.Run(op+"/"+denied.Error(), func(t *testing.T) {
				scheme := lockScheme(t)
				c := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
					List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
						if op == "list" {
							return denied
						}
						return c.List(ctx, list, opts...)
					},
					Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
						return denied
					},
				}).Build()
				owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: "default", UID: "owner-uid"}}
				skipped, err := EnsurePodDisruptionBudget(context.Background(), c, scheme, owner, testParams().PodDisruptionBudget())
				if apierrors.IsForbidden(denied) || meta.IsNoMatchError(denied) {
					require.NoError(t, err)
					require.True(t, skipped)
				} else {
					require.ErrorIs(t, err, denied)
					require.False(t, skipped)
				}
			})
		}
	}
}
