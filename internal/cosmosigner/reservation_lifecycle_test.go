package cosmosigner

import (
	"context"
	"errors"
	"strings"
	"testing"

	appsk8sv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
)

func TestEnsureConsensusKeyReservationOwnerFinalizerPersistsBeforeClaim(t *testing.T) {
	scheme := reservationLifecycleScheme(t)
	owner := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{
		Name: "validator", Namespace: "default", UID: "owner-uid",
	}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(owner).Build()

	changed, err := EnsureConsensusKeyReservationOwnerFinalizer(context.Background(), c, c, owner)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("the lifecycle finalizer must be persisted before the first reservation")
	}

	fresh := &appsv1.ChainNode{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(owner), fresh); err != nil {
		t.Fatal(err)
	}
	if !containsString(fresh.Finalizers, ReservationOwnerFinalizer) {
		t.Fatalf("missing lifecycle finalizer: %v", fresh.Finalizers)
	}
}

func TestReleaseConsensusKeyReservationsDeletesOnlyExactOwnerAndConfirmsAbsence(t *testing.T) {
	scheme := reservationLifecycleScheme(t)
	owner := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{
		Name: "nodes", Namespace: "default", UID: "owner-uid",
	}}
	exact := reservationLifecycleObject("exact", "ckr-exact", ReservationHolder{
		UID: owner.UID, Kind: "ChainNodeSet", Namespace: owner.Namespace, Name: owner.Name, Claim: "nodes-validator",
	})
	foreign := reservationLifecycleObject("foreign", "ckr-foreign", ReservationHolder{
		UID: "foreign-uid", Kind: "ChainNodeSet", Namespace: owner.Namespace, Name: owner.Name, Claim: "nodes-validator",
	})
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(owner, exact, foreign).Build()
	tracked := &reservationLifecycleTrackingClient{Client: base}

	released, done, err := ReleaseConsensusKeyReservations(context.Background(), tracked, tracked, owner)
	if err != nil {
		t.Fatal(err)
	}
	if !done {
		t.Fatal("release must complete after the exact reservation is confirmed absent")
	}
	if len(released) != 1 || released[0] != exact.Name {
		t.Fatalf("released reservations = %v, want [%s]", released, exact.Name)
	}
	if tracked.deleteUID != exact.UID {
		t.Fatalf("delete UID precondition = %q, want %q", tracked.deleteUID, exact.UID)
	}
	if !tracked.confirmedAbsent {
		t.Fatal("release must perform an uncached absence confirmation after delete")
	}
	if err := base.Get(context.Background(), client.ObjectKeyFromObject(foreign), &appsv1.ConsensusKeyReservation{}); err != nil {
		t.Fatalf("foreign reservation was modified: %v", err)
	}
}

func TestReleaseConsensusKeyReservationsFailsClosedOnOwnerMetadataMismatch(t *testing.T) {
	scheme := reservationLifecycleScheme(t)
	owner := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{
		Name: "validator", Namespace: "default", UID: "owner-uid",
	}}
	inconsistent := reservationLifecycleObject("inconsistent", "ckr-inconsistent", ReservationHolder{
		UID: owner.UID, Kind: "ChainNode", Namespace: owner.Namespace, Name: "different-name", Claim: owner.Name,
	})
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(owner, inconsistent).Build()

	_, _, err := ReleaseConsensusKeyReservations(context.Background(), c, c, owner)
	if err == nil {
		t.Fatal("a matching UID with inconsistent owner metadata must block automatic release")
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(inconsistent), &appsv1.ConsensusKeyReservation{}); err != nil {
		t.Fatalf("inconsistent reservation must remain for operator inspection: %v", err)
	}
}

func TestCleanupManagedSigningPathWaitsForOrphanedJobPod(t *testing.T) {
	scheme := reservationLifecycleScheme(t)
	owner := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{
		Name: "validator", Namespace: "default", UID: "owner-uid",
	}}
	jobName := owner.Name + "-tmkms-vault-upload"
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: jobName + "-generated", Namespace: owner.Namespace, UID: "pod-uid",
	}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(owner, pod).Build()

	result, err := CleanupManagedSigningPath(context.Background(), c, c, owner, owner.Namespace, ManagedSigningPath{
		OneShotNames: []string{jobName},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Done || !strings.Contains(result.Waiting, pod.Name) {
		t.Fatalf("orphaned generated Job pod must keep cleanup pending: %+v", result)
	}
}

func TestFinalizeConsensusKeySigningPathsWaitsForOrphanedRootJobPod(t *testing.T) {
	scheme := reservationLifecycleScheme(t)
	owner := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{
		Name: "nodes", Namespace: "default", UID: "owner-uid",
	}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "nodes-signer-import-generated", Namespace: owner.Namespace, UID: "pod-uid",
	}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(owner, pod).Build()

	done, err := FinalizeConsensusKeySigningPaths(context.Background(), c, c, owner, owner.Namespace)
	if err != nil {
		t.Fatal(err)
	}
	if done {
		t.Fatal("orphaned generated root Job pod must keep owner finalization pending")
	}
}

