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

	appsv1 "github.com/voluzi/cosmopilot/v3/api/v1"
	"github.com/voluzi/cosmopilot/v3/internal/cosmosigner"
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
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: owner.Namespace}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(namespace, owner, reservation, job).Build()
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

func TestReconcileConsensusKeyReservationClaimsReleasesRemovedStandaloneClaimAndPreservesState(t *testing.T) {
	scheme := reservationLifecycleScheme(t)
	owner := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: "sentry", Namespace: "default", UID: "owner-uid"}}
	previous := owner.DeepCopy()
	previous.Spec.Cosmosigner = &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{
		Software: &appsv1.CosmosignerSoftwareBackend{PrivateKeySecret: ptr.To("old-key")},
	}}
	reservation := standaloneReservationLifecycleObject("obsolete", "obsolete-uid", owner, standaloneCosmosignerReservationClaim(previous), "old-public-key")
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "old-key", Namespace: owner.Namespace}}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data-sentry-signer-0", Namespace: owner.Namespace}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(owner, reservation, secret, pvc).Build()
	recorder := record.NewFakeRecorder(10)
	r := &Reconciler{Client: c, APIReader: c, recorder: recorder}

	done, err := r.reconcileConsensusKeyReservationClaims(context.Background(), owner)
	if err != nil || !done {
		t.Fatalf("obsolete standalone claim retirement failed, done=%v err=%v", done, err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(reservation), &appsv1.ConsensusKeyReservation{}); !apierrors.IsNotFound(err) {
		t.Fatalf("obsolete standalone reservation still exists: %v", err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(secret), &corev1.Secret{}); err != nil {
		t.Fatalf("retained signing Secret was modified: %v", err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(pvc), &corev1.PersistentVolumeClaim{}); err != nil {
		t.Fatalf("retained signer PVC was modified: %v", err)
	}
	select {
	case event := <-recorder.Events:
		if !strings.Contains(event, appsv1.ReasonConsensusKeyReservationReleased) || !strings.Contains(event, reservation.Name) {
			t.Fatalf("reservation release event is not actionable: %q", event)
		}
	default:
		t.Fatal("obsolete standalone reservation release did not emit an event")
	}
}

func TestReconcileConsensusKeyReservationClaimsRetiresCompletedMigrationClaimOnly(t *testing.T) {
	scheme := reservationLifecycleScheme(t)
	owner := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "sentry", Namespace: "default", UID: "owner-uid"},
		Spec: appsv1.ChainNodeSpec{Cosmosigner: &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{
			Software: &appsv1.CosmosignerSoftwareBackend{PrivateKeySecret: ptr.To("current-key")},
		}}},
		Status: appsv1.ChainNodeStatus{
			ChainID: "chain-1", CosmosignerAppliedDigest: "current-digest", CosmosignerPublicKey: "current-public-key",
		},
	}
	previous := owner.DeepCopy()
	previous.Spec.Cosmosigner.Backend.Software.PrivateKeySecret = ptr.To("old-key")
	oldReservation := standaloneReservationLifecycleObject("old", "old-uid", owner, standaloneCosmosignerReservationClaim(previous), "old-public-key")
	currentReservation := standaloneReservationLifecycleObject("current", "current-uid", owner, standaloneCosmosignerReservationClaim(owner), owner.Status.CosmosignerPublicKey)
	sts := &appsk8sv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: cosmosignerName(owner), Namespace: owner.Namespace, UID: "current-sts-uid",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNode", Name: owner.Name,
				UID: owner.UID, Controller: ptr.To(true),
			}},
		},
		Spec: appsk8sv1.StatefulSetSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
			"app.kubernetes.io/name": "cosmosigner", "app.kubernetes.io/instance": cosmosignerName(owner), "chain-node": owner.Name,
		}}}},
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "old-key", Namespace: owner.Namespace}}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "data-sentry-signer-0", Namespace: owner.Namespace}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(owner, oldReservation, currentReservation, sts, secret, pvc).Build()
	r := &Reconciler{Client: c, APIReader: c, recorder: record.NewFakeRecorder(10)}

	done, err := r.reconcileConsensusKeyReservationClaims(context.Background(), owner)
	if err != nil || !done {
		t.Fatalf("completed migration claim retirement failed, done=%v err=%v", done, err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(oldReservation), &appsv1.ConsensusKeyReservation{}); !apierrors.IsNotFound(err) {
		t.Fatalf("old migration reservation still exists: %v", err)
	}
	for _, object := range []client.Object{currentReservation, sts, secret, pvc} {
		fresh := object.DeepCopyObject().(client.Object)
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(object), fresh); err != nil {
			t.Fatalf("current signing resource %T %s was modified: %v", object, object.GetName(), err)
		}
	}
}

