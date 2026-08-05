package chainnodeset

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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/cosmosigner"
)

func TestPrepareConsensusKeyReservationOwnerFinalizesChainNodeSet(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "nodes", Namespace: "default", UID: "nodes-uid"},
		Spec:       appsv1.ChainNodeSetSpec{Validator: &appsv1.NodeSetValidatorConfig{}},
	}
	r := newValidatorTestReconciler(t, nodeSet)
	r.APIReader = r.Client

	changed, err := r.prepareConsensusKeyReservationOwner(context.Background(), nodeSet)
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatal("the finalizer must be persisted before ChainNodeSet signing paths are created")
	}
	fresh := &appsv1.ChainNodeSet{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(nodeSet), fresh); err != nil {
		t.Fatal(err)
	}
	if !controllerutil.ContainsFinalizer(fresh, cosmosigner.ReservationOwnerFinalizer) {
		t.Fatalf("finalizers = %v", fresh.Finalizers)
	}
}

func TestFinalizeConsensusKeyReservationOwnerWaitsForOwnedChild(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{
		Name: "nodes", Namespace: "default", UID: "nodes-uid",
		Finalizers: []string{cosmosigner.ReservationOwnerFinalizer},
	}}
	reservation := &appsv1.ConsensusKeyReservation{
		ObjectMeta: metav1.ObjectMeta{
			Name: cosmosigner.ConsensusKeyReservationName("chain-1", nodeSetReservationLifecyclePublicKey), UID: "ckr-uid",
		},
		Spec: appsv1.ConsensusKeyReservationSpec{
			ChainID: "chain-1", PublicKey: nodeSetReservationLifecyclePublicKey,
			OwnerUID: nodeSet.UID, OwnerKind: "ChainNodeSet", Namespace: nodeSet.Namespace,
			OwnerName: nodeSet.Name, Claim: "nodes-validator",
		},
	}
	child := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{
		Name: "nodes-validator", Namespace: nodeSet.Namespace, UID: "child-uid",
		Finalizers: []string{"test.voluzi.com/hold"},
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNodeSet", Name: nodeSet.Name,
			UID: nodeSet.UID, Controller: ptr.To(true),
		}},
	}}
	foreign := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{
		Name: "foreign", Namespace: nodeSet.Namespace, UID: "foreign-child-uid",
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNodeSet", Name: "foreign",
			UID: "foreign-owner-uid", Controller: ptr.To(true),
		}},
	}}
	key := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "validator-key", Namespace: nodeSet.Namespace}}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "nodes-validator", Namespace: nodeSet.Namespace}}
	r := newValidatorTestReconciler(t, nodeSet, reservation, child, foreign, key, pvc)
	r.APIReader = r.Client
	r.recorder = record.NewFakeRecorder(10)

	done, err := r.finalizeConsensusKeyReservationOwner(context.Background(), nodeSet)
	if err != nil {
		t.Fatal(err)
	}
	if done {
		t.Fatal("a terminating owned child must keep reservation finalization pending")
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(reservation), &appsv1.ConsensusKeyReservation{}); err != nil {
		t.Fatalf("reservation released before child absence: %v", err)
	}
	freshChild := &appsv1.ChainNode{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(child), freshChild); err != nil {
		t.Fatal(err)
	}
	if freshChild.DeletionTimestamp == nil {
		t.Fatal("owned child deletion was not requested")
	}
	freshChild.Finalizers = nil
	if err := r.Update(context.Background(), freshChild); err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}

	done, err = r.finalizeConsensusKeyReservationOwner(context.Background(), nodeSet)
	if err != nil {
		t.Fatal(err)
	}
	if !done {
		t.Fatal("reservation lifecycle must complete after every owned child is absent")
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(reservation), &appsv1.ConsensusKeyReservation{}); !apierrors.IsNotFound(err) {
		t.Fatalf("reservation still exists after verified teardown: %v", err)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(foreign), &appsv1.ChainNode{}); err != nil {
		t.Fatalf("foreign child was modified: %v", err)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(key), &corev1.Secret{}); err != nil {
		t.Fatalf("signing key retention changed: %v", err)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(pvc), &corev1.PersistentVolumeClaim{}); err != nil {
		t.Fatalf("data PVC retention changed: %v", err)
	}
}