func TestEnsureConsensusKeyReservationRecoversStaleOwnerWithoutDeletingRetainedState(t *testing.T) {
	scheme := reservationLifecycleScheme(t)
	holder := ReservationHolder{
		UID: "new-owner-uid", Kind: "ChainNode", Namespace: "default", Name: "validator", Claim: "validator",
	}
	stale := reservationLifecycleObject(ConsensusKeyReservationName("chain-1", reservationTestPublicKey), "ckr-stale", ReservationHolder{
		UID: "old-owner-uid", Kind: "ChainNode", Namespace: "default", Name: "validator", Claim: "validator",
	})
	currentOwner := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{
		Name: holder.Name, Namespace: holder.Namespace, UID: holder.UID, Finalizers: []string{ReservationOwnerFinalizer},
	}}
	retainedKey := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "validator-key", Namespace: holder.Namespace}}
	retainedPVC := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "validator", Namespace: holder.Namespace}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(currentOwner, stale, retainedKey, retainedPVC).Build()

	result, err := EnsureConsensusKeyReservationWithResult(
		context.Background(), c, c, "chain-1", reservationTestPublicKey, holder,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.RecoveredReservation != stale.Name {
		t.Fatalf("recovered reservation = %q, want %q", result.RecoveredReservation, stale.Name)
	}

	fresh := &appsv1.ConsensusKeyReservation{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: stale.Name}, fresh); err != nil {
		t.Fatal(err)
	}
	if fresh.Spec.OwnerUID != holder.UID || fresh.Spec.Claim != holder.Claim {
		t.Fatalf("stale reservation was not replaced by the exact new owner/claim: %#v", fresh.Spec)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(retainedKey), &corev1.Secret{}); err != nil {
		t.Fatalf("retained signing key must be preserved: %v", err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(retainedPVC), &corev1.PersistentVolumeClaim{}); err != nil {
		t.Fatalf("retained data PVC must be preserved: %v", err)
	}
}

func TestEnsureConsensusKeyReservationRefusesStaleRecoveryWhileSigningPathExists(t *testing.T) {
	tests := []struct {
		name string
		path client.Object
	}{
		{
			name: "standalone validator pod",
			path: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Name: "validator", Namespace: "default",
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNode", Name: "validator",
					UID: "old-owner-uid", Controller: ptr.To(true),
				}},
			}},
		},
		{
			name: "managed cosmosigner statefulset",
			path: &appsk8sv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
				Name: "validator-signer", Namespace: "default",
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNode", Name: "validator",
					UID: "old-owner-uid", Controller: ptr.To(true),
				}},
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := reservationLifecycleScheme(t)
			holder := ReservationHolder{
				UID: "new-owner-uid", Kind: "ChainNode", Namespace: "default", Name: "validator", Claim: "validator",
			}
			stale := reservationLifecycleObject(ConsensusKeyReservationName("chain-1", reservationTestPublicKey), "ckr-stale", ReservationHolder{
				UID: "old-owner-uid", Kind: "ChainNode", Namespace: "default", Name: "validator", Claim: "validator",
			})
			currentOwner := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{
				Name: holder.Name, Namespace: holder.Namespace, UID: holder.UID, Finalizers: []string{ReservationOwnerFinalizer},
			}}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(currentOwner, stale, tt.path).Build()

			_, err := EnsureConsensusKeyReservationWithResult(
				context.Background(), c, c, "chain-1", reservationTestPublicKey, holder,
			)
			if !errors.Is(err, ErrConsensusKeyReservationConflict) {
				t.Fatalf("live signing path must keep stale reservation conflict, got %v", err)
			}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(stale), &appsv1.ConsensusKeyReservation{}); err != nil {
				t.Fatalf("stale reservation must remain while signing is possible: %v", err)
			}
		})
	}
}

