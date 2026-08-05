package chainnodeset

import (
	"context"
	"testing"
	"time"

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