func TestFinalizeConsensusKeyReservationOwnerWithoutNewFinalizerReleasesUpgradeReservation(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{Name: "nodes", Namespace: "default", UID: "nodes-uid"}}
	reservation := &appsv1.ConsensusKeyReservation{
		ObjectMeta: metav1.ObjectMeta{Name: cosmosigner.ConsensusKeyReservationName("chain-1", nodeSetReservationLifecyclePublicKey), UID: "ckr-uid"},
		Spec: appsv1.ConsensusKeyReservationSpec{
			ChainID: "chain-1", PublicKey: nodeSetReservationLifecyclePublicKey,
			OwnerUID: nodeSet.UID, OwnerKind: "ChainNodeSet", Namespace: nodeSet.Namespace,
			OwnerName: nodeSet.Name, Claim: "nodes-validator",
		},
	}
	r := newValidatorTestReconciler(t, nodeSet, reservation)
	r.APIReader = r.Client

	done, err := r.finalizeConsensusKeyReservationOwner(context.Background(), nodeSet)
	if err != nil || !done {
		t.Fatalf("upgrade reservation cleanup failed: done=%v err=%v", done, err)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(reservation), &appsv1.ConsensusKeyReservation{}); !apierrors.IsNotFound(err) {
		t.Fatalf("exact-owner upgrade reservation still exists: %v", err)
	}
}

func TestFinalizeReservationOwnerChildrenBlocksStatusRecordedForeignIdentity(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "nodes", Namespace: "default", UID: "nodes-uid"},
		Status:     appsv1.ChainNodeSetStatus{Nodes: []appsv1.ChainNodeSetNodeStatus{{Name: "nodes-validator"}}},
	}
	child := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: "nodes-validator", Namespace: nodeSet.Namespace, UID: "child-uid"}}
	r := newValidatorTestReconciler(t, nodeSet, child)
	r.APIReader = r.Client

	done, err := r.finalizeReservationOwnerChildren(context.Background(), nodeSet)
	if err == nil || done {
		t.Fatalf("status-recorded child without exact UID ownership must fail closed, done=%v err=%v", done, err)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(child), &appsv1.ChainNode{}); err != nil {
		t.Fatalf("ambiguous child must remain untouched: %v", err)
	}
}

func TestFinalizeReservationOwnerChildrenBlocksClaimNamedForeignIdentity(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{Name: "nodes", Namespace: "default", UID: "nodes-uid"}}
	reservation := nodeSetReservation(nodeSet, "reserved", "ckr-uid", nodeSetReservationLifecyclePublicKey, "nodes-validator")
	child := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: reservation.Spec.Claim, Namespace: nodeSet.Namespace, UID: "child-uid"}}
	r := newValidatorTestReconciler(t, nodeSet, reservation, child)
	r.APIReader = r.Client

	done, err := r.finalizeReservationOwnerChildren(context.Background(), nodeSet)
	if err == nil || done || !strings.Contains(err.Error(), child.Name) {
		t.Fatalf("reservation claim child without exact UID ownership must fail closed, done=%v err=%v", done, err)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(child), &appsv1.ChainNode{}); err != nil {
		t.Fatalf("ambiguous claim child must remain untouched: %v", err)
	}
}

func TestReservationOwnerPodsGoneBlocksKnownClaimWithoutLabels(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{Name: "nodes", Namespace: "default", UID: "nodes-uid"}}
	reservation := nodeSetReservation(nodeSet, "reserved", "ckr-uid", nodeSetReservationLifecyclePublicKey, "nodes-validator")
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: reservation.Spec.Claim, Namespace: nodeSet.Namespace, UID: "pod-uid"}}
	r := newValidatorTestReconciler(t, nodeSet, reservation, pod)
	r.APIReader = r.Client

	done, err := r.reservationOwnerPodsGone(context.Background(), nodeSet)
	if err != nil {
		t.Fatal(err)
	}
	if done {
		t.Fatal("label-less Pod matching an exact reservation claim must block release")
	}
}