func TestEnsureConsensusKeyReservationRefusesStaleChainNodeSetRecoveryWhileChildExists(t *testing.T) {
	scheme := reservationLifecycleScheme(t)
	holder := ReservationHolder{
		UID: "new-owner-uid", Kind: "ChainNodeSet", Namespace: "default", Name: "nodes", Claim: "nodes-validator",
	}
	stale := reservationLifecycleObject(ConsensusKeyReservationName("chain-1", reservationTestPublicKey), "ckr-stale", ReservationHolder{
		UID: "old-owner-uid", Kind: "ChainNodeSet", Namespace: "default", Name: "nodes", Claim: "nodes-validator",
	})
	currentOwner := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{
		Name: holder.Name, Namespace: holder.Namespace, UID: holder.UID, Finalizers: []string{ReservationOwnerFinalizer},
	}}
	child := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{
		Name: holder.Claim, Namespace: holder.Namespace, UID: "child-uid",
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNodeSet", Name: "nodes",
			UID: "old-owner-uid", Controller: ptr.To(true),
		}},
	}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(currentOwner, stale, child).Build()

	_, err := EnsureConsensusKeyReservationWithResult(
		context.Background(), c, c, "chain-1", reservationTestPublicKey, holder,
	)
	if !errors.Is(err, ErrConsensusKeyReservationConflict) {
		t.Fatalf("an old root child must keep stale reservation conflict, got %v", err)
	}
}

func TestEnsureConsensusKeyReservationReportsBlockedStaleRecoveryWhileManagedJobExists(t *testing.T) {
	scheme := reservationLifecycleScheme(t)
	holder := ReservationHolder{
		UID: "new-owner-uid", Kind: "ChainNode", Namespace: "default", Name: "validator", Claim: "validator",
	}
	stale := reservationLifecycleObject(ConsensusKeyReservationName("chain-1", reservationTestPublicKey), "ckr-stale", ReservationHolder{
		UID: "old-owner-uid", Kind: "ChainNode", Namespace: "default", Name: "validator", Claim: "validator",
	})
	currentOwner := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{
		Name: holder.Name, Namespace: holder.Namespace, UID: holder.UID, Finalizers: []string{ReservationOwnerFinalizer},
	}}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: "validator-tmkms-vault-upload", Namespace: holder.Namespace, UID: "job-uid",
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNode", Name: holder.Name,
			UID: "old-owner-uid", Controller: ptr.To(true),
		}},
	}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "validator-tmkms-vault-upload-pod", Namespace: holder.Namespace,
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: batchv1.SchemeGroupVersion.String(), Kind: "Job", Name: job.Name,
			UID: job.UID, Controller: ptr.To(true),
		}},
	}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(currentOwner, stale, job, pod).Build()

	_, err := EnsureConsensusKeyReservationWithResult(
		context.Background(), c, c, "chain-1", reservationTestPublicKey, holder,
	)
	if !errors.Is(err, ErrConsensusKeyReservationConflict) {
		t.Fatalf("managed stale-owner Job must preserve the reservation conflict, got %v", err)
	}
	if !errors.Is(err, ErrConsensusKeyReservationRecoveryBlocked) {
		t.Fatalf("managed stale-owner Job must report blocked recovery, got %v", err)
	}
	if !strings.Contains(err.Error(), job.Name) {
		t.Fatalf("blocked recovery error must name the Job, got %v", err)
	}
}

func TestEnsureConsensusKeyReservationBlocksRecoveryForOrphanedManagedJobPod(t *testing.T) {
	scheme := reservationLifecycleScheme(t)
	holder := ReservationHolder{
		UID: "new-owner-uid", Kind: "ChainNode", Namespace: "default", Name: "validator", Claim: "validator",
	}
	stale := reservationLifecycleObject(ConsensusKeyReservationName("chain-1", reservationTestPublicKey), "ckr-stale", ReservationHolder{
		UID: "old-owner-uid", Kind: "ChainNode", Namespace: "default", Name: "validator", Claim: "validator",
	})
	currentOwner := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{
		Name: holder.Name, Namespace: holder.Namespace, UID: holder.UID, Finalizers: []string{ReservationOwnerFinalizer},
	}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "validator-tmkms-vault-upload-r4ndm", Namespace: holder.Namespace,
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: batchv1.SchemeGroupVersion.String(), Kind: "Job",
			Name: "validator-tmkms-vault-upload", UID: "deleted-job-uid", Controller: ptr.To(true),
		}},
	}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(currentOwner, stale, pod).Build()

	_, err := EnsureConsensusKeyReservationWithResult(
		context.Background(), c, c, "chain-1", reservationTestPublicKey, holder,
	)
	if !errors.Is(err, ErrConsensusKeyReservationRecoveryBlocked) {
		t.Fatalf("orphaned managed Job pod must block stale recovery, got %v", err)
	}
	if !strings.Contains(err.Error(), pod.Name) {
		t.Fatalf("blocked recovery error must name the orphaned Job pod, got %v", err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(stale), &appsv1.ConsensusKeyReservation{}); err != nil {
		t.Fatalf("stale reservation must remain while orphaned Job pod exists: %v", err)
	}
}

