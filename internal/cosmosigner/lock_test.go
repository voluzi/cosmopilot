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

// TestReadSignerLockIgnoresQuiescedReplicas verifies a signer scaled to zero (transient quiesce
// during a Vault re-import) is not recorded as the raft membership: the CRD forbids replicas==0, so
// recording it would wedge every later comparison. foundReplicas stays false -> caller uses the spec.
func TestReadSignerLockIgnoresQuiescedReplicas(t *testing.T) {
	const ns, name = "default", "cs-signer"
	owner := fakeOwner("cs", types.UID("cs-uid"))
	c := fake.NewClientBuilder().WithScheme(lockScheme(t)).
		WithObjects(signerSTS(name, ns, owner, 0, "10Gi", ptr.To("fast"))).Build()

	_, _, class, foundR, foundS, err := ReadSignerLock(context.Background(), c, owner, ns, name)
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