func TestReconcileConsensusKeyReservationClaimsReleasesOnlyUndesiredClaim(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "nodes", Namespace: "default", UID: "nodes-uid",
			Finalizers: []string{cosmosigner.ReservationOwnerFinalizer},
		},
		Spec: appsv1.ChainNodeSetSpec{Validator: &appsv1.NodeSetValidatorConfig{}},
	}
	desiredClaim := validatorNodeName(nodeSet, appsv1.ReservedValidatorGroupName, 0)
	desired := nodeSetReservation(nodeSet, "desired", "desired-uid", nodeSetReservationLifecyclePublicKey, desiredClaim)
	stale := nodeSetReservation(nodeSet, "stale", "stale-uid", nodeSetReservationLifecycleOtherPublicKey, "nodes-old-validator")
	child := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{
		Name: desiredClaim, Namespace: nodeSet.Namespace, UID: "desired-child-uid",
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNodeSet", Name: nodeSet.Name,
			UID: nodeSet.UID, Controller: ptr.To(true),
		}},
	}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: desiredClaim, Namespace: nodeSet.Namespace, UID: "desired-pod-uid",
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNode", Name: child.Name,
			UID: child.UID, Controller: ptr.To(true),
		}},
	}}
	r := newValidatorTestReconciler(t, nodeSet, desired, stale, child, pod)
	r.APIReader = r.Client

	done, err := r.reconcileConsensusKeyReservationClaims(context.Background(), nodeSet)
	if err != nil {
		t.Fatal(err)
	}
	if !done {
		t.Fatal("claim reconciliation must finish when every undesired path is absent")
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(stale), &appsv1.ConsensusKeyReservation{}); !apierrors.IsNotFound(err) {
		t.Fatalf("undesired claim reservation still exists: %v", err)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(desired), &appsv1.ConsensusKeyReservation{}); err != nil {
		t.Fatalf("desired claim reservation was disrupted: %v", err)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(child), &appsv1.ChainNode{}); err != nil {
		t.Fatalf("desired claim child was disrupted: %v", err)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(pod), &corev1.Pod{}); err != nil {
		t.Fatalf("desired claim pod was disrupted: %v", err)
	}
}

func TestReconcileConsensusKeyReservationClaimsBlocksGeneratedOneShotPod(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{
		Name: "nodes", Namespace: "default", UID: "nodes-uid",
		Finalizers: []string{cosmosigner.ReservationOwnerFinalizer},
	}}
	claim := "nodes-old-validator"
	stale := nodeSetReservation(nodeSet, "stale", "stale-uid", nodeSetReservationLifecycleOtherPublicKey, claim)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: claim + "-tmkms-vault-upload-generated", Namespace: nodeSet.Namespace, UID: "pod-uid",
	}}
	r := newValidatorTestReconciler(t, nodeSet, stale, pod)
	r.APIReader = r.Client

	done, err := r.reconcileConsensusKeyReservationClaims(context.Background(), nodeSet)
	if err == nil || done {
		t.Fatalf("generated one-shot pod must block claim release, done=%v err=%v", done, err)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(stale), &appsv1.ConsensusKeyReservation{}); err != nil {
		t.Fatalf("reservation must remain while generated one-shot pod exists: %v", err)
	}
}

func TestReconcileConsensusKeyReservationClaimsBlocksLabelLessOwnedSignerAfterStatusLoss(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{
		Name: "nodes", Namespace: "default", UID: "nodes-uid",
		Finalizers: []string{cosmosigner.ReservationOwnerFinalizer},
	}}
	reservation := nodeSetReservation(nodeSet, "retired", "ckr-uid", nodeSetReservationLifecyclePublicKey, "signer-retired-hash")
	sts := &appsk8sv1.StatefulSet{ObjectMeta: metav1.ObjectMeta{
		Name: "nodes-retired-signer", Namespace: nodeSet.Namespace, UID: "signer-uid",
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNodeSet", Name: nodeSet.Name,
			UID: nodeSet.UID, Controller: ptr.To(true),
		}},
	}}
	r := newValidatorTestReconciler(t, nodeSet, reservation, sts)
	r.APIReader = r.Client

	done, err := r.reconcileConsensusKeyReservationClaims(context.Background(), nodeSet)
	if err == nil || done || !strings.Contains(err.Error(), sts.Name) {
		t.Fatalf("label-less owned signer must block claim release after status loss, done=%v err=%v", done, err)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(reservation), &appsv1.ConsensusKeyReservation{}); err != nil {
		t.Fatalf("reservation must remain while the label-less owned signer exists: %v", err)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(sts), &appsk8sv1.StatefulSet{}); err != nil {
		t.Fatalf("ambiguous signer StatefulSet must remain untouched: %v", err)
	}
}

