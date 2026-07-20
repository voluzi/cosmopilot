package cosmosigner

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// TestIsRolledOut covers the rollout-gating logic that arms the signing-identity digest: only a
// StatefulSet whose CURRENT generation is observed and fully updated+ready counts as rolled out, so
// readiness left over from a previous revision (or a partial rollout) never locks in a pending
// signing change.
func TestIsRolledOut(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	const (
		ns       = "default"
		name     = "mychain-signer"
		replicas = int32(3)
	)

	sts := func(generation, observed int64, updated, ready int32) *appsv1.StatefulSet {
		return &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Generation: generation},
			Status: appsv1.StatefulSetStatus{
				ObservedGeneration: observed,
				UpdatedReplicas:    updated,
				ReadyReplicas:      ready,
			},
		}
	}

	tests := []struct {
		name string
		obj  *appsv1.StatefulSet
		want bool
	}{
		{"fully rolled out", sts(2, 2, replicas, replicas), true},
		{"missing statefulset", nil, false},
		{"generation not yet observed (pending change)", sts(3, 2, replicas, replicas), false},
		{"replicas ready but not updated (previous revision readiness)", sts(2, 2, 1, replicas), false},
		{"replicas updated but not ready (crashlooping new revision)", sts(2, 2, replicas, 1), false},
		{"fresh create, nothing ready", sts(1, 1, 0, 0), false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			builder := fake.NewClientBuilder().WithScheme(scheme)
			if tc.obj != nil {
				builder = builder.WithObjects(tc.obj)
			}
			c := builder.Build()

			got, err := IsRolledOut(context.Background(), c, ns, name, replicas)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("IsRolledOut = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestIsRolledOutPropagatesErrors verifies non-NotFound errors are surfaced, not treated as
// not-rolled-out.
func TestIsRolledOutPropagatesErrors(t *testing.T) {
	scheme := runtime.NewScheme() // StatefulSet NOT registered → Get returns a non-NotFound error
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	if _, err := IsRolledOut(context.Background(), c, "default", "x", 1); err == nil {
		t.Fatal("expected an error for an unregistered scheme, got nil")
	}
}

func TestScaleDownWaitsForAllSignerPodsGone(t *testing.T) {
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

	quiesced, err := ScaleDown(context.Background(), c, owner, ns, name)
	if err != nil {
		t.Fatal(err)
	}
	if quiesced {
		t.Fatal("a terminating signer pod must block quiescence even when StatefulSet status reports zero replicas")
	}
}

func TestScaleDownWaitsForSignerPodAfterStatefulSetIsGone(t *testing.T) {
	scheme := lockScheme(t)
	const ns, name = "default", "mychain-signer"
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: ns, UID: types.UID("owner-uid")}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: name + "-0", Namespace: ns,
		DeletionTimestamp: &metav1.Time{Time: time.Now()},
		Finalizers:        []string{"cosmopilot.voluzi.com/test-hold"},
	}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()

	quiesced, err := ScaleDown(context.Background(), c, owner, ns, name)
	if err != nil {
		t.Fatal(err)
	}
	if quiesced {
		t.Fatal("a terminating signer pod must block quiescence after the StatefulSet is gone")
	}
}

func TestDeleteStatefulSetWaitsForAllSignerPodsGone(t *testing.T) {
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

	deleted, err := DeleteStatefulSet(context.Background(), c, owner, ns, name)
	if err != nil {
		t.Fatal(err)
	}
	if deleted {
		t.Fatal("StatefulSet deletion must remain pending while a terminating signer pod exists")
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(sts), &appsv1.StatefulSet{}); err != nil {
		t.Fatalf("StatefulSet must not be deleted before every signer pod is gone: %v", err)
	}
}

func TestDeleteStatefulSetEnablesPVCRetentionBeforeDeleting(t *testing.T) {
	scheme := lockScheme(t)
	const ns, name = "default", "mychain-signer"
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: ns, UID: types.UID("owner-uid")}}
	zero := int32(0)
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns, Generation: 1,
			OwnerReferences: []metav1.OwnerReference{ownerRef(owner)},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &zero,
			PersistentVolumeClaimRetentionPolicy: &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
				WhenDeleted: appsv1.DeletePersistentVolumeClaimRetentionPolicyType,
				WhenScaled:  appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
			},
		},
		Status: appsv1.StatefulSetStatus{ObservedGeneration: 1},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sts).Build()

	deleted, err := DeleteStatefulSet(context.Background(), c, owner, ns, name)
	if err != nil {
		t.Fatal(err)
	}
	if deleted {
		t.Fatal("StatefulSet must not be deleted in the same step that enables PVC retention")
	}
	retained := &appsv1.StatefulSet{}
	if err := c.Get(context.Background(), clientKey(ns, name), retained); err != nil {
		t.Fatal(err)
	}
	if retained.Spec.PersistentVolumeClaimRetentionPolicy == nil ||
		retained.Spec.PersistentVolumeClaimRetentionPolicy.WhenDeleted != appsv1.RetainPersistentVolumeClaimRetentionPolicyType {
		t.Fatalf("migration deletion did not enable PVC retention: %#v", retained.Spec.PersistentVolumeClaimRetentionPolicy)
	}
}

