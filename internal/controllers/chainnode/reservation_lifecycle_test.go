package chainnode

import (
	"context"
	"strings"
	"testing"
	"time"

	appsk8sv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/cosmosigner"
)

func TestPrepareConsensusKeyReservationOwnerFinalizesChainNodeSetRoot(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	parent := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{
		Name: "nodes", Namespace: "default", UID: "nodes-uid",
	}}
	child := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{
			Name: "nodes-validator", Namespace: parent.Namespace, UID: "child-uid",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNodeSet", Name: parent.Name,
				UID: parent.UID, Controller: ptr.To(true),
			}},
		},
		Spec: appsv1.ChainNodeSpec{Validator: &appsv1.ValidatorConfig{}},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(parent, child).Build()
	r := &Reconciler{Client: c, APIReader: c}

	changed, err := r.prepareConsensusKeyReservationOwner(context.Background(), child)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("the parent finalizer must be persisted before a child-owned local reservation")
	}

	freshParent := &appsv1.ChainNodeSet{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(parent), freshParent); err != nil {
		t.Fatal(err)
	}
	if !controllerutil.ContainsFinalizer(freshParent, cosmosigner.ReservationOwnerFinalizer) {
		t.Fatalf("parent finalizers = %v", freshParent.Finalizers)
	}
	freshChild := &appsv1.ChainNode{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(child), freshChild); err != nil {
		t.Fatal(err)
	}
	if controllerutil.ContainsFinalizer(freshChild, cosmosigner.ReservationOwnerFinalizer) {
		t.Fatal("a ChainNodeSet child must not own the root reservation lifecycle")
	}
}

func TestReconcileTerminatingNamespaceRunsReservationFinalizer(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := appsk8sv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	now := metav1.NewTime(time.Now())
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "terminating", DeletionTimestamp: &now, Finalizers: []string{"kubernetes"},
	}}
	owner := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{
		Name: "validator", Namespace: ns.Name, UID: "owner-uid",
	}}
	reservation := &appsv1.ConsensusKeyReservation{
		ObjectMeta: metav1.ObjectMeta{Name: cosmosigner.ConsensusKeyReservationName("chain-1", reservationLifecyclePublicKey), UID: "ckr-uid"},
		Spec: appsv1.ConsensusKeyReservationSpec{
			ChainID: "chain-1", PublicKey: reservationLifecyclePublicKey,
			OwnerUID: owner.UID, OwnerKind: "ChainNode", Namespace: owner.Namespace,
			OwnerName: owner.Name, Claim: owner.Name,
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(ns, owner, reservation).Build()
	r := &Reconciler{Client: c, APIReader: c}

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: owner.Namespace, Name: owner.Name,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(reservation), &appsv1.ConsensusKeyReservation{}); !apierrors.IsNotFound(err) {
		t.Fatalf("terminating namespace stranded reservation: %v", err)
	}
}

func TestFinalizeConsensusKeyReservationOwnerWaitsForPodAndPreservesState(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := appsk8sv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	owner := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{
		Name: "validator", Namespace: "default", UID: "owner-uid",
		Finalizers: []string{cosmosigner.ReservationOwnerFinalizer},
	}}
	reservation := &appsv1.ConsensusKeyReservation{
		ObjectMeta: metav1.ObjectMeta{
			Name: cosmosigner.ConsensusKeyReservationName("chain-1", reservationLifecyclePublicKey), UID: "ckr-uid",
		},
		Spec: appsv1.ConsensusKeyReservationSpec{
			ChainID: "chain-1", PublicKey: reservationLifecyclePublicKey,
			OwnerUID: owner.UID, OwnerKind: "ChainNode", Namespace: owner.Namespace,
			OwnerName: owner.Name, Claim: owner.Name,
		},
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: owner.Name, Namespace: owner.Namespace, UID: "pod-uid",
		Finalizers: []string{"test.voluzi.com/hold"},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNode", Name: owner.Name,
			UID: owner.UID, Controller: ptr.To(true),
		}},
	}}
	key := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "validator-key", Namespace: owner.Namespace}}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: owner.Name, Namespace: owner.Namespace}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(owner, reservation, pod, key, pvc).Build()
	r := &Reconciler{Client: c, APIReader: c, recorder: record.NewFakeRecorder(10)}

	done, err := r.finalizeConsensusKeyReservationOwner(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	if done {
		t.Fatal("a terminating validator pod must keep reservation finalization pending")
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(reservation), &appsv1.ConsensusKeyReservation{}); err != nil {
		t.Fatalf("reservation released before pod absence: %v", err)
	}
	freshPod := &corev1.Pod{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(pod), freshPod); err != nil {
		t.Fatal(err)
	}
	if freshPod.DeletionTimestamp == nil {
		t.Fatal("validator pod teardown was not requested")
	}
	freshPod.Finalizers = nil
	if err := c.Update(context.Background(), freshPod); err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}

	done, err = r.finalizeConsensusKeyReservationOwner(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	if !done {
		t.Fatal("reservation lifecycle must complete after signing paths are absent")
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(reservation), &appsv1.ConsensusKeyReservation{}); !apierrors.IsNotFound(err) {
		t.Fatalf("reservation still exists after verified teardown: %v", err)
	}
	freshOwner := &appsv1.ChainNode{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(owner), freshOwner); err != nil {
		t.Fatal(err)
	}
	if controllerutil.ContainsFinalizer(freshOwner, cosmosigner.ReservationOwnerFinalizer) {
		t.Fatal("reservation lifecycle finalizer was not removed")
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(key), &corev1.Secret{}); err != nil {
		t.Fatalf("signing key retention changed: %v", err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(pvc), &corev1.PersistentVolumeClaim{}); err != nil {
		t.Fatalf("data PVC retention changed: %v", err)
	}
}

func TestFinalizeOwnedSigningPodsBlocksForeignHelperPod(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	owner := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: "validator", Namespace: "default", UID: "owner-uid"}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: owner.Name + "-create-validator", Namespace: owner.Namespace, UID: "foreign-pod-uid"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(owner, pod).Build()
	r := &Reconciler{Client: c, APIReader: c}

	done, err := r.finalizeOwnedSigningPods(context.Background(), owner)
	if err == nil || done || !strings.Contains(err.Error(), pod.Name) {
		t.Fatalf("foreign helper pod must block reservation release, done=%v err=%v", done, err)
	}
}

