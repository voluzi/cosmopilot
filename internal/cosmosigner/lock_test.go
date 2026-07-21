package cosmosigner

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func lockScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func signerSTS(name, ns string, owner metav1.Object, replicas int32, size string, class *string) *appsv1.StatefulSet {
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, OwnerReferences: []metav1.OwnerReference{ownerRef(owner)}},
		Spec: appsv1.StatefulSetSpec{
			Replicas: ptr.To(replicas),
			VolumeClaimTemplates: []corev1.PersistentVolumeClaim{{
				ObjectMeta: metav1.ObjectMeta{Name: dataVolumeName},
				Spec: corev1.PersistentVolumeClaimSpec{
					StorageClassName: class,
					Resources: corev1.VolumeResourceRequirements{
						Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)},
					},
				},
			}},
		},
	}
	return sts
}

// TestReadSignerLockPreservesNilStorageClass verifies a volumeClaimTemplate that omits
// storageClassName round-trips as nil (cluster default), not "". Collapsing to "" would make the
// no-webhook guard reject an unchanged spec whose storageClassName is also omitted.
func TestReadSignerLockPreservesNilStorageClass(t *testing.T) {
	const ns, name = "default", "cs-signer"
	owner := fakeOwner("cs", types.UID("cs-uid"))
	c := fake.NewClientBuilder().WithScheme(lockScheme(t)).
		WithObjects(signerSTS(name, ns, owner, 3, "10Gi", nil)).Build()

	replicas, size, class, foundR, foundS, err := ReadSignerLock(context.Background(), c, owner, ns, name)
	if err != nil {
		t.Fatal(err)
	}
	if !foundR || replicas != 3 {
		t.Fatalf("replicas: got %d found=%v, want 3 true", replicas, foundR)
	}
	if !foundS || size != "10Gi" {
		t.Fatalf("size: got %q found=%v, want 10Gi true", size, foundS)
	}
	if class != nil {
		t.Fatalf("storage class must stay nil (cluster default), got %q", *class)
	}
}

// TestReadSignerLockFailsClosedOnQuiescedReplicas verifies that when the only live StatefulSet is
// scaled to zero (a transient Vault re-import quiesce) and no status lock is recorded, ReadSignerLock
// fails closed rather than reporting foundReplicas=false — otherwise the caller would fall back to the
// (mutable) spec and could record a changed replica count as the immutable raft membership.
func TestReadSignerLockFailsClosedOnQuiescedReplicas(t *testing.T) {
	const ns, name = "default", "cs-signer"
	owner := fakeOwner("cs", types.UID("cs-uid"))
	c := fake.NewClientBuilder().WithScheme(lockScheme(t)).
		WithObjects(signerSTS(name, ns, owner, 0, "10Gi", ptr.To("fast"))).Build()

	_, _, _, foundR, foundS, err := ReadSignerLock(context.Background(), c, owner, ns, name)
	if err == nil {
		t.Fatal("a quiesced (replicas=0) StatefulSet with no recorded lock must fail closed, got nil error")
	}
	if foundR || foundS {
		t.Fatal("no lock may be reported for a quiesced StatefulSet")
	}
}

// TestReadSignerLockNoSignerState verifies a missing StatefulSet reports nothing (true first rollout).
func TestReadSignerLockNoSignerState(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(lockScheme(t)).Build()
	_, _, _, foundR, foundS, err := ReadSignerLock(context.Background(), c, fakeOwner("cs", types.UID("u")), "default", "cs-signer")
	if err != nil {
		t.Fatal(err)
	}
	if foundR || foundS {
		t.Fatal("no live StatefulSet must report neither replica nor storage lock")
	}
}

// TestReadSignerLockIgnoresForeignStatefulSet verifies a same-name StatefulSet owned by another CR is
// never adopted as this owner's lock.
func TestReadSignerLockIgnoresForeignStatefulSet(t *testing.T) {
	const ns, name = "default", "cs-signer"
	me := fakeOwner("cs", types.UID("me-uid"))
	other := fakeOwner("other", types.UID("other-uid"))
	c := fake.NewClientBuilder().WithScheme(lockScheme(t)).
		WithObjects(signerSTS(name, ns, other, 5, "20Gi", nil)).Build()

	_, _, _, foundR, foundS, err := ReadSignerLock(context.Background(), c, me, ns, name)
	if err != nil {
		t.Fatal(err)
	}
	if foundR || foundS {
		t.Fatal("a foreign-owned StatefulSet must not seed this owner's lock")
	}
}