func TestDeleteStatefulSetEnablesScaleRetentionBeforeScaling(t *testing.T) {
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

	deleted, err := DeleteStatefulSet(context.Background(), c, owner, ns, name)
	if err != nil {
		t.Fatal(err)
	}
	if deleted {
		t.Fatal("StatefulSet must not be deleted while PVC retention is being enabled")
	}
	retained := &appsv1.StatefulSet{}
	if err := c.Get(context.Background(), clientKey(ns, name), retained); err != nil {
		t.Fatal(err)
	}
	if ptr.Deref(retained.Spec.Replicas, 0) != 1 {
		t.Fatalf("StatefulSet scaled before WhenScaled retention was enabled: replicas=%d", ptr.Deref(retained.Spec.Replicas, 0))
	}
	if retained.Spec.PersistentVolumeClaimRetentionPolicy == nil ||
		retained.Spec.PersistentVolumeClaimRetentionPolicy.WhenDeleted != appsv1.RetainPersistentVolumeClaimRetentionPolicyType ||
		retained.Spec.PersistentVolumeClaimRetentionPolicy.WhenScaled != appsv1.RetainPersistentVolumeClaimRetentionPolicyType {
		t.Fatalf("migration deletion did not enable both PVC retention policies: %#v", retained.Spec.PersistentVolumeClaimRetentionPolicy)
	}
}

func TestDeleteStatefulSetWaitsForRetentionGenerationBeforeScaling(t *testing.T) {
	scheme := lockScheme(t)
	const ns, name = "default", "mychain-signer"
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: ns, UID: types.UID("owner-uid")}}
	one := int32(1)
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns, Generation: 2,
			OwnerReferences: []metav1.OwnerReference{ownerRef(owner)},
		},
		Spec: appsv1.StatefulSetSpec{
			Replicas: &one,
			PersistentVolumeClaimRetentionPolicy: &appsv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
				WhenDeleted: appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
				WhenScaled:  appsv1.RetainPersistentVolumeClaimRetentionPolicyType,
			},
		},
		Status: appsv1.StatefulSetStatus{ObservedGeneration: 1, Replicas: 1},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sts).Build()

	deleted, err := DeleteStatefulSet(context.Background(), c, owner, ns, name)
	if err != nil {
		t.Fatal(err)
	}
	if deleted {
		t.Fatal("StatefulSet must not be deleted before the retention generation is observed")
	}
	remaining := &appsv1.StatefulSet{}
	if err := c.Get(context.Background(), clientKey(ns, name), remaining); err != nil {
		t.Fatal(err)
	}
	if ptr.Deref(remaining.Spec.Replicas, 0) != 1 {
		t.Fatalf("StatefulSet scaled before the retention generation was observed: replicas=%d", ptr.Deref(remaining.Spec.Replicas, 0))
	}
}