func TestEnsureConsensusKeyReservationRefusesMalformedStaleReservation(t *testing.T) {
	scheme := reservationLifecycleScheme(t)
	holder := ReservationHolder{UID: "new-owner-uid", Kind: "ChainNode", Namespace: "default", Name: "validator", Claim: "validator"}
	stale := reservationLifecycleObject(ConsensusKeyReservationName("chain-1", reservationTestPublicKey), "ckr-stale", ReservationHolder{
		UID: "old-owner-uid", Kind: "ChainNode", Namespace: "default", Name: "validator",
	})
	currentOwner := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{
		Name: holder.Name, Namespace: holder.Namespace, UID: holder.UID, Finalizers: []string{ReservationOwnerFinalizer},
	}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(currentOwner, stale).Build()

	_, err := EnsureConsensusKeyReservationWithResult(context.Background(), c, c, "chain-1", reservationTestPublicKey, holder)
	if !errors.Is(err, ErrConsensusKeyReservationConflict) {
		t.Fatalf("malformed stale reservation must fail closed, got %v", err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(stale), &appsv1.ConsensusKeyReservation{}); err != nil {
		t.Fatalf("malformed stale reservation must remain for inspection: %v", err)
	}
}

func TestFinalizeConsensusKeySigningPathsBlocksForeignManagedJob(t *testing.T) {
	scheme := reservationLifecycleScheme(t)
	owner := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: "validator", Namespace: "default", UID: "owner-uid"}}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "validator-tmkms-vault-upload", Namespace: owner.Namespace, UID: "foreign-job-uid"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(owner, job).Build()

	done, err := FinalizeConsensusKeySigningPaths(context.Background(), c, c, owner, owner.Namespace)
	if err == nil || done || !strings.Contains(err.Error(), job.Name) {
		t.Fatalf("foreign managed Job must block finalization, done=%v err=%v", done, err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(job), &batchv1.Job{}); err != nil {
		t.Fatalf("foreign managed Job must not be modified: %v", err)
	}
}

