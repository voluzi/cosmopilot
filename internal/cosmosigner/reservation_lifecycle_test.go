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
	"k8s.io/apimachinery/pkg/runtime/schema"
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

func TestEnsureConsensusKeyReservationOwnerFinalizerSynchronizesCachedOwner(t *testing.T) {
	tests := []struct {
		name                    string
		finalizerAlreadyPresent bool
		wantChanged             bool
	}{
		{name: "already present", finalizerAlreadyPresent: true, wantChanged: false},
		{name: "newly patched", wantChanged: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := reservationLifecycleScheme(t)
			owner := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{
				Name: "validator", Namespace: "default", UID: "owner-uid", ResourceVersion: "1",
				Labels: map[string]string{"cached": "stale"}, Finalizers: []string{"example.com/other"},
			}}
			freshOwner := owner.DeepCopy()
			freshOwner.ResourceVersion = ""
			freshOwner.Labels = map[string]string{"concurrent": "preserve"}
			freshOwner.Spec.Validator = &appsv1.ValidatorConfig{}
			if tt.finalizerAlreadyPresent {
				freshOwner.Finalizers = append(freshOwner.Finalizers, ReservationOwnerFinalizer)
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(freshOwner).Build()

			changed, err := EnsureConsensusKeyReservationOwnerFinalizer(context.Background(), c, c, owner)
			if err != nil {
				t.Fatal(err)
			}
			if changed != tt.wantChanged {
				t.Fatalf("changed = %v, want %v", changed, tt.wantChanged)
			}

			confirmed := &appsv1.ChainNode{}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(owner), confirmed); err != nil {
				t.Fatal(err)
			}
			if !containsString(confirmed.Finalizers, ReservationOwnerFinalizer) {
				t.Fatalf("missing lifecycle finalizer in API-server state: %v", confirmed.Finalizers)
			}
			if confirmed.Labels["concurrent"] != "preserve" {
				t.Fatalf("concurrent metadata edit was lost: %v", confirmed.Labels)
			}
			if owner.Labels["concurrent"] != "preserve" {
				t.Fatalf("synchronized owner is missing concurrent metadata: %v", owner.Labels)
			}
			if _, stale := owner.Labels["cached"]; stale {
				t.Fatalf("synchronized owner retained stale cached metadata: %v", owner.Labels)
			}
			if owner.Spec.Validator == nil {
				t.Fatal("synchronized owner is missing the fresh spec")
			}
			if !containsString(owner.Finalizers, "example.com/other") ||
				!containsString(owner.Finalizers, ReservationOwnerFinalizer) {
				t.Fatalf("synchronized owner finalizers = %v", owner.Finalizers)
			}
			if owner.ResourceVersion != confirmed.ResourceVersion {
				t.Fatalf("synchronized owner resourceVersion = %q, want %q", owner.ResourceVersion, confirmed.ResourceVersion)
			}

			owner.Labels["reconciled"] = "true"
			if err := c.Update(context.Background(), owner); err != nil {
				t.Fatalf("full-object update from synchronized owner failed: %v", err)
			}
			updated := &appsv1.ChainNode{}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(owner), updated); err != nil {
				t.Fatal(err)
			}
			if updated.Labels["concurrent"] != "preserve" || updated.Labels["reconciled"] != "true" {
				t.Fatalf("full-object update lost synchronized metadata: %v", updated.Labels)
			}
			if _, stale := updated.Labels["cached"]; stale {
				t.Fatalf("full-object update restored stale cached metadata: %v", updated.Labels)
			}
		})
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

func TestFinalizeConsensusKeySigningPathsBlocksExactForeignOneShotPod(t *testing.T) {
	scheme := reservationLifecycleScheme(t)
	owner := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: "validator", Namespace: "default", UID: "owner-uid"}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "validator-signer-import", Namespace: owner.Namespace, UID: "pod-uid"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(owner, pod).Build()

	done, err := FinalizeConsensusKeySigningPaths(context.Background(), c, c, owner, owner.Namespace)
	if err == nil || done || !strings.Contains(err.Error(), pod.Name) {
		t.Fatalf("exact foreign one-shot Pod must block finalization, done=%v err=%v", done, err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(pod), &corev1.Pod{}); err != nil {
		t.Fatalf("foreign one-shot Pod must remain untouched: %v", err)
	}
}