func TestDiscoveryEndpointsGoneWaitsForEndpointSlices(t *testing.T) {
	const ns, name = "default", "mychain-signer"
	slice := &discoveryv1.EndpointSlice{ObjectMeta: metav1.ObjectMeta{
		Name: name + "-privval-stale", Namespace: ns,
		Labels: map[string]string{discoveryv1.LabelServiceName: name + discoveryServiceSuffix},
	}}
	c := fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(slice).Build()

	gone, err := DiscoveryEndpointsGone(context.Background(), c, ns, name)
	if err != nil {
		t.Fatal(err)
	}
	if gone {
		t.Fatal("a stale discovery EndpointSlice must block signer recreation")
	}
	if err := c.Delete(context.Background(), slice); err != nil {
		t.Fatal(err)
	}
	gone, err = DiscoveryEndpointsGone(context.Background(), c, ns, name)
	if err != nil {
		t.Fatal(err)
	}
	if !gone {
		t.Fatal("discovery endpoints should be gone after the stale EndpointSlice is deleted")
	}
}

// fakeOwner builds a minimal client.Object carrying a name/UID, usable both as an owner argument and
// as a target of metav1.IsControlledBy. PartialObjectMetadata implements client.Object.
func fakeOwner(name string, uid types.UID) *metav1.PartialObjectMetadata {
	return &metav1.PartialObjectMetadata{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: uid}}
}

func ownerRef(o metav1.Object) metav1.OwnerReference {
	return metav1.OwnerReference{APIVersion: "v1", Kind: "Owner", Name: o.GetName(), UID: o.GetUID(), Controller: ptrBool(true)}
}

func ptrBool(b bool) *bool { return &b }

// TestIsTornDownOwnerScoping verifies IsTornDown gates only on OUR resources: our own StatefulSet or
// our own lingering per-pod PVCs (matched by owner-UID label) block completion, while a same-name
// StatefulSet or PVC owned by another CR does not.
func TestIsTornDownOwnerScoping(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	const (
		ns   = "default"
		name = "mychain-signer"
	)
	me := fakeOwner("me", types.UID("me-uid"))
	other := fakeOwner("other", types.UID("other-uid"))

	ownedSTS := func(owner metav1.Object) *appsv1.StatefulSet {
		return &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns, OwnerReferences: []metav1.OwnerReference{ownerRef(owner)},
		}}
	}
	policy := func(owner metav1.Object) client.Object {
		obj := networkPolicyObject(ns, name)
		obj.SetOwnerReferences([]metav1.OwnerReference{ownerRef(owner)})
		return obj
	}
	pvc := func(ownerUID types.UID) *corev1.PersistentVolumeClaim {
		return &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
			Name: dataVolumeName + "-" + name + "-0", Namespace: ns, Labels: pvcOwnerLabels(name, ownerUID),
		}}
	}

	tests := []struct {
		name string
		objs []client.Object
		want bool
	}{
		{"nothing present → torn down", nil, true},
		{"our statefulset present → not torn down", []client.Object{ownedSTS(me)}, false},
		{"foreign statefulset only → torn down", []client.Object{ownedSTS(other)}, true},
		{"our import pod present → not torn down", []client.Object{&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name + "-" + importJobSuffix, Namespace: ns, OwnerReferences: []metav1.OwnerReference{ownerRef(me)}}}}, false},
		{"foreign import pod only → torn down", []client.Object{&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name + "-" + importJobSuffix, Namespace: ns, OwnerReferences: []metav1.OwnerReference{ownerRef(other)}}}}, true},
		{"our network policy present → not torn down", []client.Object{policy(me)}, false},
		{"foreign network policy only → torn down", []client.Object{policy(other)}, true},
		{"signer replica pod present → not torn down", []client.Object{&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name + "-0", Namespace: ns}}}, false},
		{"our lingering pvc → not torn down", []client.Object{pvc("me-uid")}, false},
		{"foreign pvc only → torn down", []client.Object{pvc("other-uid")}, true},
		{"foreign statefulset + our lingering pvc → not torn down", []client.Object{ownedSTS(other), pvc("me-uid")}, false},
		{"foreign statefulset + foreign pvc → torn down", []client.Object{ownedSTS(other), pvc("other-uid")}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tc.objs...).Build()
			got, err := IsTornDown(context.Background(), c, me, ns, name)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("IsTornDown = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestUndeployDeletesOwnedImportPod(t *testing.T) {
	scheme := lockScheme(t)
	const ns, name = "default", "mychain-signer"
	me := fakeOwner("me", types.UID("me-uid"))
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: name + "-" + importJobSuffix, Namespace: ns, OwnerReferences: []metav1.OwnerReference{ownerRef(me)},
	}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pod).Build()

	if err := Undeploy(context.Background(), c, me, ns, name); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(pod), &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("owned import pod must be deleted, got err=%v", err)
	}
}