func TestFinalizeConsensusKeyReservationOwnerWaitsForManagedJobPod(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := appsk8sv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	owner := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{
		Name: "validator", Namespace: "default", UID: "owner-uid",
		Finalizers: []string{cosmosigner.ReservationOwnerFinalizer},
	}}
	reservation := &appsv1.ConsensusKeyReservation{
		ObjectMeta: metav1.ObjectMeta{
			Name: cosmosigner.ConsensusKeyReservationName("chain-1", reservationLifecyclePublicKey), UID: "ckr-uid",
		},
		Spec: appsv1.ConsensusKeyReservationSpec{
			ChainID: "chain-1", PublicKey: reservationLifecyclePublicKey,
			OwnerUID: owner.UID, OwnerKind: "ChainNode", Namespace: owner.Namespace,
			OwnerName: owner.Name, Claim: owner.Name,
		},
	}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: owner.Name + "-tmkms-vault-upload", Namespace: owner.Namespace, UID: "job-uid",
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNode", Name: owner.Name,
			UID: owner.UID, Controller: ptr.To(true),
		}},
	}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: job.Name + "-pod", Namespace: owner.Namespace, UID: "pod-uid",
		Finalizers: []string{"test.voluzi.com/hold"},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: batchv1.SchemeGroupVersion.String(), Kind: "Job", Name: job.Name,
			UID: job.UID, Controller: ptr.To(true),
		}},
	}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(owner, reservation, job, pod).Build()
	r := &Reconciler{Client: c, APIReader: c, recorder: record.NewFakeRecorder(10)}

	done, err := r.finalizeConsensusKeyReservationOwner(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	if done {
		t.Fatal("a terminating managed Job pod must keep reservation finalization pending")
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(reservation), &appsv1.ConsensusKeyReservation{}); err != nil {
		t.Fatalf("reservation released before Job pod absence: %v", err)
	}
	freshPod := &corev1.Pod{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(pod), freshPod); err != nil {
		t.Fatal(err)
	}
	if freshPod.DeletionTimestamp != nil {
		t.Fatal("managed Job pod must be left for foreground Job deletion")
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(job), &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Fatalf("managed Job deletion was not requested before waiting for its pod: %v", err)
	}
	freshPod.Finalizers = nil
	if err := c.Update(context.Background(), freshPod); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(context.Background(), freshPod); err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}

	done, err = r.finalizeConsensusKeyReservationOwner(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	if !done {
		t.Fatal("reservation lifecycle must complete after managed Job and pod absence")
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(job), &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Fatalf("managed Job still exists after finalization: %v", err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(reservation), &appsv1.ConsensusKeyReservation{}); !apierrors.IsNotFound(err) {
		t.Fatalf("reservation still exists after managed Job teardown: %v", err)
	}
}

func TestEnsureConsensusKeyReservationEmitsBlockedStaleRecoveryEvent(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := appsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := appsk8sv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	owner := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{
		Name: "validator", Namespace: "default", UID: "new-owner-uid",
		Finalizers: []string{cosmosigner.ReservationOwnerFinalizer},
	}}
	reservation := &appsv1.ConsensusKeyReservation{
		ObjectMeta: metav1.ObjectMeta{
			Name: cosmosigner.ConsensusKeyReservationName("chain-1", reservationLifecyclePublicKey), UID: "ckr-uid",
		},
		Spec: appsv1.ConsensusKeyReservationSpec{
			ChainID: "chain-1", PublicKey: reservationLifecyclePublicKey,
			OwnerUID: "old-owner-uid", OwnerKind: "ChainNode", Namespace: owner.Namespace,
			OwnerName: owner.Name, Claim: owner.Name,
		},
	}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: owner.Name + "-tmkms-vault-upload", Namespace: owner.Namespace, UID: "job-uid",
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNode", Name: owner.Name,
			UID: "old-owner-uid", Controller: ptr.To(true),
		}},
	}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(owner, reservation, job).Build()
	recorder := record.NewFakeRecorder(10)
	r := &Reconciler{Client: c, APIReader: c, recorder: recorder}

	err := r.ensureConsensusKeyReservation(context.Background(), owner, "chain-1", reservationLifecyclePublicKey, cosmosigner.ReservationHolder{
		UID: owner.UID, Kind: "ChainNode", Namespace: owner.Namespace, Name: owner.Name, Claim: owner.Name,
	})
	if err == nil {
		t.Fatal("stale recovery must remain blocked while its managed Job exists")
	}
	select {
	case event := <-recorder.Events:
		if !strings.Contains(event, appsv1.ReasonConsensusKeyReservationBlocked) || !strings.Contains(event, job.Name) {
			t.Fatalf("blocked recovery event is not actionable: %q", event)
		}
	default:
		t.Fatal("blocked stale recovery did not emit an event")
	}
}

const reservationLifecyclePublicKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