func TestFinalizeConsensusKeySigningPathsUsesExactOneShotLabelsBeforeNameFallback(t *testing.T) {
	tests := []struct {
		name  string
		owner client.Object
		path  client.Object
	}{
		{
			name:  "ChainNodeSet Job",
			owner: &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "default", UID: "owner-uid"}},
			path: &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
				Name: "foo-bar-signer-import", Namespace: "default", UID: "job-uid", Labels: map[string]string{"nodeset": "foo-bar"},
			}},
		},
		{
			name:  "ChainNodeSet direct Pod",
			owner: &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "default", UID: "owner-uid"}},
			path: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Name: "foo-bar-signer-pubkey", Namespace: "default", UID: "pod-uid", Labels: map[string]string{"nodeset": "foo-bar"},
			}},
		},
		{
			name:  "ChainNodeSet generated Job Pod",
			owner: &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{Name: "foo", Namespace: "default", UID: "owner-uid"}},
			path: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Name: "foo-bar-signer-import-generated", Namespace: "default", UID: "pod-uid", Labels: map[string]string{"nodeset": "foo-bar"},
			}},
		},
		{
			name:  "ChainNode Job",
			owner: &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: "validator", Namespace: "default", UID: "owner-uid"}},
			path: &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
				Name: "validator-signer-import", Namespace: "default", UID: "job-uid", Labels: map[string]string{"chain-node": "validator-copy"},
			}},
		},
		{
			name:  "ChainNode direct Pod attributed to node set",
			owner: &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: "validator", Namespace: "default", UID: "owner-uid"}},
			path: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Name: "validator-signer-pubkey", Namespace: "default", UID: "pod-uid", Labels: map[string]string{"nodeset": "validators"},
			}},
		},
		{
			name:  "ChainNode generated Job Pod",
			owner: &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: "validator", Namespace: "default", UID: "owner-uid"}},
			path: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Name: "validator-signer-import-generated", Namespace: "default", UID: "pod-uid", Labels: map[string]string{"chain-node": "validator-copy"},
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := reservationLifecycleScheme(t)
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tt.owner, tt.path).Build()

			done, err := FinalizeConsensusKeySigningPaths(context.Background(), c, c, tt.owner, tt.owner.GetNamespace())
			if err != nil || !done {
				t.Fatalf("foreign-attributed one-shot path must be ignored, done=%v err=%v", done, err)
			}
			remaining, ok := tt.path.DeepCopyObject().(client.Object)
			if !ok {
				t.Fatalf("path %T is not a Kubernetes object", tt.path)
			}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(tt.path), remaining); err != nil {
				t.Fatalf("foreign-attributed one-shot path was modified: %v", err)
			}
		})
	}
}

func TestFinalizeConsensusKeySigningPathsLeavesChildOneShotPodToChainNodeSetCleanup(t *testing.T) {
	scheme := reservationLifecycleScheme(t)
	owner := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{Name: "nodes", Namespace: "default", UID: "owner-uid"}}
	child := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{
		Name: "nodes-validator", Namespace: owner.Namespace, UID: "child-uid",
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNodeSet", Name: owner.Name,
			UID: owner.UID, Controller: ptr.To(true),
		}},
	}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: child.Name + "-tmkms-vault-upload", Namespace: owner.Namespace, UID: "pod-uid",
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNode", Name: child.Name,
			UID: child.UID, Controller: ptr.To(true),
		}},
	}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(owner, child, pod).Build()

	done, err := FinalizeConsensusKeySigningPaths(context.Background(), c, c, owner, owner.Namespace)
	if err != nil || !done {
		t.Fatalf("child-owned one-shot Pod must be left to child cleanup, done=%v err=%v", done, err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(pod), &corev1.Pod{}); err != nil {
		t.Fatalf("child-owned one-shot Pod must remain untouched: %v", err)
	}
}