func TestCleanupManagedSigningPathDeletesJobPodBeforeJob(t *testing.T) {
	scheme := reservationLifecycleScheme(t)
	owner := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{
		Name: "validator", Namespace: "default", UID: "owner-uid",
	}}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: "validator-tmkms-generate-identity", Namespace: owner.Namespace, UID: "job-uid",
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNode", Name: owner.Name,
			UID: owner.UID, Controller: ptr.To(true),
		}},
	}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "validator-tmkms-generate-identity-pod", Namespace: owner.Namespace, UID: "pod-uid",
		Finalizers: []string{"test.voluzi.com/hold"},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: batchv1.SchemeGroupVersion.String(), Kind: "Job", Name: job.Name,
			UID: job.UID, Controller: ptr.To(true),
		}},
	}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(owner, job, pod).Build()

	result, err := CleanupManagedSigningPath(context.Background(), c, c, owner, owner.Namespace, ManagedSigningPath{
		OneShotNames: []string{job.Name},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Done || !strings.Contains(result.Waiting, pod.Name) {
		t.Fatalf("cleanup result = %#v, want wait for Job pod %q", result, pod.Name)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(job), &batchv1.Job{}); err != nil {
		t.Fatalf("Job must remain until its pod is confirmed absent: %v", err)
	}
	freshPod := &corev1.Pod{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(pod), freshPod); err != nil {
		t.Fatal(err)
	}
	if freshPod.DeletionTimestamp == nil {
		t.Fatal("Job pod deletion was not requested")
	}
	freshPod.Finalizers = nil
	if err := c.Update(context.Background(), freshPod); err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}

	result, err = CleanupManagedSigningPath(context.Background(), c, c, owner, owner.Namespace, ManagedSigningPath{
		OneShotNames: []string{job.Name},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Done {
		t.Fatalf("cleanup must complete after Job and pod absence: %#v", result)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(job), &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Fatalf("Job must be confirmed absent, got %v", err)
	}
}

func TestCleanupManagedSigningPathBlocksPodFromPreviousJobUID(t *testing.T) {
	scheme := reservationLifecycleScheme(t)
	owner := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{
		Name: "validator", Namespace: "default", UID: "owner-uid",
	}}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: "validator-tmkms-vault-upload", Namespace: owner.Namespace, UID: "current-job-uid",
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNode", Name: owner.Name,
			UID: owner.UID, Controller: ptr.To(true),
		}},
	}}
	oldPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: job.Name + "-old", Namespace: owner.Namespace, UID: "old-pod-uid",
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: batchv1.SchemeGroupVersion.String(), Kind: "Job", Name: job.Name,
			UID: "previous-job-uid", Controller: ptr.To(true),
		}},
	}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(owner, job, oldPod).Build()

	result, err := CleanupManagedSigningPath(context.Background(), c, c, owner, owner.Namespace, ManagedSigningPath{
		OneShotNames: []string{job.Name},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Done || !strings.Contains(result.Blocked, oldPod.Name) {
		t.Fatalf("previous-UID Job pod must block cleanup: %+v", result)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(job), &batchv1.Job{}); err != nil {
		t.Fatalf("current Job must remain while an ambiguous old pod exists: %v", err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(oldPod), &corev1.Pod{}); err != nil {
		t.Fatalf("previous-UID Job pod must not be modified: %v", err)
	}
}

func TestManagedSigningOneShotPodJobNameUsesLastMarker(t *testing.T) {
	name := "validator-import-copy-tmkms-vault-upload-generated"
	got, ok := managedSigningOneShotPodJobName(name)
	if !ok {
		t.Fatalf("expected %q to be recognized as a generated managed Job pod", name)
	}
	want := "validator-import-copy-tmkms-vault-upload"
	if got != want {
		t.Fatalf("managed Job name = %q, want %q", got, want)
	}
}

func TestFinalizeConsensusKeySigningPathsRetainsPVC(t *testing.T) {
	scheme := reservationLifecycleScheme(t)
	owner := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{
		Name: "validator", Namespace: "default", UID: "owner-uid",
	}}
	sts := &appsk8sv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "validator-signer", Namespace: owner.Namespace, UID: "signer-uid",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNode", Name: owner.Name,
				UID: owner.UID, Controller: ptr.To(true),
			}},
		},
		Spec: appsk8sv1.StatefulSetSpec{
			Replicas: ptr.To(int32(0)),
			PersistentVolumeClaimRetentionPolicy: &appsk8sv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
				WhenDeleted: appsk8sv1.RetainPersistentVolumeClaimRetentionPolicyType,
				WhenScaled:  appsk8sv1.RetainPersistentVolumeClaimRetentionPolicyType,
			},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: InstanceLabels("validator-signer")}},
		},
		Status: appsk8sv1.StatefulSetStatus{ObservedGeneration: 1},
	}
	sts.Generation = 1
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{
		Name: dataVolumeName + "-" + sts.Name + "-0", Namespace: owner.Namespace,
		Labels: pvcOwnerLabels(sts.Name, owner.UID), Finalizers: []string{RetainedStateFinalizer},
	}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(owner, sts, pvc).Build()

	done, err := FinalizeConsensusKeySigningPaths(context.Background(), c, c, owner, owner.Namespace)
	if err != nil {
		t.Fatal(err)
	}
	if !done {
		t.Fatal("zero-replica signer with no pods must be fully torn down")
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(sts), &appsk8sv1.StatefulSet{}); !apierrors.IsNotFound(err) {
		t.Fatalf("signer StatefulSet must be absent, got %v", err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(pvc), &corev1.PersistentVolumeClaim{}); err != nil {
		t.Fatalf("reservation teardown must not delete retained signer PVCs: %v", err)
	}
}

func reservationLifecycleScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := reservationScheme(t)
	if err := appsk8sv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func reservationLifecycleObject(name, uid string, holder ReservationHolder) *appsv1.ConsensusKeyReservation {
	return &appsv1.ConsensusKeyReservation{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(uid)},
		Spec: appsv1.ConsensusKeyReservationSpec{
			ChainID: "chain-1", PublicKey: reservationTestPublicKey,
			OwnerUID: holder.UID, OwnerKind: holder.Kind, Namespace: holder.Namespace,
			OwnerName: holder.Name, Claim: holder.Claim,
		},
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

type reservationLifecycleTrackingClient struct {
	client.Client
	deleteUID       types.UID
	deletedName     string
	confirmedAbsent bool
}

func (c *reservationLifecycleTrackingClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	deleteOptions := (&client.DeleteOptions{}).ApplyOptions(opts)
	if deleteOptions.Preconditions != nil && deleteOptions.Preconditions.UID != nil {
		c.deleteUID = *deleteOptions.Preconditions.UID
	}
	c.deletedName = obj.GetName()
	return c.Client.Delete(ctx, obj, opts...)
}

func (c *reservationLifecycleTrackingClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	err := c.Client.Get(ctx, key, obj, opts...)
	if key.Name == c.deletedName && apierrors.IsNotFound(err) {
		c.confirmedAbsent = true
	}
	return err
}