// TestUndeployCleansOwnPVCsDespiteForeignStatefulSet verifies that a same-name StatefulSet owned by
// another CR does not short-circuit teardown: Undeploy skips the foreign StatefulSet but still
// deletes THIS owner's lingering raft-state PVCs (matched by owner-UID label), so the IsTornDown gate
// that waits on them cannot deadlock. The foreign StatefulSet and a foreign CR's PVC are untouched.
func TestUndeployCleansOwnPVCsDespiteForeignStatefulSet(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	const (
		ns   = "default"
		name = "mychain-signer"
	)
	me := fakeOwner("me", types.UID("me-uid"))
	other := fakeOwner("other", types.UID("other-uid"))

	foreignSTS := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: ns, OwnerReferences: []metav1.OwnerReference{ownerRef(other)},
	}}
	myPVC := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: dataVolumeName + "-" + name + "-0", Namespace: ns, Labels: pvcOwnerLabels(name, "me-uid"),
	}}
	foreignPVC := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: dataVolumeName + "-" + name + "-1", Namespace: ns, Labels: pvcOwnerLabels(name, "other-uid"),
	}}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(foreignSTS, myPVC, foreignPVC).Build()

	if err := Undeploy(context.Background(), c, me, ns, name); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Our PVC must be gone; teardown is now complete from our perspective.
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(myPVC), &corev1.PersistentVolumeClaim{}); !apierrors.IsNotFound(err) {
		t.Fatalf("our PVC must be deleted, got err=%v", err)
	}
	torn, err := IsTornDown(context.Background(), c, me, ns, name)
	if err != nil {
		t.Fatal(err)
	}
	if !torn {
		t.Fatal("IsTornDown must be true once our PVC is gone, even with a foreign StatefulSet present")
	}

	// The foreign StatefulSet and the foreign CR's PVC must be untouched.
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(foreignSTS), &appsv1.StatefulSet{}); err != nil {
		t.Fatalf("foreign StatefulSet must remain, got err=%v", err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(foreignPVC), &corev1.PersistentVolumeClaim{}); err != nil {
		t.Fatalf("foreign PVC must remain, got err=%v", err)
	}
}