func TestFinalizeConsensusKeySigningPathsWaitsForLabelLessSignerReplica(t *testing.T) {
	scheme := reservationLifecycleScheme(t)
	owner := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: "validator", Namespace: "default", UID: "owner-uid"}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "validator-signer-0", Namespace: owner.Namespace, UID: "pod-uid"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(owner, pod).Build()

	done, err := FinalizeConsensusKeySigningPaths(context.Background(), c, c, owner, owner.Namespace)
	if err != nil {
		t.Fatal(err)
	}
	if done {
		t.Fatal("label-less deterministic signer replica must keep finalization pending")
	}
}

func TestFinalizeConsensusKeySigningPathsDeletesOwnedLabelLessDeterministicChainNodeSetSigner(t *testing.T) {
	scheme := reservationLifecycleScheme(t)
	owner := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{Name: "nodes", Namespace: "default", UID: "owner-uid"}}
	sts := &appsk8sv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "nodes-legacy-signer", Namespace: owner.Namespace, UID: "signer-uid",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNodeSet", Name: owner.Name,
				UID: owner.UID, Controller: ptr.To(true),
			}},
		},
		Spec: appsk8sv1.StatefulSetSpec{
			Replicas: ptr.To(int32(0)),
			PersistentVolumeClaimRetentionPolicy: &appsk8sv1.StatefulSetPersistentVolumeClaimRetentionPolicy{
				WhenDeleted: appsk8sv1.RetainPersistentVolumeClaimRetentionPolicyType,
				WhenScaled:  appsk8sv1.RetainPersistentVolumeClaimRetentionPolicyType,
			},
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
		t.Fatal("label-less deterministic signer must be fully torn down")
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(sts), &appsk8sv1.StatefulSet{}); !apierrors.IsNotFound(err) {
		t.Fatalf("label-less deterministic signer StatefulSet must be absent, got %v", err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(pvc), &corev1.PersistentVolumeClaim{}); err != nil {
		t.Fatalf("retained signer PVC must be preserved: %v", err)
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
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		reservationLifecycleNamespace(holder.Namespace), currentOwner, stale, retainedKey, retainedPVC,
	).Build()

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

func TestEnsureConsensusKeyReservationRecoversStaleOwnerAfterNamespaceDeletion(t *testing.T) {
	scheme := reservationLifecycleScheme(t)
	holder := ReservationHolder{
		UID: "new-owner-uid", Kind: "ChainNodeSet", Namespace: "deleted", Name: "nodes", Claim: "signer-current",
	}
	stale := reservationLifecycleObject(ConsensusKeyReservationName("chain-1", reservationTestPublicKey), "ckr-stale", ReservationHolder{
		UID: "old-owner-uid", Kind: holder.Kind, Namespace: holder.Namespace, Name: holder.Name, Claim: holder.Claim,
	})
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(stale).Build()
	reader := &namespaceNotFoundListReader{Reader: base, namespace: holder.Namespace}

	result, err := EnsureConsensusKeyReservationWithResult(
		context.Background(), reader, base, "chain-1", reservationTestPublicKey, holder,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.RecoveredReservation != stale.Name {
		t.Fatalf("recovered reservation = %q, want %q", result.RecoveredReservation, stale.Name)
	}
	if reader.namespacedLists != 0 {
		t.Fatalf("deleted namespace recovery attempted %d namespaced Lists", reader.namespacedLists)
	}
}

func TestStaleReservationSigningPathsGonePreservesListNotFoundForExistingNamespace(t *testing.T) {
	scheme := reservationLifecycleScheme(t)
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "existing"}}
	stale := reservationLifecycleObject("stale", "ckr-stale", ReservationHolder{
		UID: "old-owner-uid", Kind: "ChainNodeSet", Namespace: namespace.Name, Name: "nodes", Claim: "signer-current",
	})
	base := fake.NewClientBuilder().WithScheme(scheme).WithObjects(namespace).Build()
	reader := &namespaceNotFoundListReader{Reader: base, namespace: namespace.Name}

	gone, _, err := staleReservationSigningPathsGone(context.Background(), reader, stale, ReservationHolder{})
	if err == nil || gone || !apierrors.IsNotFound(err) {
		t.Fatalf("List NotFound in an existing namespace must be preserved, gone=%v err=%v", gone, err)
	}
}