func TestRemovedCosmosignerStatusRetainedUntilResourceOneShotsAreGone(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{
		Name: "nodes", Namespace: "default", UID: "nodes-uid",
		Finalizers: []string{cosmosigner.ReservationOwnerFinalizer},
	}, Status: appsv1.ChainNodeSetStatus{Cosmosigners: []appsv1.CosmosignerStatus{{
		Name: "nodes-signer", ResourceName: "nodes-retired-signer", PublicKey: nodeSetReservationLifecyclePublicKey,
	}}}}
	reservation := nodeSetReservation(nodeSet, "retired", "ckr-uid", nodeSetReservationLifecyclePublicKey, "nodes-validator")
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: "nodes-retired-signer-import", Namespace: nodeSet.Namespace, UID: "job-uid",
	}}
	r := newValidatorTestReconciler(t, nodeSet, reservation, job)
	r.APIReader = r.Client

	done, err := r.reconcileSignerTeardown(context.Background(), nodeSet)
	if err != nil || !done {
		t.Fatalf("absent retired signer should finish workload teardown, done=%v err=%v", done, err)
	}
	fresh := &appsv1.ChainNodeSet{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(nodeSet), fresh); err != nil {
		t.Fatal(err)
	}
	if fresh.GetCosmosignerStatus("nodes-signer") == nil {
		t.Fatal("retired signer status must remain while its reservation still needs the resource name")
	}

	done, err = r.reconcileConsensusKeyReservationClaims(context.Background(), fresh)
	if err == nil || done {
		t.Fatalf("foreign retired-resource one-shot Job must block claim release, done=%v err=%v", done, err)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(reservation), &appsv1.ConsensusKeyReservation{}); err != nil {
		t.Fatalf("reservation must remain while retired-resource one-shot Job exists: %v", err)
	}
}

func TestRemovedCosmosignerStatusWithoutPublicKeyRetainedForExactOwnerRetirement(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{
		Name: "nodes", Namespace: "default", UID: "nodes-uid",
		Finalizers: []string{cosmosigner.ReservationOwnerFinalizer},
	}, Status: appsv1.ChainNodeSetStatus{Cosmosigners: []appsv1.CosmosignerStatus{{
		Name: "retired", ResourceName: "nodes-retired-signer",
	}}}}
	reservation := nodeSetReservation(nodeSet, "retired", "ckr-uid", nodeSetReservationLifecyclePublicKey, "signer-retired-hash")
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: "nodes-retired-signer-import", Namespace: nodeSet.Namespace, UID: "job-uid",
	}}
	r := newValidatorTestReconciler(t, nodeSet, reservation, job)
	r.APIReader = r.Client

	done, err := r.reconcileSignerTeardown(context.Background(), nodeSet)
	if err != nil || !done {
		t.Fatalf("absent retired signer should finish workload teardown, done=%v err=%v", done, err)
	}
	fresh := &appsv1.ChainNodeSet{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(nodeSet), fresh); err != nil {
		t.Fatal(err)
	}
	if fresh.GetCosmosignerStatus("retired") == nil {
		t.Fatal("incomplete retired signer status must remain while an exact-owner reservation is retiring")
	}

	done, err = r.reconcileConsensusKeyReservationClaims(context.Background(), fresh)
	if err == nil || done || !strings.Contains(err.Error(), job.Name) {
		t.Fatalf("resource-named one-shot Job must block claim release, done=%v err=%v", done, err)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(reservation), &appsv1.ConsensusKeyReservation{}); err != nil {
		t.Fatalf("reservation must remain while retired-resource one-shot Job exists: %v", err)
	}
}

