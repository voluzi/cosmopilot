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

	replicas, size, class, foundR, foundS, err := ReadSignerLock(context.Background(), c, owner, ns, name, nil)
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

// TestReadSignerLockIgnoresQuiescedReplicas verifies a signer scaled to zero (transient quiesce
// during a Vault re-import) is not recorded as the raft membership: the CRD forbids replicas==0, so
// recording it would wedge every later comparison. foundReplicas stays false -> caller uses the spec.
func TestReadSignerLockIgnoresQuiescedReplicas(t *testing.T) {
	const ns, name = "default", "cs-signer"
	owner := fakeOwner("cs", types.UID("cs-uid"))
	c := fake.NewClientBuilder().WithScheme(lockScheme(t)).
		WithObjects(signerSTS(name, ns, owner, 0, "10Gi", ptr.To("fast"))).Build()

	_, _, class, foundR, foundS, err := ReadSignerLock(context.Background(), c, owner, ns, name, ptr.To("fast"))
	if err != nil {
		t.Fatal(err)
	}
	if foundR {
		t.Fatal("a quiesced (replicas=0) StatefulSet must not report a replica lock")
	}
	// Storage is still adopted from the live template.
	if !foundS || class == nil || *class != "fast" {
		t.Fatalf("storage class: got %v found=%v, want fast", class, foundS)
	}
}

// TestReadSignerLockNoSignerState verifies a missing StatefulSet reports nothing (true first rollout).
func TestReadSignerLockNoSignerState(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(lockScheme(t)).Build()
	_, _, _, foundR, foundS, err := ReadSignerLock(context.Background(), c, fakeOwner("cs", types.UID("u")), "default", "cs-signer", nil)
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

	_, _, _, foundR, foundS, err := ReadSignerLock(context.Background(), c, me, ns, name, nil)
	if err != nil {
		t.Fatal(err)
	}
	if foundR || foundS {
		t.Fatal("a foreign-owned StatefulSet must not seed this owner's lock")
	}
}

// TestReadSignerLockRecoversFromOrphanedOwnedPVCs verifies that when the StatefulSet is gone but this
// owner's per-pod raft-state PVCs survive, the lock is recovered from those claims (replica count =
// number of claims, size/class from a claim) rather than falling back to the spec — a fresh
// StatefulSet would re-bind them, so the recorded lock must match their membership and size/class.
func TestReadSignerLockRecoversFromOrphanedOwnedPVCs(t *testing.T) {
	const ns, name = "default", "cs-signer"
	owner := fakeOwner("cs", types.UID("cs-uid"))
	pvc := func(ordinal int) *corev1.PersistentVolumeClaim {
		return &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      dataVolumeName + "-" + name + "-" + string(rune('0'+ordinal)),
				Namespace: ns,
				Labels:    pvcOwnerLabels(name, "cs-uid"),
			},
			Spec: corev1.PersistentVolumeClaimSpec{
				StorageClassName: ptr.To("fast"),
				Resources: corev1.VolumeResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("5Gi")},
				},
			},
		}
	}
	// No StatefulSet; three owned data PVCs survive.
	c := fake.NewClientBuilder().WithScheme(lockScheme(t)).WithObjects(pvc(0), pvc(1), pvc(2)).Build()

	replicas, size, class, foundR, foundS, err := ReadSignerLock(context.Background(), c, owner, ns, name, ptr.To("fast"))
	if err != nil {
		t.Fatal(err)
	}
	if !foundR || replicas != 3 {
		t.Fatalf("replicas from orphaned PVCs: got %d found=%v, want 3 true", replicas, foundR)
	}
	if !foundS || size != "5Gi" || class == nil || *class != "fast" {
		t.Fatalf("storage from orphaned PVCs: got %q class=%v found=%v", size, class, foundS)
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
	_, _, _, foundR, foundS, err := ReadSignerLock(context.Background(), c, me, ns, name, nil)
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

// TestReadSignerLockRejectsGappedOrphanedPVCs verifies that a non-contiguous set of orphaned claims
// (a claim deleted from the MIDDLE of the membership) is refused rather than mistaken for a smaller
// raft cluster: the claim count cannot prove the original membership, so recovery fails closed.
func TestReadSignerLockRejectsGappedOrphanedPVCs(t *testing.T) {
	const ns, name = "default", "cs-signer"
	owner := fakeOwner("cs", types.UID("cs-uid"))
	// Ordinals {0, 2} — ordinal 1 was deleted from the middle.
	c := fake.NewClientBuilder().WithScheme(lockScheme(t)).
		WithObjects(
			ownedDataPVC(name, ns, 0, "cs-uid", "5Gi", ptr.To("fast")),
			ownedDataPVC(name, ns, 2, "cs-uid", "5Gi", ptr.To("fast")),
		).Build()

	_, _, _, foundR, _, err := ReadSignerLock(context.Background(), c, owner, ns, name, ptr.To("fast"))
	if err == nil {
		t.Fatal("a gapped orphaned-PVC set must fail closed, got nil error")
	}
	if foundR {
		t.Fatal("no replica lock may be reported for a gapped set")
	}
}

// TestReadSignerLockRecoveryUsesDesiredStorageClass verifies the orphaned-PVC recovery reports the
// DESIRED template class, independent of the class materialised on the PVC. A recreated StatefulSet
// re-binds the claims by name, so the class is fixed by the existing PVs; recording the desired value
// keeps an unchanged (omitting) template from reading as a storage change, and needs no cluster-scoped
// StorageClass lookup.
func TestReadSignerLockRecoveryUsesDesiredStorageClass(t *testing.T) {
	const ns, name = "default", "cs-signer"
	owner := fakeOwner("cs", types.UID("cs-uid"))
	// PVCs carry the admission-materialised default class "standard".
	c := fake.NewClientBuilder().WithScheme(lockScheme(t)).
		WithObjects(
			ownedDataPVC(name, ns, 0, "cs-uid", "5Gi", ptr.To("standard")),
			ownedDataPVC(name, ns, 1, "cs-uid", "5Gi", ptr.To("standard")),
		).Build()

	// Desired template omits the class (nil): recovery reports nil, not "standard".
	_, size, class, foundR, foundS, err := ReadSignerLock(context.Background(), c, owner, ns, name, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !foundR || !foundS || size != "5Gi" {
		t.Fatalf("recovery: foundR=%v foundS=%v size=%q", foundR, foundS, size)
	}
	if class != nil {
		t.Fatalf("omitting template must recover nil class, got %q", *class)
	}

	// Desired template names an explicit class: recovery reports that class.
	_, _, class2, _, _, err := ReadSignerLock(context.Background(), c, owner, ns, name, ptr.To("gold"))
	if err != nil {
		t.Fatal(err)
	}
	if class2 == nil || *class2 != "gold" {
		t.Fatalf("explicit desired class must round-trip, got %v", class2)
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

	_, _, _, foundR, foundS, err := ReadSignerLock(context.Background(), c, owner, ns, name, nil)
	if err == nil {
		t.Fatal("an unlabeled exact-name state PVC must fail closed, got nil error")
	}
	if foundR || foundS {
		t.Fatal("no lock may be reported while an ambiguous claim exists")
	}
}