func TestEnsureConsensusKeyReservationKeepsStaleReservationWhenReplacementClaimConflicts(t *testing.T) {
	scheme := reservationLifecycleScheme(t)
	holder := ReservationHolder{
		UID: "new-owner-uid", Kind: "ChainNode", Namespace: "default", Name: "validator", Claim: "validator",
	}
	stale := reservationLifecycleObject(ConsensusKeyReservationName("chain-1", reservationTestPublicKey), "ckr-stale", ReservationHolder{
		UID: "old-owner-uid", Kind: holder.Kind, Namespace: holder.Namespace, Name: holder.Name, Claim: holder.Claim,
	})
	conflictingClaim := reservationLifecycleObject(ConsensusKeyReservationName("chain-1", reservationOtherPublicKey), "ckr-conflict", holder)
	conflictingClaim.Spec.PublicKey = reservationOtherPublicKey
	currentOwner := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{
		Name: holder.Name, Namespace: holder.Namespace, UID: holder.UID, Finalizers: []string{ReservationOwnerFinalizer},
	}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		reservationLifecycleNamespace(holder.Namespace), currentOwner, stale, conflictingClaim,
	).Build()

	_, err := EnsureConsensusKeyReservationWithResult(
		context.Background(), c, c, "chain-1", reservationTestPublicKey, holder,
	)
	if !errors.Is(err, ErrConsensusKeyReservationConflict) {
		t.Fatalf("conflicting replacement claim must reject stale recovery, got %v", err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(stale), &appsv1.ConsensusKeyReservation{}); err != nil {
		t.Fatalf("stale reservation must remain when replacement claim conflicts: %v", err)
	}
}

func TestEnsureConsensusKeyReservationKeepsStaleReservationWhenLegacyOwnerConflicts(t *testing.T) {
	scheme := reservationLifecycleScheme(t)
	holder := ReservationHolder{
		UID: "new-owner-uid", Kind: "ChainNode", Namespace: "default", Name: "validator", Claim: "validator",
	}
	stale := reservationLifecycleObject(ConsensusKeyReservationName("chain-1", reservationTestPublicKey), "ckr-stale", ReservationHolder{
		UID: "old-owner-uid", Kind: holder.Kind, Namespace: holder.Namespace, Name: holder.Name, Claim: holder.Claim,
	})
	currentOwner := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{
		Name: holder.Name, Namespace: holder.Namespace, UID: holder.UID, Finalizers: []string{ReservationOwnerFinalizer},
	}}
	legacyOwner := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "other-validator", Namespace: holder.Namespace, UID: "other-owner-uid"},
		Status:     appsv1.ChainNodeStatus{ChainID: "chain-1", CosmosignerPublicKey: reservationTestPublicKey},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		reservationLifecycleNamespace(holder.Namespace), currentOwner, legacyOwner, stale,
	).Build()

	_, err := EnsureConsensusKeyReservationWithResult(
		context.Background(), c, c, "chain-1", reservationTestPublicKey, holder,
	)
	if !errors.Is(err, ErrConsensusKeyReservationConflict) {
		t.Fatalf("legacy replacement owner must reject stale recovery, got %v", err)
	}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(stale), &appsv1.ConsensusKeyReservation{}); err != nil {
		t.Fatalf("stale reservation must remain when a legacy owner conflicts: %v", err)
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
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
				reservationLifecycleNamespace(holder.Namespace), currentOwner, stale, tt.path,
			).Build()

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
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		reservationLifecycleNamespace(holder.Namespace), currentOwner, stale, child,
	).Build()

	_, err := EnsureConsensusKeyReservationWithResult(
		context.Background(), c, c, "chain-1", reservationTestPublicKey, holder,
	)
	if !errors.Is(err, ErrConsensusKeyReservationConflict) {
		t.Fatalf("an old root child must keep stale reservation conflict, got %v", err)
	}
}