// TestAmbiguousLegacyPVCBlocksTornDown verifies unlabeled legacy raft-state claims are never deleted
// and never treated as torn down: they cannot be attributed to any owner without a race, so they
// block completion until the operator resolves them (delete or label), preventing a recreated signer
// from silently binding stale raft state.
func TestAmbiguousLegacyPVCBlocksTornDown(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	const (
		ns   = "default"
		name = "mychain-signer"
	)
	me := fakeOwner("me", types.UID("me-uid"))
	legacyPVC := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: dataVolumeName + "-" + name + "-0", Namespace: ns, Labels: InstanceLabels(name),
	}}

	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(legacyPVC).Build()

	// Teardown never deletes the ambiguous claim...
	if err := Undeploy(context.Background(), c, me, ns, name); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: legacyPVC.GetName()}, &corev1.PersistentVolumeClaim{}); err != nil {
		t.Fatalf("ambiguous legacy claim must never be deleted: %v", err)
	}

	// ...and it blocks torn-down until resolved.
	torn, err := IsTornDown(context.Background(), c, me, ns, name)
	if err != nil {
		t.Fatal(err)
	}
	if torn {
		t.Fatal("an ambiguous legacy claim must block teardown completion")
	}

	// Labeling it with our UID (the operator's resolution) unblocks and allows deletion.
	labeled := legacyPVC.DeepCopy()
	labeled.Labels = pvcOwnerLabels(name, "me-uid")
	if err := c.Update(context.Background(), labeled); err != nil {
		t.Fatal(err)
	}
	if err := Undeploy(context.Background(), c, me, ns, name); err != nil {
		t.Fatal(err)
	}
	torn, err = IsTornDown(context.Background(), c, me, ns, name)
	if err != nil {
		t.Fatal(err)
	}
	if !torn {
		t.Fatal("teardown must complete once the claim is labeled and deleted")
	}
}

func TestUndeployFindsOwnedPVCWithSelectorLabelsStripped(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	const (
		ns   = "default"
		name = "mychain-signer"
	)
	me := fakeOwner("me", types.UID("me-uid"))
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name:      dataVolumeName + "-" + name + "-0",
		Namespace: ns,
		Labels:    map[string]string{labelOwnerUID: "me-uid"},
	}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pvc).Build()

	torn, err := IsTornDown(context.Background(), c, me, ns, name)
	if err != nil {
		t.Fatal(err)
	}
	if torn {
		t.Fatal("a deterministic owned claim must block teardown even when selector labels were stripped")
	}
	if err := Undeploy(context.Background(), c, me, ns, name); err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(pvc), &corev1.PersistentVolumeClaim{}); !apierrors.IsNotFound(err) {
		t.Fatalf("owned name-matching claim must be deleted despite missing selector labels, got %v", err)
	}
}