func TestReconcileConsensusKeyReservationClaimsRetainsCurrentAndLocalClaims(t *testing.T) {
	scheme := reservationLifecycleScheme(t)
	owner := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "signer-" + strings.Repeat("a", 64), Namespace: "default", UID: "owner-uid"},
		Spec: appsv1.ChainNodeSpec{Cosmosigner: &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{
			Software: &appsv1.CosmosignerSoftwareBackend{PrivateKeySecret: ptr.To("current-key")},
		}}},
		Status: appsv1.ChainNodeStatus{
			ChainID: "chain-1", CosmosignerAppliedDigest: "current-digest", CosmosignerPublicKey: "current-public-key",
		},
	}
	local := standaloneReservationLifecycleObject("local", "local-uid", owner, owner.Name, "local-public-key")
	current := standaloneReservationLifecycleObject("current", "current-uid", owner, standaloneCosmosignerReservationClaim(owner), owner.Status.CosmosignerPublicKey)
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(owner, local, current).Build()
	r := &Reconciler{Client: c, APIReader: c}

	done, err := r.reconcileConsensusKeyReservationClaims(context.Background(), owner)
	if err != nil || !done {
		t.Fatalf("retained claim reconciliation failed, done=%v err=%v", done, err)
	}
	for _, reservation := range []*appsv1.ConsensusKeyReservation{local, current} {
		if err := c.Get(context.Background(), client.ObjectKeyFromObject(reservation), &appsv1.ConsensusKeyReservation{}); err != nil {
			t.Fatalf("retained reservation %q was released: %v", reservation.Name, err)
		}
	}
}

func TestReconcileConsensusKeyReservationClaimsBlocksUnsafeStandaloneSigningPaths(t *testing.T) {
	tests := []struct {
		name   string
		object func(*appsv1.ChainNode) client.Object
	}{
		{
			name: "managed one-shot Job",
			object: func(owner *appsv1.ChainNode) client.Object {
				return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: cosmosignerName(owner) + "-import", Namespace: owner.Namespace, UID: "job-uid"}}
			},
		},
		{
			name: "direct one-shot Pod",
			object: func(owner *appsv1.ChainNode) client.Object {
				return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: cosmosignerName(owner) + "-pubkey", Namespace: owner.Namespace, UID: "pod-uid"}}
			},
		},
		{
			name: "generated Job Pod",
			object: func(owner *appsv1.ChainNode) client.Object {
				return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: cosmosignerName(owner) + "-import-generated", Namespace: owner.Namespace, UID: "pod-uid"}}
			},
		},
		{
			name: "orphaned signer StatefulSet",
			object: func(owner *appsv1.ChainNode) client.Object {
				return &appsk8sv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{Name: cosmosignerName(owner), Namespace: owner.Namespace, UID: "sts-uid"}}
			},
		},
		{
			name: "orphaned signer replica Pod",
			object: func(owner *appsv1.ChainNode) client.Object {
				return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: cosmosignerName(owner) + "-0", Namespace: owner.Namespace, UID: "pod-uid"}}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := reservationLifecycleScheme(t)
			owner := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: "sentry", Namespace: "default", UID: "owner-uid"}}
			previous := owner.DeepCopy()
			previous.Spec.Cosmosigner = &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{
				Software: &appsv1.CosmosignerSoftwareBackend{PrivateKeySecret: ptr.To("old-key")},
			}}
			reservation := standaloneReservationLifecycleObject("obsolete", "obsolete-uid", owner, standaloneCosmosignerReservationClaim(previous), "old-public-key")
			path := tt.object(owner)
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(owner, reservation, path).Build()
			r := &Reconciler{Client: c, APIReader: c}

			done, err := r.reconcileConsensusKeyReservationClaims(context.Background(), owner)
			if err == nil || done || !strings.Contains(err.Error(), path.GetName()) {
				t.Fatalf("unsafe standalone signing path must block claim retirement, done=%v err=%v", done, err)
			}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(reservation), &appsv1.ConsensusKeyReservation{}); err != nil {
				t.Fatalf("reservation was released while signing path %q remained: %v", path.GetName(), err)
			}
		})
	}
}

func standaloneReservationLifecycleObject(name, uid string, owner *appsv1.ChainNode, claim, publicKey string) *appsv1.ConsensusKeyReservation {
	return &appsv1.ConsensusKeyReservation{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(uid)},
		Spec: appsv1.ConsensusKeyReservationSpec{
			ChainID: "chain-1", PublicKey: publicKey, OwnerUID: owner.UID, OwnerKind: "ChainNode",
			Namespace: owner.Namespace, OwnerName: owner.Name, Claim: claim,
		},
	}
}

func reservationLifecycleScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
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
	return scheme
}

const reservationLifecyclePublicKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