func TestReconcileConsensusKeyReservationClaimsBlocksDeterministicSignerOneShotsAfterStatusLoss(t *testing.T) {
	tests := []struct {
		name string
		path func(*appsv1.ChainNodeSet) client.Object
	}{
		{
			name: "exact-owner Job",
			path: func(nodeSet *appsv1.ChainNodeSet) client.Object {
				return &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
					Name: "nodes-sentry-signer-import", Namespace: nodeSet.Namespace, UID: "job-uid",
					Labels: map[string]string{"nodeset": "foreign"},
					OwnerReferences: []metav1.OwnerReference{{
						APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNodeSet", Name: nodeSet.Name,
						UID: nodeSet.UID, Controller: ptr.To(true),
					}},
				}}
			},
		},
		{
			name: "exact-labelled direct Pod",
			path: func(nodeSet *appsv1.ChainNodeSet) client.Object {
				return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
					Name: "nodes-sentry-signer-pubkey", Namespace: nodeSet.Namespace, UID: "pod-uid",
					Labels: map[string]string{"nodeset": nodeSet.Name},
				}}
			},
		},
		{
			name: "exact-labelled generated Job Pod",
			path: func(nodeSet *appsv1.ChainNodeSet) client.Object {
				return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
					Name: "nodes-sentry-signer-import-generated", Namespace: nodeSet.Namespace, UID: "pod-uid",
					Labels: map[string]string{"nodeset": nodeSet.Name},
				}}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeSet := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{
				Name: "nodes", Namespace: "default", UID: "nodes-uid",
				Finalizers: []string{cosmosigner.ReservationOwnerFinalizer},
			}}
			reservation := nodeSetReservation(nodeSet, "retired", "ckr-uid", nodeSetReservationLifecyclePublicKey, "signer-retired-hash")
			path := tt.path(nodeSet)
			r := newValidatorTestReconciler(t, nodeSet, reservation, path)
			r.APIReader = r.Client

			done, err := r.reconcileConsensusKeyReservationClaims(context.Background(), nodeSet)
			if err == nil || done || !strings.Contains(err.Error(), path.GetName()) {
				t.Fatalf("deterministic retired signer one-shot must block claim release, done=%v err=%v", done, err)
			}
			if err := r.Get(context.Background(), client.ObjectKeyFromObject(reservation), &appsv1.ConsensusKeyReservation{}); err != nil {
				t.Fatalf("reservation must remain while %q exists: %v", path.GetName(), err)
			}
		})
	}
}

func TestReconcileConsensusKeyReservationClaimsUsesExactOneShotLabelsBeforeNodeSetPrefixFallback(t *testing.T) {
	tests := []struct {
		name string
		path client.Object
	}{
		{
			name: "Job",
			path: &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
				Name: "foo-bar-sentry-signer-import", Namespace: "default", UID: "job-uid",
				Labels: map[string]string{"nodeset": "foo-bar"},
			}},
		},
		{
			name: "direct Pod",
			path: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Name: "foo-bar-sentry-signer-pubkey", Namespace: "default", UID: "pod-uid",
				Labels: map[string]string{"chain-node": "foo-bar-validator"},
			}},
		},
		{
			name: "generated Job Pod",
			path: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Name: "foo-bar-sentry-signer-import-generated", Namespace: "default", UID: "pod-uid",
				Labels: map[string]string{"nodeset": "foo-bar"},
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeSet := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{
				Name: "foo", Namespace: "default", UID: "nodes-uid",
				Finalizers: []string{cosmosigner.ReservationOwnerFinalizer},
			}}
			reservation := nodeSetReservation(nodeSet, "retired", "ckr-uid", nodeSetReservationLifecyclePublicKey, "signer-retired-hash")
			r := newValidatorTestReconciler(t, nodeSet, reservation, tt.path)
			r.APIReader = r.Client

			done, err := r.reconcileConsensusKeyReservationClaims(context.Background(), nodeSet)
			if err != nil || !done {
				t.Fatalf("foreign exact labels must override the node-set prefix fallback, done=%v err=%v", done, err)
			}
			if err := r.Get(context.Background(), client.ObjectKeyFromObject(tt.path), tt.path); err != nil {
				t.Fatalf("foreign-attributed one-shot was modified: %v", err)
			}
		})
	}
}

func TestReconcileConsensusKeyReservationClaimsBlocksSignerReplicaAfterStatusLoss(t *testing.T) {
	tests := []struct {
		name        string
		pod         *corev1.Pod
		wantBlocked bool
	}{
		{
			name: "label-less owned-name replica",
			pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Name: "foo-sentry-signer-0", Namespace: "default", UID: "pod-uid",
			}},
			wantBlocked: true,
		},
		{
			name: "prefix-related exact foreign replica",
			pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Name: "foo-bar-sentry-signer-0", Namespace: "default", UID: "pod-uid",
				Labels: map[string]string{"nodeset": "foo-bar"},
			}},
		},
		{
			name: "noncanonical ordinal",
			pod: &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Name: "foo-sentry-signer-00", Namespace: "default", UID: "pod-uid",
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeSet := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{
				Name: "foo", Namespace: "default", UID: "nodes-uid",
				Finalizers: []string{cosmosigner.ReservationOwnerFinalizer},
			}}
			reservation := nodeSetReservation(nodeSet, "retired", "ckr-uid", nodeSetReservationLifecyclePublicKey, "signer-retired-hash")
			r := newValidatorTestReconciler(t, nodeSet, reservation, tt.pod)
			r.APIReader = r.Client

			done, err := r.reconcileConsensusKeyReservationClaims(context.Background(), nodeSet)
			if tt.wantBlocked {
				if err == nil || done || !strings.Contains(err.Error(), tt.pod.Name) {
					t.Fatalf("label-less signer replica must block claim release, done=%v err=%v", done, err)
				}
				if err := r.Get(context.Background(), client.ObjectKeyFromObject(reservation), &appsv1.ConsensusKeyReservation{}); err != nil {
					t.Fatalf("reservation must remain while the signer replica exists: %v", err)
				}
				return
			}
			if err != nil || !done {
				t.Fatalf("foreign exact labels must override replica prefix fallback, done=%v err=%v", done, err)
			}
		})
	}
}

