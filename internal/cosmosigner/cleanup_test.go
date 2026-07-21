package cosmosigner

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func TestProtectRetainedStatePVCsUpgradesOnlyVerifiedClaims(t *testing.T) {
	const namespace, name = "default", "mychain-signer"
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: namespace, UID: types.UID("owner-uid")}}
	bound := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: dataVolumeName + "-" + name + "-0", Namespace: namespace, Labels: pvcOwnerLabels(name, owner.UID),
	}, Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "volume-0"}, Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound}}
	pending := bound.DeepCopy()
	pending.Name = dataVolumeName + "-" + name + "-1"
	pending.Spec.VolumeName = ""
	pending.Status.Phase = corev1.ClaimPending
	foreign := bound.DeepCopy()
	foreign.Name = dataVolumeName + "-" + name + "-2"
	foreign.Labels = pvcOwnerLabels(name, "other-uid")
	c := fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(bound, pending, foreign).Build()

	changed, err := ProtectRetainedStatePVCs(context.Background(), c, owner, namespace)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("a verified legacy claim must be protected")
	}
	for _, tc := range []struct {
		name string
		want bool
	}{{bound.Name, true}, {pending.Name, false}, {foreign.Name, false}} {
		fresh := &corev1.PersistentVolumeClaim{}
		if err := c.Get(context.Background(), client.ObjectKey{Namespace: namespace, Name: tc.name}, fresh); err != nil {
			t.Fatal(err)
		}
		if got := controllerutil.ContainsFinalizer(fresh, RetainedStateFinalizer); got != tc.want {
			t.Fatalf("claim %q protected = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestFinalizeOwnerWaitsForSignerThenDeletesRetainedClaims(t *testing.T) {
	const namespace, name = "default", "mychain-signer"
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: namespace, UID: types.UID("owner-uid")}}
	zero := int32(0)
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: namespace, OwnerReferences: []metav1.OwnerReference{{UID: owner.UID, Controller: boolPointer(true)}},
	}, Spec: appsv1.StatefulSetSpec{
		Replicas: &zero,
		Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: InstanceLabels(name)}},
	}, Status: appsv1.StatefulSetStatus{ObservedGeneration: 0, Replicas: 0}}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: dataVolumeName + "-" + name + "-0", Namespace: namespace,
		Labels: pvcOwnerLabels(name, owner.UID), Finalizers: []string{RetainedStateFinalizer},
	}}
	c := fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(sts, pvc).Build()

	done, err := FinalizeOwner(context.Background(), c, owner, namespace)
	if err != nil {
		t.Fatal(err)
	}
	if done {
		t.Fatal("finalization must observe StatefulSet deletion before releasing claims")
	}
	for attempts := 0; attempts < 4 && !done; attempts++ {
		done, err = FinalizeOwner(context.Background(), c, owner, namespace)
		if err != nil {
			t.Fatal(err)
		}
	}
	if !done {
		t.Fatal("owner finalization did not remove retained signer state")
	}
}

func TestHasOwnedSignerStateIgnoresNonSignerStatefulSet(t *testing.T) {
	const namespace = "default"
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: namespace, UID: types.UID("owner-uid")}}
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
		Name: "application", Namespace: namespace,
		OwnerReferences: []metav1.OwnerReference{{UID: owner.UID, Controller: boolPointer(true)}},
	}}
	c := fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(sts).Build()

	found, err := HasOwnedSignerState(context.Background(), c, owner, namespace)
	if err != nil {
		t.Fatal(err)
	}
	if found {
		t.Fatal("a non-signer StatefulSet must not arm Cosmosigner owner cleanup")
	}
}

func TestFinalizeOwnerIgnoresNonSignerStatefulSet(t *testing.T) {
	const namespace = "default"
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: namespace, UID: types.UID("owner-uid")}}
	sts := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
		Name: "application", Namespace: namespace,
		OwnerReferences: []metav1.OwnerReference{{UID: owner.UID, Controller: boolPointer(true)}},
	}}
	c := fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(sts).Build()

	done, err := FinalizeOwner(context.Background(), c, owner, namespace)
	if err != nil {
		t.Fatal(err)
	}
	if !done {
		t.Fatal("non-signer StatefulSets must not delay Cosmosigner owner cleanup")
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(sts), &appsv1.StatefulSet{}); err != nil {
		t.Fatalf("non-signer StatefulSet was modified or deleted: %v", err)
	}
}

func boolPointer(v bool) *bool { return &v }

func TestDeletePVCsReleasesRetainedStateFinalizer(t *testing.T) {
	const namespace, name = "default", "mychain-signer"
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: namespace, UID: types.UID("owner-uid")}}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: dataVolumeName + "-" + name + "-0", Namespace: namespace,
		Labels: pvcOwnerLabels(name, owner.UID), Finalizers: []string{RetainedStateFinalizer},
	}}
	c := fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(pvc).Build()

	if err := DeletePVCs(context.Background(), c, owner, namespace, name); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(pvc), &corev1.PersistentVolumeClaim{}); !apierrors.IsNotFound(err) {
		t.Fatalf("retained state PVC must be deleted after its finalizer is released, got %v", err)
	}
}