// TestApplyOwnedRefusesForeignDataPVCsOnFreshStatefulSet verifies a FRESH signer StatefulSet (no
// same-name StatefulSet exists) is never created while exact-match raft-state PVCs of a DIFFERENT
// owner remain — e.g. a CR deleted and recreated under the same name (new UID) whose claims were
// left behind. Binding them would silently inherit stale raft membership/double-sign state. Claims
// owned by THIS owner must not block (rebinding own state is the normal restart path).
func TestApplyOwnedRefusesForeignDataPVCsOnFreshStatefulSet(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	const (
		ns   = "default"
		name = "mychain-signer"
	)
	// A scheme-registered owner: ApplyOwned sets a controller reference on the object, which needs
	// the owner's GVK resolvable from the scheme (the PartialObjectMetadata fakeOwner is not).
	me := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "me", Namespace: ns, UID: types.UID("me-uid")}}

	newSTS := func() *appsv1.StatefulSet {
		return &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}}
	}

	// Foreign exact-match claim present: creation must be refused.
	foreignPVC := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: dataVolumeName + "-" + name + "-0", Namespace: ns, Labels: pvcOwnerLabels(name, "other-uid"),
	}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(foreignPVC).Build()
	err := ApplyOwned(context.Background(), c, scheme, me, newSTS())
	if err == nil {
		t.Fatal("creating a fresh signer StatefulSet over a foreign raft-state PVC must be refused")
	}
	if err := c.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: name}, &appsv1.StatefulSet{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the StatefulSet must not have been created, got err=%v", err)
	}

	// Unlabeled legacy claim: refused too (unattributable).
	legacyPVC := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: dataVolumeName + "-" + name + "-0", Namespace: ns, Labels: InstanceLabels(name),
	}}
	c = fake.NewClientBuilder().WithScheme(scheme).WithObjects(legacyPVC).Build()
	if err := ApplyOwned(context.Background(), c, scheme, me, newSTS()); err == nil {
		t.Fatal("creating a fresh signer StatefulSet over an unlabeled legacy PVC must be refused")
	}

	// Claim with ALL labels stripped: still refused — the StatefulSet controller binds claims by
	// NAME, so a label-scoped scan would miss it while Kubernetes re-binds it anyway.
	strippedPVC := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: dataVolumeName + "-" + name + "-0", Namespace: ns,
	}}
	c = fake.NewClientBuilder().WithScheme(scheme).WithObjects(strippedPVC).Build()
	if err := ApplyOwned(context.Background(), c, scheme, me, newSTS()); err == nil {
		t.Fatal("creating a fresh signer StatefulSet over a label-stripped name-matching PVC must be refused")
	}

	// Own claim: creation proceeds (normal restart path).
	ownPVC := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: dataVolumeName + "-" + name + "-0", Namespace: ns, Labels: pvcOwnerLabels(name, "me-uid"),
	}}
	c = fake.NewClientBuilder().WithScheme(scheme).WithObjects(ownPVC).Build()
	if err := ApplyOwned(context.Background(), c, scheme, me, newSTS()); err != nil {
		t.Fatalf("own claims must not block a fresh StatefulSet: %v", err)
	}
}

func TestApplyOwnedRechecksDataPVCsBeforeUpdatingStatefulSet(t *testing.T) {
	scheme := lockScheme(t)
	const ns, name = "default", "mychain-signer"
	owner := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "owner", Namespace: ns, UID: types.UID("me-uid")}}
	zero := int32(0)
	existing := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns, UID: types.UID("signer-sts-uid"),
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "v1", Kind: "ConfigMap", Name: owner.Name, UID: owner.UID, Controller: ptrBool(true),
			}},
		},
		Spec: appsv1.StatefulSetSpec{Replicas: &zero},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(existing).Build()
	requireNoError := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	requireNoError(PreflightDeployable(context.Background(), c, owner, ns, name, 1, false, false))
	requireNoError(c.Create(context.Background(), &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: dataVolumeName + "-" + name + "-0", Namespace: ns, Labels: pvcOwnerLabels(name, "other-uid"),
	}}))

	one := int32(1)
	desired := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       appsv1.StatefulSetSpec{Replicas: &one},
	}
	err := ApplyOwned(context.Background(), c, scheme, owner, desired)
	if err == nil || !strings.Contains(err.Error(), "raft-state PVC") {
		t.Fatalf("a foreign PVC appearing after preflight must block StatefulSet scale-up, got %v", err)
	}
}