func TestRemovedCosmosignerStatusDroppedWhenReservationClaimStaysDesired(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "nodes", Namespace: "default", UID: "nodes-uid"},
		Spec:       appsv1.ChainNodeSetSpec{Validator: &appsv1.NodeSetValidatorConfig{}},
		Status: appsv1.ChainNodeSetStatus{Cosmosigners: []appsv1.CosmosignerStatus{{
			Name: "nodes-signer", ResourceName: "nodes-retired-signer", PublicKey: nodeSetReservationLifecyclePublicKey,
		}}},
	}
	claim := validatorNodeName(nodeSet, appsv1.ReservedValidatorGroupName, 0)
	reservation := nodeSetReservation(nodeSet, "desired", "ckr-uid", nodeSetReservationLifecyclePublicKey, claim)
	r := newValidatorTestReconciler(t, nodeSet, reservation)
	r.APIReader = r.Client

	done, err := r.reconcileSignerTeardown(context.Background(), nodeSet)
	if err != nil || !done {
		t.Fatalf("absent retired signer should finish workload teardown, done=%v err=%v", done, err)
	}
	fresh := &appsv1.ChainNodeSet{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(nodeSet), fresh); err != nil {
		t.Fatal(err)
	}
	if fresh.GetCosmosignerStatus("nodes-signer") != nil {
		t.Fatal("retired signer status must be dropped when its reservation claim remains desired")
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(reservation), &appsv1.ConsensusKeyReservation{}); err != nil {
		t.Fatalf("desired reservation must remain: %v", err)
	}
}

func TestReconcileTerminatingNamespaceRunsReservationFinalizer(t *testing.T) {
	now := metav1.NewTime(time.Now())
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "terminating", DeletionTimestamp: &now, Finalizers: []string{"kubernetes"},
	}}
	nodeSet := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{
		Name: "nodes", Namespace: ns.Name, UID: "nodes-uid",
		Finalizers: []string{cosmosigner.ReservationOwnerFinalizer},
	}}
	reservation := nodeSetReservation(nodeSet, "reserved", "ckr-uid", nodeSetReservationLifecyclePublicKey, "nodes-validator")
	r := newValidatorTestReconciler(t, ns, nodeSet, reservation)
	r.APIReader = r.Client

	_, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{
		Namespace: nodeSet.Namespace, Name: nodeSet.Name,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(reservation), &appsv1.ConsensusKeyReservation{}); !apierrors.IsNotFound(err) {
		t.Fatalf("terminating namespace stranded reservation: %v", err)
	}
	fresh := &appsv1.ChainNodeSet{}
	if err := r.Get(context.Background(), client.ObjectKeyFromObject(nodeSet), fresh); err != nil {
		t.Fatal(err)
	}
	if controllerutil.ContainsFinalizer(fresh, cosmosigner.ReservationOwnerFinalizer) {
		t.Fatal("reservation lifecycle finalizer was not completed in terminating namespace")
	}
}

func nodeSetReservation(nodeSet *appsv1.ChainNodeSet, name, uid, publicKey, claim string) *appsv1.ConsensusKeyReservation {
	return &appsv1.ConsensusKeyReservation{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID(uid)},
		Spec: appsv1.ConsensusKeyReservationSpec{
			ChainID: "chain-1", PublicKey: publicKey, OwnerUID: nodeSet.UID,
			OwnerKind: "ChainNodeSet", Namespace: nodeSet.Namespace, OwnerName: nodeSet.Name, Claim: claim,
		},
	}
}

const nodeSetReservationLifecyclePublicKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
const nodeSetReservationLifecycleOtherPublicKey = "AQAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