// TestReadSignerLockRejectsOrphanedOwnedPVCs verifies that when the StatefulSet is gone but this
// owner's per-pod raft-state PVCs survive, the lock FAILS CLOSED rather than adopting a membership the
// claims cannot prove: a surviving subset of ordinals is indistinguishable from a smaller original
// cluster (a truncated {0} looks the same whether the raft cluster was 1 replica or 3), so recreating a
// StatefulSet from it could re-bind stale raft state under a membership it was never formed with.
func TestReadSignerLockRejectsOrphanedOwnedPVCs(t *testing.T) {
	const ns, name = "default", "cs-signer"
	owner := fakeOwner("cs", types.UID("cs-uid"))

	// A "complete-looking" set {0,1,2} and a top-truncated {0} are BOTH rejected — neither proves the
	// original raft membership without the StatefulSet or a recorded lock.
	for _, ordinals := range [][]int{{0, 1, 2}, {0}} {
		objs := make([]client.Object, 0, len(ordinals))
		for _, o := range ordinals {
			objs = append(objs, ownedDataPVC(name, ns, o, "cs-uid", "5Gi", ptr.To("fast")))
		}
		c := fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(objs...).Build()
		_, _, _, foundR, foundS, err := ReadSignerLock(context.Background(), c, owner, ns, name)
		if err == nil {
			t.Fatalf("orphaned owned PVC set %v must fail closed, got nil error", ordinals)
		}
		if foundR || foundS {
			t.Fatalf("no lock may be reported for orphaned set %v", ordinals)
		}
	}
}

// TestReadSignerLockIgnoresForeignOrphanedPVCs verifies orphaned PVCs owned by ANOTHER CR are not
// adopted as this owner's lock.
func TestReadSignerLockIgnoresForeignOrphanedPVCs(t *testing.T) {
	const ns, name = "default", "cs-signer"
	me := fakeOwner("cs", types.UID("me-uid"))
	foreign := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dataVolumeName + "-" + name + "-0",
			Namespace: ns,
			Labels:    pvcOwnerLabels(name, "other-uid"),
		},
	}
	c := fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(foreign).Build()
	_, _, _, foundR, foundS, err := ReadSignerLock(context.Background(), c, me, ns, name)
	if err != nil {
		t.Fatal(err)
	}
	if foundR || foundS {
		t.Fatal("a foreign-owned orphaned PVC must not seed this owner's lock")
	}
}

func ownedDataPVC(name, ns string, ordinal int, uid types.UID, size string, class *string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dataVolumeName + "-" + name + "-" + string(rune('0'+ordinal)),
			Namespace: ns,
			Labels:    pvcOwnerLabels(name, uid),
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: class,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse(size)},
			},
		},
	}
}

// TestReadSignerLockRejectsAmbiguousOrphanedPVCs verifies that an exact-name state PVC WITHOUT the
// owner-UID label fails closed instead of falling through to a spec-derived lock: a fresh StatefulSet
// would re-bind it by name, so recording a (possibly drifted) spec lock while it exists is unsafe.
func TestReadSignerLockRejectsAmbiguousOrphanedPVCs(t *testing.T) {
	const ns, name = "default", "cs-signer"
	owner := fakeOwner("cs", types.UID("cs-uid"))
	ambiguous := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      dataVolumeName + "-" + name + "-0",
			Namespace: ns,
			// No owner-UID label.
		},
	}
	c := fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(ambiguous).Build()

	_, _, _, foundR, foundS, err := ReadSignerLock(context.Background(), c, owner, ns, name)
	if err == nil {
		t.Fatal("an unlabeled exact-name state PVC must fail closed, got nil error")
	}
	if foundR || foundS {
		t.Fatal("no lock may be reported while an ambiguous claim exists")
	}
}