func TestEnsureConsensusKeyReservationUsesExactLabelsBeforeChainNodeSetNameFallback(t *testing.T) {
	tests := []struct {
		name    string
		objects func(ReservationHolder) []client.Object
	}{
		{
			name: "prefix-related signer StatefulSet",
			objects: func(holder ReservationHolder) []client.Object {
				return []client.Object{&appsk8sv1.StatefulSet{
					ObjectMeta: metav1.ObjectMeta{Name: "foo-bar-signer", Namespace: holder.Namespace, UID: "foreign-sts-uid"},
					Spec: appsk8sv1.StatefulSetSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
						"app.kubernetes.io/name": "cosmosigner", "nodeset": "foo-bar",
					}}}},
				}}
			},
		},
		{
			name: "prefix-related one-shot Job",
			objects: func(holder ReservationHolder) []client.Object {
				return []client.Object{&batchv1.Job{ObjectMeta: metav1.ObjectMeta{
					Name: "foo-bar-signer-import", Namespace: holder.Namespace, UID: "foreign-job-uid",
					Labels: map[string]string{"app.kubernetes.io/name": "cosmosigner", "nodeset": "foo-bar"},
				}}}
			},
		},
		{
			name: "prefix-related direct one-shot Pod",
			objects: func(holder ReservationHolder) []client.Object {
				return []client.Object{&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
					Name: "foo-bar-signer-import", Namespace: holder.Namespace, UID: "foreign-pod-uid",
					Labels: map[string]string{"app.kubernetes.io/name": "cosmosigner", "nodeset": "foo-bar"},
				}}}
			},
		},
		{
			name: "prefix-related signer replica Pod",
			objects: func(holder ReservationHolder) []client.Object {
				return []client.Object{&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
					Name: "foo-bar-signer-0", Namespace: holder.Namespace, UID: "foreign-pod-uid",
					Labels: map[string]string{"app.kubernetes.io/name": "cosmosigner", "nodeset": "foo-bar"},
				}}}
			},
		},
		{
			name: "current-holder signer and replica",
			objects: func(holder ReservationHolder) []client.Object {
				sts := &appsk8sv1.StatefulSet{
					ObjectMeta: metav1.ObjectMeta{
						Name: "foo-signer", Namespace: holder.Namespace, UID: "current-sts-uid",
						OwnerReferences: []metav1.OwnerReference{{
							APIVersion: appsv1.GroupVersion.String(), Kind: holder.Kind, Name: holder.Name,
							UID: holder.UID, Controller: ptr.To(true),
						}},
					},
					Spec: appsk8sv1.StatefulSetSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
						"app.kubernetes.io/name": "cosmosigner", "nodeset": holder.Name,
					}}}},
				}
				pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
					Name: sts.Name + "-0", Namespace: holder.Namespace, UID: "current-pod-uid",
					Labels: map[string]string{"app.kubernetes.io/name": "cosmosigner", "nodeset": holder.Name},
					OwnerReferences: []metav1.OwnerReference{{
						APIVersion: appsk8sv1.SchemeGroupVersion.String(), Kind: "StatefulSet", Name: sts.Name,
						UID: sts.UID, Controller: ptr.To(true),
					}},
				}}
				return []client.Object{sts, pod}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := reservationLifecycleScheme(t)
			holder := ReservationHolder{UID: "new-owner-uid", Kind: "ChainNodeSet", Namespace: "default", Name: "foo", Claim: "signer-current"}
			stale := reservationLifecycleObject(ConsensusKeyReservationName("chain-1", reservationTestPublicKey), "ckr-stale", ReservationHolder{
				UID: "old-owner-uid", Kind: holder.Kind, Namespace: holder.Namespace, Name: holder.Name, Claim: holder.Claim,
			})
			currentOwner := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{
				Name: holder.Name, Namespace: holder.Namespace, UID: holder.UID, Finalizers: []string{ReservationOwnerFinalizer},
			}}
			objects := []client.Object{reservationLifecycleNamespace(holder.Namespace), currentOwner, stale}
			objects = append(objects, tt.objects(holder)...)
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()

			result, err := EnsureConsensusKeyReservationWithResult(context.Background(), c, c, "chain-1", reservationTestPublicKey, holder)
			if err != nil {
				t.Fatalf("exact labels identifying a different owner must not block recovery: %v", err)
			}
			if result.RecoveredReservation != stale.Name {
				t.Fatalf("recovered reservation = %q, want %q", result.RecoveredReservation, stale.Name)
			}
		})
	}
}