// TestPreflightDeployableRefusesForeignObjects verifies PreflightDeployable fails when ANY object the
// signer deployment creates by name (ConfigMap, raft/discovery Services, one-shot import pod,
// StatefulSet) already exists owned by a different controller — so a collision is caught before the
// ChainNodeSet retargets its validators, not after ApplyOwned refuses mid-deploy. Objects this owner
// controls, or absent objects, do not block.
func TestPreflightDeployableRefusesForeignObjects(t *testing.T) {
	const ns, name = "default", "cs-signer"
	me := fakeOwner("cs", types.UID("me-uid"))
	other := fakeOwner("other", types.UID("other-uid"))
	foreign := []metav1.OwnerReference{ownerRef(other)}

	cases := []struct {
		obj  client.Object
		want string
	}{
		{&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, OwnerReferences: foreign}}, "ConfigMap"},
		{&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, OwnerReferences: foreign}}, "raft Service"},
		{&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: name + discoveryServiceSuffix, Namespace: ns, OwnerReferences: foreign}}, "discovery Service"},
		{&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name + "-" + importJobSuffix, Namespace: ns, OwnerReferences: foreign}}, "import pod"},
		{&appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, OwnerReferences: foreign}}, "StatefulSet"},
	}
	for _, tc := range cases {
		c := fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(tc.obj).Build()
		// usesImportPod=true so the import-pod name is included in the collision checks.
		err := PreflightDeployable(context.Background(), c, me, ns, name, 1, true, false)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("foreign %s must block preflight; got err=%v", tc.want, err)
		}
	}

	// A foreign <name>-import pod must NOT block a signer that does not run the import pod (software / GCP
	// / pre-provisioned Vault): usesImportPod=false skips that name so an unrelated pod cannot wedge it.
	foreignImportPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name + "-" + importJobSuffix, Namespace: ns, OwnerReferences: foreign}}
	c := fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(foreignImportPod).Build()
	if err := PreflightDeployable(context.Background(), c, me, ns, name, 1, false, false); err != nil {
		t.Fatalf("a non-uploadGenerated signer must ignore a foreign import pod, got %v", err)
	}
	foreignPubkeyPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name + "-" + pubkeyJobSuffix, Namespace: ns, OwnerReferences: foreign}}
	c = fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(foreignPubkeyPod).Build()
	if err := PreflightDeployable(context.Background(), c, me, ns, name, 1, false, true); err == nil || !strings.Contains(err.Error(), "pubkey pod") {
		t.Fatalf("an external backend must reserve its pubkey preflight pod, got %v", err)
	}
	if err := PreflightDeployable(context.Background(), c, me, ns, name, 1, false, false); err != nil {
		t.Fatalf("a Secret-resolved public key must ignore an unused foreign pubkey pod, got %v", err)
	}

	foreignReplicaPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name + "-0", Namespace: ns, OwnerReferences: foreign}}
	c = fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(foreignReplicaPod).Build()
	if err := PreflightDeployable(context.Background(), c, me, ns, name, 1, false, false); err == nil || !strings.Contains(err.Error(), "replica pod") {
		t.Fatalf("a foreign signer replica pod must block preflight, got %v", err)
	}

	ownedSTS := &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
		Name: name, Namespace: ns, UID: "signer-sts-uid", OwnerReferences: []metav1.OwnerReference{ownerRef(me)},
	}}
	c = fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(ownedSTS, foreignReplicaPod).Build()
	if err := PreflightDeployable(context.Background(), c, me, ns, name, 1, false, false); err == nil || !strings.Contains(err.Error(), "replica pod") {
		t.Fatalf("a foreign replica pod must block re-scaling an owned StatefulSet, got %v", err)
	}

	ownedReplicaPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: name + "-0", Namespace: ns, OwnerReferences: []metav1.OwnerReference{ownerRef(ownedSTS)},
	}}
	c = fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(ownedSTS, ownedReplicaPod).Build()
	if err := PreflightDeployable(context.Background(), c, me, ns, name, 1, false, false); err != nil {
		t.Fatalf("a replica pod controlled by the owned StatefulSet must remain deployable, got %v", err)
	}

	foreignDataPVC := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: dataVolumeName + "-" + name + "-0", Namespace: ns, Labels: pvcOwnerLabels(name, "other-uid"),
	}}
	c = fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(ownedSTS, foreignDataPVC).Build()
	if err := PreflightDeployable(context.Background(), c, me, ns, name, 1, false, false); err == nil || !strings.Contains(err.Error(), "raft-state PVC") {
		t.Fatalf("a foreign retained data PVC must block re-scaling an owned StatefulSet, got %v", err)
	}

	ownDataPVC := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: dataVolumeName + "-" + name + "-0", Namespace: ns, Labels: pvcOwnerLabels(name, "me-uid"),
	}}
	c = fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(ownedSTS, ownDataPVC).Build()
	if err := PreflightDeployable(context.Background(), c, me, ns, name, 1, false, false); err != nil {
		t.Fatalf("an owned retained data PVC must remain deployable, got %v", err)
	}

	// Nothing present (true first rollout): allowed.
	empty := fake.NewClientBuilder().WithScheme(lockScheme(t)).Build()
	if err := PreflightDeployable(context.Background(), empty, me, ns, name, 1, true, false); err != nil {
		t.Fatalf("empty namespace must be deployable, got %v", err)
	}

	// Our own objects: allowed.
	mine := fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, OwnerReferences: []metav1.OwnerReference{ownerRef(me)}}},
		&corev1.Service{
			ObjectMeta: metav1.ObjectMeta{Name: name + discoveryServiceSuffix, Namespace: ns, OwnerReferences: []metav1.OwnerReference{ownerRef(me)}},
			Spec:       corev1.ServiceSpec{ClusterIP: corev1.ClusterIPNone},
		},
	).Build()
	if err := PreflightDeployable(context.Background(), mine, me, ns, name, 1, true, false); err != nil {
		t.Fatalf("own objects must be deployable, got %v", err)
	}
}

