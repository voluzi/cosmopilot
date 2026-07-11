package cosmosigner

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
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

// fakeOwner is a minimal metav1.Object carrying a UID and a controller owner reference, so
// metav1.IsControlledBy works against StatefulSets it owns.
type fakeOwner struct {
	metav1.ObjectMeta
}

func ownerRef(o *fakeOwner) metav1.OwnerReference {
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
	me := &fakeOwner{ObjectMeta: metav1.ObjectMeta{Name: "me", Namespace: ns, UID: types.UID("me-uid")}}
	other := &fakeOwner{ObjectMeta: metav1.ObjectMeta{Name: "other", Namespace: ns, UID: types.UID("other-uid")}}

	ownedSTS := func(owner *fakeOwner) *appsv1.StatefulSet {
		return &appsv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: ns, OwnerReferences: []metav1.OwnerReference{ownerRef(owner)},
		}}
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