func TestEnsureConsensusKeyReservationBlocksOrphanedChainNodeSetSignerBeforePodsExist(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
	}{
		{
			name: "exact pod-template labels",
			labels: map[string]string{
				"app.kubernetes.io/name": "cosmosigner", "nodeset": "foo",
			},
		},
		{name: "legacy label-less deterministic name"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := reservationLifecycleScheme(t)
			holder := ReservationHolder{UID: "new-owner-uid", Kind: "ChainNodeSet", Namespace: "default", Name: "foo", Claim: "signer-current"}
			stale := reservationLifecycleObject(ConsensusKeyReservationName("chain-1", reservationTestPublicKey), "ckr-stale", ReservationHolder{
				UID: "old-owner-uid", Kind: holder.Kind, Namespace: holder.Namespace, Name: holder.Name, Claim: holder.Claim,
			})
			currentOwner := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{
				Name: holder.Name, Namespace: holder.Namespace, UID: holder.UID, Finalizers: []string{ReservationOwnerFinalizer},
			}}
			sts := &appsk8sv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{Name: "foo-signer", Namespace: holder.Namespace, UID: "orphan-sts-uid"},
				Spec:       appsk8sv1.StatefulSetSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: tt.labels}}},
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
				reservationLifecycleNamespace(holder.Namespace), currentOwner, stale, sts,
			).Build()

			_, err := EnsureConsensusKeyReservationWithResult(context.Background(), c, c, "chain-1", reservationTestPublicKey, holder)
			if !errors.Is(err, ErrConsensusKeyReservationRecoveryBlocked) || !strings.Contains(err.Error(), sts.Name) {
				t.Fatalf("orphaned stale signer StatefulSet must block recovery before it creates Pods, got %v", err)
			}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(stale), &appsv1.ConsensusKeyReservation{}); err != nil {
				t.Fatalf("stale reservation must remain while orphaned signer StatefulSet exists: %v", err)
			}
		})
	}
}

func TestEnsureConsensusKeyReservationKeepsLabelLessChainNodeSetStalePaths(t *testing.T) {
	tests := []struct {
		name string
		path client.Object
	}{
		{name: "managed Job", path: &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "foo-signer-import", Namespace: "default", UID: "job-uid"}}},
		{name: "direct one-shot Pod", path: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "foo-signer-pubkey", Namespace: "default", UID: "pod-uid"}}},
		{name: "generated Job Pod", path: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "foo-signer-import-generated", Namespace: "default", UID: "pod-uid"}}},
		{name: "signer replica Pod", path: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "foo-signer-0", Namespace: "default", UID: "pod-uid"}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := reservationLifecycleScheme(t)
			holder := ReservationHolder{UID: "new-owner-uid", Kind: "ChainNodeSet", Namespace: "default", Name: "foo", Claim: "signer-current"}
			stale := reservationLifecycleObject(ConsensusKeyReservationName("chain-1", reservationTestPublicKey), "ckr-stale", ReservationHolder{
				UID: "old-owner-uid", Kind: holder.Kind, Namespace: holder.Namespace, Name: holder.Name, Claim: holder.Claim,
			})
			currentOwner := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{
				Name: holder.Name, Namespace: holder.Namespace, UID: holder.UID, Finalizers: []string{ReservationOwnerFinalizer},
			}}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
				reservationLifecycleNamespace(holder.Namespace), currentOwner, stale, tt.path,
			).Build()

			_, err := EnsureConsensusKeyReservationWithResult(context.Background(), c, c, "chain-1", reservationTestPublicKey, holder)
			if !errors.Is(err, ErrConsensusKeyReservationRecoveryBlocked) || !strings.Contains(err.Error(), tt.path.GetName()) {
				t.Fatalf("label-less deterministic stale path must block recovery, got %v", err)
			}
			if err := c.Get(context.Background(), client.ObjectKeyFromObject(stale), &appsv1.ConsensusKeyReservation{}); err != nil {
				t.Fatalf("stale reservation must remain while %q exists: %v", tt.path.GetName(), err)
			}
		})
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
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		reservationLifecycleNamespace(holder.Namespace), currentOwner, stale, job, pod,
	).Build()

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
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		reservationLifecycleNamespace(holder.Namespace), currentOwner, stale, pod,
	).Build()

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