func TestPreflightDeployableRefusesOwnedNonHeadlessRaftService(t *testing.T) {
	const ns, name = "default", "cs-signer"
	me := fakeOwner("cs", types.UID("me-uid"))
	normal := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, OwnerReferences: []metav1.OwnerReference{ownerRef(me)}},
		Spec:       corev1.ServiceSpec{ClusterIP: "10.0.0.10"},
	}
	client := fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(normal).Build()

	err := PreflightDeployable(context.Background(), client, me, ns, name, 1, false, false)
	if err == nil || !strings.Contains(err.Error(), "not headless") {
		t.Fatalf("owned non-headless raft Service must block preflight, got %v", err)
	}
	externalName := normal.DeepCopy()
	externalName.Spec = corev1.ServiceSpec{Type: corev1.ServiceTypeExternalName, ExternalName: "example.com"}
	client = fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(externalName).Build()
	if err := PreflightDeployable(context.Background(), client, me, ns, name, 1, false, false); err == nil || !strings.Contains(err.Error(), "not headless") {
		t.Fatalf("owned ExternalName raft Service must block preflight, got %v", err)
	}

	headless := normal.DeepCopy()
	headless.Spec.ClusterIP = corev1.ClusterIPNone
	client = fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(headless).Build()
	if err := PreflightDeployable(context.Background(), client, me, ns, name, 1, false, false); err != nil {
		t.Fatalf("owned headless raft Service must remain deployable, got %v", err)
	}
}

func TestPreflightDeployableRefusesOwnedNonHeadlessDiscoveryService(t *testing.T) {
	const ns, name = "default", "cs-signer"
	me := fakeOwner("cs", types.UID("me-uid"))
	normal := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name + discoveryServiceSuffix, Namespace: ns, OwnerReferences: []metav1.OwnerReference{ownerRef(me)}},
		Spec:       corev1.ServiceSpec{ClusterIP: "10.0.0.11"},
	}
	client := fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(normal).Build()

	err := PreflightDeployable(context.Background(), client, me, ns, name, 1, false, false)
	if err == nil || !strings.Contains(err.Error(), "discovery Service") || !strings.Contains(err.Error(), "not headless") {
		t.Fatalf("owned non-headless discovery Service must block preflight, got %v", err)
	}

	headless := normal.DeepCopy()
	headless.Spec.ClusterIP = corev1.ClusterIPNone
	client = fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(headless).Build()
	if err := PreflightDeployable(context.Background(), client, me, ns, name, 1, false, false); err != nil {
		t.Fatalf("owned headless discovery Service must remain deployable, got %v", err)
	}
}
