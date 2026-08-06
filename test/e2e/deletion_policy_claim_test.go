package e2e

import (
	"testing"

	. "github.com/onsi/gomega"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
)

// These are plain Go tests rather than Ginkgo specs: the decision they cover is what the reservation
// cleanup is allowed to conclude from one read, and every input it can be handed is constructible
// here. The suite's specs cannot reach most of them — a live child under a terminating parent is a
// window the controller closes on its own clock — so the branch that keeps this fallback from dropping
// a reservation out from under a live signer would otherwise go unexercised until it regressed.
//
// TestE2E skips without E2E_TEST, so these run against no cluster; nothing below touches Framework().
// Gomega is used through NewWithT because the suite's fail handler is only registered on the path that
// skipped.

const (
	claimTestNamespace = "e2e-claim"
	claimTestParentUID = types.UID("11111111-1111-1111-1111-111111111111")
	claimTestChildUID  = types.UID("22222222-2222-2222-2222-222222222222")
	claimTestOtherUID  = types.UID("33333333-3333-3333-3333-333333333333")
)

func setOwnedClaimRecord() reservationRecord {
	return reservationRecord{
		Name:      "reservation-abc",
		UID:       types.UID("44444444-4444-4444-4444-444444444444"),
		OwnerUID:  claimTestParentUID,
		OwnerKind: "ChainNodeSet",
		Namespace: claimTestNamespace,
		OwnerName: "nodeset",
		Claim:     "nodeset-validator",
	}
}

// newClaimChild builds the ChainNode a claim read would return. A controllerUID of "" leaves the child
// with no controller reference at all, which is how an orphan or an adopted replacement presents.
func newClaimChild(uid, controllerUID types.UID, terminating bool) *appsv1.ChainNode {
	child := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{
		Namespace: claimTestNamespace,
		Name:      "nodeset-validator",
		UID:       uid,
	}}
	if controllerUID != "" {
		child.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: appsv1.GroupVersion.String(),
			Kind:       "ChainNodeSet",
			Name:       "nodeset",
			UID:        controllerUID,
			Controller: ptr.To(true),
		}}
	}
	if terminating {
		child.DeletionTimestamp = ptr.To(metav1.Now())
		child.Finalizers = []string{"cosmopilot.voluzi.com/test"}
	}
	return child
}

func TestClaimChildSigningPodOwners(t *testing.T) {
	notFound := apierrors.NewNotFound(
		schema.GroupResource{Group: appsv1.GroupVersion.Group, Resource: "chainnodes"}, "nodeset-validator",
	)
	unreadable := apierrors.NewServiceUnavailable("the apiserver is shutting down")

	tests := []struct {
		name       string
		record     reservationRecord
		child      *appsv1.ChainNode
		err        error
		wantOwners []signingPodOwner
		wantSettle bool
	}{
		{
			name:       "an absent child settles the claim with nothing left to wait out",
			record:     setOwnedClaimRecord(),
			child:      &appsv1.ChainNode{},
			err:        notFound,
			wantSettle: true,
		},
		{
			name:   "a terminating child of the recorded parent settles it and scopes the surface",
			record: setOwnedClaimRecord(),
			child:  newClaimChild(claimTestChildUID, claimTestParentUID, true),
			wantOwners: []signingPodOwner{
				{chainNode: "nodeset-validator", uid: claimTestChildUID},
			},
			wantSettle: true,
		},
		{
			// The regression this gate exists for: the parent is already Terminating when the fallback
			// runs, so an empty surface here is a gap between two pods, not the end of the signing path.
			name:   "a live child of the recorded parent settles nothing",
			record: setOwnedClaimRecord(),
			child:  newClaimChild(claimTestChildUID, claimTestParentUID, false),
		},
		{
			name:   "a terminating child of another parent is not this record's to release",
			record: setOwnedClaimRecord(),
			child:  newClaimChild(claimTestChildUID, claimTestOtherUID, true),
		},
		{
			name:   "a child with no controller cannot be attributed",
			record: setOwnedClaimRecord(),
			child:  newClaimChild(claimTestChildUID, "", true),
		},
		{
			name:   "a non-controller owner reference does not attribute the child",
			record: setOwnedClaimRecord(),
			child: func() *appsv1.ChainNode {
				child := newClaimChild(claimTestChildUID, claimTestParentUID, true)
				child.OwnerReferences[0].Controller = ptr.To(false)
				return child
			}(),
		},
		{
			name:   "a transient read error stays retryable",
			record: setOwnedClaimRecord(),
			child:  &appsv1.ChainNode{},
			err:    unreadable,
		},
		{
			name:   "a child with no UID settles nothing",
			record: setOwnedClaimRecord(),
			child:  newClaimChild("", claimTestParentUID, true),
		},
		{
			// Standalone claims that name something other than the owner are cosmosigner claims, which
			// name no ChainNode; a read that did resolve one keeps the pre-existing widening.
			name: "a standalone owner's claim keeps its surface widening",
			record: reservationRecord{
				OwnerUID:  claimTestOtherUID,
				OwnerKind: "ChainNode",
				Namespace: claimTestNamespace,
				OwnerName: "validator",
				Claim:     "nodeset-validator",
			},
			child: newClaimChild(claimTestChildUID, "", false),
			wantOwners: []signingPodOwner{
				{chainNode: "nodeset-validator", uid: claimTestChildUID},
			},
			wantSettle: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			g := NewWithT(t)
			owners, settled := claimChildSigningPodOwners(test.record, test.child, test.err)
			g.Expect(settled).To(Equal(test.wantSettle))
			g.Expect(owners).To(Equal(test.wantOwners))
		})
	}
}

// TestClaimSigningPodOwnersStandaloneNeedsNoRead pins the standalone shape's short circuit. It runs
// with no cluster behind it on purpose: the record already carries the exact owner UID, so resolving
// the claim must not reach for the API at all — and a regression that made it read would fail here
// rather than silently pick up a same-named replacement in a real run.
func TestClaimSigningPodOwnersStandaloneNeedsNoRead(t *testing.T) {
	g := NewWithT(t)
	record := reservationRecord{
		OwnerUID:  claimTestParentUID,
		OwnerKind: "ChainNode",
		Namespace: claimTestNamespace,
		OwnerName: "validator",
		Claim:     "validator",
	}
	owners, settled := claimSigningPodOwners(record)
	g.Expect(settled).To(BeTrue())
	g.Expect(owners).To(Equal([]signingPodOwner{{chainNode: "validator", uid: claimTestParentUID}}))
}
