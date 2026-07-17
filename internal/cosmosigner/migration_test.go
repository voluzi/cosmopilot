package cosmosigner

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	cosmopilotv1 "github.com/voluzi/cosmopilot/v2/api/v1"
)

func TestReconcileStatefulSetMigrationWaitsForTerminatingPod(t *testing.T) {
	scheme := lockScheme(t)
	const ns, name = "default", "mychain-signer"
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: ns, UID: types.UID("owner-uid")}}
	zero := int32(0)
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns, UID: types.UID("signer-sts-uid"), Generation: 2,
			OwnerReferences: []metav1.OwnerReference{ownerRef(owner)},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &zero,
			PersistentVolumeClaimRetentionPolicy: &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
				WhenDeleted: appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
				WhenScaled:  appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
			},
		},
		Status: appsv1.StatefulSetStatus{
			ObservedGeneration: 2,
			Replicas:           0,
		},
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: name + "-0", Namespace: ns,
		DeletionTimestamp: &metav1.Time{Time: time.Now()},
		Finalizers:        []string{"cosmopilot.voluzi.com/test-hold"},
		OwnerReferences:   []metav1.OwnerReference{ownerRef(sts)},
	}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sts, pod).Build()

	ready, next, err := ReconcileStatefulSetMigration(
		context.Background(), c, owner, ns, name,
		cosmopilotv1.CosmosignerMigrationQuiescing, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if ready || next != cosmopilotv1.CosmosignerMigrationQuiescing {
		t.Fatalf("migration advanced with a terminating pod: ready=%v next=%q", ready, next)
	}
	if err := c.Get(context.Background(), clientKey(ns, name), &appsv1.StatefulSet{}); err != nil {
		t.Fatalf("StatefulSet must remain while its pod terminates: %v", err)
	}
}

func TestReconcileStatefulSetMigrationRetainsPVCsBeforeQuiescing(t *testing.T) {
	scheme := lockScheme(t)
	const ns, name = "default", "mychain-signer"
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: ns, UID: types.UID("owner-uid")}}
	one := int32(1)
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns, Generation: 1,
			OwnerReferences: []metav1.OwnerReference{ownerRef(owner)},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &one,
			PersistentVolumeClaimRetentionPolicy: &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
				WhenDeleted: appsv1.DeletePersistentVolumeClaimRetentionPolicyType,
				WhenScaled:  appsv1.DeletePersistentVolumeClaimRetentionPolicyType,
			},
		},
		Status: appsv1.StatefulSetStatus{ObservedGeneration: 1, Replicas: 1},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sts).Build()

	ready, next, err := ReconcileStatefulSetMigration(
		context.Background(), c, owner, ns, name,
		cosmopilotv1.CosmosignerMigrationQuiescing, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if ready || next != cosmopilotv1.CosmosignerMigrationQuiescing {
		t.Fatalf("migration advanced while enabling PVC retention: ready=%v next=%q", ready, next)
	}
	retained := &appsv1.StatefulSet{}
	if err := c.Get(context.Background(), clientKey(ns, name), retained); err != nil {
		t.Fatal(err)
	}
	if retained.Spec.Replicas == nil || *retained.Spec.Replicas != 1 {
		t.Fatalf("migration scaled before PVC retention was enabled: replicas=%v", retained.Spec.Replicas)
	}
	if retained.Spec.PersistentVolumeClaimRetentionPolicy == nil ||
		retained.Spec.PersistentVolumeClaimRetentionPolicy.WhenDeleted != appsv1.RetainPersistentVolumeClaimRetentionPolicyType ||
		retained.Spec.PersistentVolumeClaimRetentionPolicy.WhenScaled != appsv1.RetainPersistentVolumeClaimRetentionPolicyType {
		t.Fatalf("migration did not retain PVCs before quiescing: %#v", retained.Spec.PersistentVolumeClaimRetentionPolicy)
	}
}

func TestReconcileStatefulSetMigrationRetainsOrResetsPVCs(t *testing.T) {
	const ns, name = "default", "mychain-signer"
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: ns, UID: types.UID("owner-uid")}}
	pvc := func() *corev1.PersistentVolumeClaim {
		return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
			Name: dataVolumeName + "-" + name + "-0", Namespace: ns, Labels: pvcOwnerLabels(name, owner.UID),
		}}
	}

	for _, tc := range []struct {
		name      string
		reset     bool
		wantClaim bool
	}{
		{name: "same key retains state", reset: false, wantClaim: true},
		{name: "different key resets state", reset: true, wantClaim: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claim := pvc()
			c := fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(claim).Build()
			ready, next, err := ReconcileStatefulSetMigration(
				context.Background(), c, owner, ns, name,
				cosmopilotv1.CosmosignerMigrationResettingState, tc.reset,
			)
			if err != nil {
				t.Fatal(err)
			}
			if ready || next != cosmopilotv1.CosmosignerMigrationRecreating {
				t.Fatalf("reset phase did not advance to recreation: ready=%v next=%q", ready, next)
			}
			err = c.Get(context.Background(), clientKey(ns, claim.Name), &corev1.PersistentVolumeClaim{})
			if tc.wantClaim && err != nil {
				t.Fatalf("same-key migration deleted retained state: %v", err)
			}
			if !tc.wantClaim && err == nil {
				t.Fatal("different-key migration retained stale state")
			}
		})
	}
}

func TestReconcileStatefulSetMigrationRechecksStatefulSetAbsenceBeforeRecreation(t *testing.T) {
	scheme := lockScheme(t)
	const ns, name = "default", "mychain-signer"
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: ns, UID: types.UID("owner-uid")}}
	zero := int32(0)
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns, Generation: 1,
			Finalizers:      []string{"cosmopilot.voluzi.com/test-hold"},
			OwnerReferences: []metav1.OwnerReference{ownerRef(owner)},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &zero,
			PersistentVolumeClaimRetentionPolicy: &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
				WhenDeleted: appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
				WhenScaled:  appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
			},
		},
		Status: appsv1.StatefulSetStatus{ObservedGeneration: 1},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sts).Build()

	ready, next, err := ReconcileStatefulSetMigration(
		context.Background(), c, owner, ns, name,
		cosmopilotv1.CosmosignerMigrationRecreating, false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if ready || next != cosmopilotv1.CosmosignerMigrationRecreating {
		t.Fatalf("migration recreated before confirming StatefulSet absence: ready=%v next=%q", ready, next)
	}
}

func clientKey(namespace, name string) types.NamespacedName {
	return types.NamespacedName{Namespace: namespace, Name: name}
}