func TestEnsureConsensusKeyReservationBlocksRecoveryForExactManagedOneShotPod(t *testing.T) {
	scheme := reservationLifecycleScheme(t)
	holder := ReservationHolder{UID: "new-owner-uid", Kind: "ChainNode", Namespace: "default", Name: "validator", Claim: "validator"}
	stale := reservationLifecycleObject(ConsensusKeyReservationName("chain-1", reservationTestPublicKey), "ckr-stale", ReservationHolder{
		UID: "old-owner-uid", Kind: "ChainNode", Namespace: holder.Namespace, Name: holder.Name, Claim: holder.Claim,
	})
	currentOwner := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{
		Name: holder.Name, Namespace: holder.Namespace, UID: holder.UID, Finalizers: []string{ReservationOwnerFinalizer},
	}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "validator-tmkms-vault-upload", Namespace: holder.Namespace, UID: "pod-uid"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		reservationLifecycleNamespace(holder.Namespace), currentOwner, stale, pod,
	).Build()

	_, err := EnsureConsensusKeyReservationWithResult(context.Background(), c, c, "chain-1", reservationTestPublicKey, holder)
	if !errors.Is(err, ErrConsensusKeyReservationRecoveryBlocked) || !strings.Contains(err.Error(), pod.Name) {
		t.Fatalf("exact managed one-shot Pod must block stale recovery, got %v", err)
	}
}

func TestEnsureConsensusKeyReservationBlocksRecoveryForLabelLessSignerReplica(t *testing.T) {
	scheme := reservationLifecycleScheme(t)
	holder := ReservationHolder{UID: "new-owner-uid", Kind: "ChainNode", Namespace: "default", Name: "validator", Claim: "validator"}
	stale := reservationLifecycleObject(ConsensusKeyReservationName("chain-1", reservationTestPublicKey), "ckr-stale", ReservationHolder{
		UID: "old-owner-uid", Kind: "ChainNode", Namespace: holder.Namespace, Name: holder.Name, Claim: holder.Claim,
	})
	currentOwner := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{
		Name: holder.Name, Namespace: holder.Namespace, UID: holder.UID, Finalizers: []string{ReservationOwnerFinalizer},
	}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "validator-signer-0", Namespace: holder.Namespace, UID: "pod-uid"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		reservationLifecycleNamespace(holder.Namespace), currentOwner, stale, pod,
	).Build()

	_, err := EnsureConsensusKeyReservationWithResult(context.Background(), c, c, "chain-1", reservationTestPublicKey, holder)
	if !errors.Is(err, ErrConsensusKeyReservationRecoveryBlocked) || !strings.Contains(err.Error(), pod.Name) {
		t.Fatalf("label-less stale signer replica must block recovery, got %v", err)
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

func TestCleanupManagedSigningPathDeletesJobBeforeWaitingForPod(t *testing.T) {
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
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(job), &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Fatalf("Job deletion must be requested before waiting for its pod, got %v", err)
	}
	freshPod := &corev1.Pod{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(pod), freshPod); err != nil {
		t.Fatal(err)
	}
	if freshPod.DeletionTimestamp != nil {
		t.Fatal("Job pod must be left for foreground Job deletion")
	}
	freshPod.Finalizers = nil
	if err := c.Update(context.Background(), freshPod); err != nil {
		t.Fatal(err)
	}
	if err := c.Delete(context.Background(), freshPod); err != nil && !apierrors.IsNotFound(err) {
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

func reservationLifecycleNamespace(name string) *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
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

type namespaceNotFoundListReader struct {
	client.Reader
	namespace       string
	namespacedLists int
}

func (r *namespaceNotFoundListReader) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	options := &client.ListOptions{}
	options.ApplyOptions(opts)
	if options.Namespace == r.namespace {
		r.namespacedLists++
		return apierrors.NewNotFound(schema.GroupResource{Resource: "namespaces"}, r.namespace)
	}
	return r.Reader.List(ctx, list, opts...)
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
