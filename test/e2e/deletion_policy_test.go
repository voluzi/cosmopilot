package e2e

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	managedcosmosigner "github.com/voluzi/cosmopilot/v2/internal/cosmosigner"
	"github.com/voluzi/cosmopilot/v2/internal/resourcecleanup"
	"github.com/voluzi/cosmopilot/v2/test/e2e/apps"
)

var _ = Describe("Deletion policy", func() {
	It("deletes only attributed node data and generated keys after the workload stops", WithNs(func(ns *corev1.Namespace) {
		app := apps.Nibiru()
		node := app.BuildChainNode(ns.Name)
		node.Spec.DeletionPolicy = &appsv1.DeletionPolicy{
			DataVolumes:      ptr.To(appsv1.DeletionPolicyDelete),
			GeneratedKeys:    ptr.To(appsv1.DeletionPolicyDelete),
			CosmosignerState: ptr.To(appsv1.DeletionPolicyDelete),
		}
		Expect(Framework().Client().Create(Framework().Context(), node)).To(Succeed())
		WaitForChainNodeHeight(node, 1)
		RefreshChainNode(node)

		Eventually(func() int {
			return attributedPVCCount(ns.Name, node.UID)
		}).Should(BeNumerically(">", 0))
		Eventually(func() int {
			return attributedSecretCount(ns.Name, node.UID)
		}).Should(BeNumerically(">", 0))
		record := recordConsensusKeyReservation("ChainNode", ns.Name, node.Name, node.UID)
		deferReservationCleanup(record)

		Expect(Framework().Client().Delete(Framework().Context(), node)).To(Succeed())
		Eventually(func() bool {
			err := Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(node), &appsv1.ChainNode{})
			return apierrors.IsNotFound(err)
		}).Should(BeTrue())
		Eventually(func() bool {
			err := Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(node), &corev1.Pod{})
			return apierrors.IsNotFound(err)
		}).Should(BeTrue())
		Eventually(func() int {
			return attributedPVCCount(ns.Name, node.UID)
		}).Should(Equal(0))
		Eventually(func() int {
			return attributedSecretCount(ns.Name, node.UID)
		}).Should(Equal(0))
		assertReservationAbsent(record)
	}))

	It("releases a standalone ChainNode reservation only after signing teardown and reacquires it on recreate", WithNs(func(ns *corev1.Namespace) {
		app := apps.Nibiru()
		node := app.BuildChainNode(ns.Name)
		// Pin the name instead of using GenerateName so the recreate below can reuse the exact
		// namespace/name pair the released reservation was claimed for.
		node.Name = fmt.Sprintf("e2e-deletion-policy-%s", RandString(6))
		node.GenerateName = ""
		// Drop the data volumes so the recreated node re-runs genesis init from scratch, but leave
		// generated keys on the Retain default: the recreate then reuses the same consensus key and
		// must reacquire the very same reservation name under a new owner UID.
		node.Spec.DeletionPolicy = &appsv1.DeletionPolicy{
			DataVolumes: ptr.To(appsv1.DeletionPolicyDelete),
		}
		Expect(Framework().Client().Create(Framework().Context(), node)).To(Succeed())
		WaitForChainNodeHeight(node, 1)
		RefreshChainNode(node)

		record := recordConsensusKeyReservation("ChainNode", ns.Name, node.Name, node.UID)
		deferReservationCleanup(record)
		Expect(record.Claim).To(Equal(node.Name), "a standalone validator claims the reservation under its own name")

		surface := signingSurface{
			namespace: ns.Name,
			claimPods: []string{node.Name},
		}
		assertSigningSurfaceServing(surface)

		podUID := signingPodUID(ns.Name, node.Name)
		// Pin the validator pod in Terminating before deleting the owner: quiesceNodePod refuses to
		// report the node quiesced while the pod object exists at all, so the reservation release is
		// blocked behind a workload the test controls instead of behind a lucky sampling window.
		setPodTestFinalizer(ns.Name, node.Name, true)
		releaseHold := onceReleaseHold(ns.Name, node.Name)
		DeferCleanup(releaseHold)

		Expect(Framework().Client().Delete(Framework().Context(), node)).To(Succeed())
		waitForPodTerminating(ns.Name, node.Name, podUID)
		assertReservationHeldDuringSigningTeardown(surface, record, reservationHeldWindow)

		// Arm the release watch before lifting the hold, never after: with the pod still pinned the
		// controller cannot have released anything yet, so the stream starts from a revision at which
		// this exact reservation is known to be held and every later transition is delivered on it.
		releaseWatch := armReservationReleaseWatch(surface, record)
		DeferCleanup(releaseWatch.stop)
		releaseHold()
		releaseWatch.awaitRelease()
		waitForSigningSurfaceGone(surface)
		assertReservationAbsent(record)
		Eventually(func() bool {
			err := Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(node), &appsv1.ChainNode{})
			return apierrors.IsNotFound(err)
		}).Should(BeTrue())

		recreated := app.BuildChainNode(ns.Name)
		recreated.Name = node.Name
		recreated.GenerateName = ""
		Expect(Framework().Client().Create(Framework().Context(), recreated)).To(Succeed())
		// Arm the ownership-aware reclaim here rather than after the waits below: from the moment Create
		// returns, the controller may claim a cluster-scoped reservation for this owner, so a failure
		// anywhere below would otherwise leave both the owner and that reservation behind — and namespace
		// teardown is fire-and-forget and could not reclaim a cluster-scoped object in any case. There is
		// nothing to record yet, so the reclaim resolves the reservation from the owner UID when it runs.
		// Nothing pins the pod this owner brings up: the hold this spec placed was on the previous pod
		// and was lifted above, so the reclaim's teardown wait is not blocked behind it.
		deferReservationCleanupForOwner("ChainNode", ns.Name, recreated.Name, recreated.UID)

		// Running is the acceptance bar here, not a second block: the phase already requires the pod
		// to be up and the node to answer RPC as synced, and the reacquisition itself is proven by
		// recordConsensusKeyReservation below rather than by any height the node reaches.
		WaitForChainNodeRunning(recreated)
		RefreshChainNode(recreated)
		Expect(recreated.UID).NotTo(Equal(node.UID), "the recreated ChainNode must be a new object, not the old one")

		reacquired := recordConsensusKeyReservation("ChainNode", ns.Name, recreated.Name, recreated.UID)
		Expect(reacquired.Name).To(Equal(record.Name),
			"the retained consensus key must resolve to the same deterministic reservation name")
		Expect(reacquired.ChainID).To(Equal(record.ChainID))
		Expect(reacquired.PublicKey).To(Equal(record.PublicKey))
		Expect(reacquired.Claim).To(Equal(record.Claim))
		Expect(reacquired.UID).NotTo(Equal(record.UID),
			"the reservation must have been genuinely released and recreated, not merely retargeted in place")
		assertReservationStable(reacquired)
	}))

	It("releases a ChainNodeSet reservation only after its validator signing workload is gone", WithNs(func(ns *corev1.Namespace) {
		app := apps.Nibiru()
		cns := app.BuildChainNodeSet(ns.Name, 0)
		// This spec exercises the legacy validator owner path only. Keep the required nodes field,
		// but drop the zero-instance fullnode group because its inherited snapshotNodeIndex is invalid
		// when there are no instances.
		cns.Spec.Nodes = []appsv1.NodeGroupSpec{}
		Expect(Framework().Client().Create(Framework().Context(), cns)).To(Succeed())
		WaitForChainNodeSetHeight(cns, 1)
		RefreshChainNodeSet(cns)

		validator := cns.Name + "-validator"
		record := recordConsensusKeyReservation("ChainNodeSet", ns.Name, cns.Name, cns.UID)
		deferReservationCleanup(record)
		Expect(record.Claim).To(Equal(validator),
			"the set owns the reservation while its validator child holds the claim")

		surface := signingSurface{
			namespace: ns.Name,
			claimPods: []string{validator},
		}
		assertSigningSurfaceServing(surface)

		podUID := signingPodUID(ns.Name, validator)
		// The set cannot release the reservation until its child ChainNodes are gone, and the child
		// cannot finish deleting while its pod object exists, so holding the pod pins both gates.
		setPodTestFinalizer(ns.Name, validator, true)
		releaseHold := onceReleaseHold(ns.Name, validator)
		DeferCleanup(releaseHold)

		Expect(Framework().Client().Delete(Framework().Context(), cns)).To(Succeed())
		waitForPodTerminating(ns.Name, validator, podUID)
		assertReservationHeldDuringSigningTeardown(surface, record, reservationHeldWindow)

		// As above, the watch is armed while the pod is still pinned so the release cannot be observed
		// only after the fact.
		releaseWatch := armReservationReleaseWatch(surface, record)
		DeferCleanup(releaseWatch.stop)
		releaseHold()
		releaseWatch.awaitRelease()
		waitForSigningSurfaceGone(surface)
		assertReservationAbsent(record)
		Eventually(func() bool {
			err := Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(cns), &appsv1.ChainNodeSet{})
			return apierrors.IsNotFound(err)
		}).Should(BeTrue())
		Eventually(func() bool {
			err := Framework().Client().Get(
				Framework().Context(), client.ObjectKey{Namespace: ns.Name, Name: validator}, &appsv1.ChainNode{},
			)
			return apierrors.IsNotFound(err)
		}).Should(BeTrue())
	}))
})

const (
	// reservationSampleInterval paces the direct sampling loops that watch a reservation against the
	// signing surface it must outlive.
	reservationSampleInterval = 500 * time.Millisecond

	// reservationHeldWindow is how long the reservation must be observed surviving alongside the
	// pinned signing workload. It is not racing anything — the hold cannot expire on its own — so the
	// only question is how many chances a regression gets to show itself. Two clocks drive that: the
	// deletion path requeues every second (~20 finalizer passes), and the ordinary reconcile runs at
	// the 5s period the e2e apps configure (~4 passes), which is what the per-sample object-UID check
	// gives a release-and-recreate regression a chance to trip. Shortening this to 10s would halve
	// the slower of the two to two cycles, for a saving that is noise next to chain bootstrap.
	reservationHeldWindow = 20 * time.Second

	// reservationReleaseTimeout bounds the drain that follows the hold being lifted.
	reservationReleaseTimeout = 5 * time.Minute
)

// reservationRecord is the pre-deletion snapshot of a ConsensusKeyReservation: its identity (name
// and object UID), the owner it was claimed for — kind, UID, and the namespaced name the failed-spec
// cleanup needs to reach it — and the chain-id/public-key pair its name is derived from. Every later
// assertion is made against this snapshot rather than a fresh read, so a reservation that is released
// and silently recreated cannot pass as "still held".
type reservationRecord struct {
	Name      string
	UID       types.UID
	OwnerUID  types.UID
	OwnerKind string
	Namespace string
	OwnerName string
	ChainID   string
	PublicKey string
	Claim     string
}

// recordConsensusKeyReservation captures the single reservation held by ownerUID and asserts it is
// internally consistent: the owner attribution matches the CR under test and the object name is the
// deterministic hash of its chain-id and public key.
func recordConsensusKeyReservation(ownerKind, namespace, ownerName string, ownerUID types.UID) reservationRecord {
	var record reservationRecord
	Eventually(func(g Gomega) {
		list := &appsv1.ConsensusKeyReservationList{}
		g.Expect(Framework().Client().List(Framework().Context(), list)).To(Succeed())
		owned := make([]appsv1.ConsensusKeyReservation, 0, 1)
		for i := range list.Items {
			if list.Items[i].Spec.OwnerUID == ownerUID {
				owned = append(owned, list.Items[i])
			}
		}
		g.Expect(owned).To(HaveLen(1), "exactly one consensus-key reservation must be held by owner UID %q", ownerUID)

		reservation := owned[0]
		g.Expect(reservation.UID).NotTo(BeEmpty())
		g.Expect(reservation.Spec.OwnerKind).To(Equal(ownerKind))
		g.Expect(reservation.Spec.Namespace).To(Equal(namespace))
		g.Expect(reservation.Spec.OwnerName).To(Equal(ownerName))
		g.Expect(reservation.Spec.ChainID).NotTo(BeEmpty())
		g.Expect(reservation.Spec.PublicKey).NotTo(BeEmpty())
		g.Expect(reservation.Spec.Claim).NotTo(BeEmpty())
		g.Expect(reservation.Name).To(Equal(
			managedcosmosigner.ConsensusKeyReservationName(reservation.Spec.ChainID, reservation.Spec.PublicKey),
		), "the reservation name must be derived from its chain-id and public key")

		record = reservationRecord{
			Name:      reservation.Name,
			UID:       reservation.UID,
			OwnerUID:  reservation.Spec.OwnerUID,
			OwnerKind: reservation.Spec.OwnerKind,
			Namespace: reservation.Spec.Namespace,
			OwnerName: reservation.Spec.OwnerName,
			ChainID:   reservation.Spec.ChainID,
			PublicKey: reservation.Spec.PublicKey,
			Claim:     reservation.Spec.Claim,
		}
	}).Should(Succeed())
	return record
}

// deferReservationCleanup keeps a cluster-scoped reservation from outliving a failed spec: namespace
// teardown is fire-and-forget and could not reclaim a cluster-scoped object in any case.
//
// Both specs that pin a pod register their hold release after this cleanup, and Ginkgo runs cleanups
// in reverse order of registration, so the hold is already lifted by the time reclaimReservation
// starts waiting on teardown.
func deferReservationCleanup(record reservationRecord) {
	DeferCleanup(func() {
		reclaimReservation(record)
	})
}

// deferReservationCleanupForOwner registers the same reclaim for an owner whose reservation has not
// been recorded yet, so it can be armed the moment a Create returns instead of only once the spec has
// observed what that Create led the controller to claim. A failure in between would otherwise leave
// both the owner and its cluster-scoped reservation behind.
//
// The reservation is resolved when the cleanup runs rather than captured now, because at registration
// time the controller may not have created it. Attribution stays exact: only a reservation whose
// immutable spec names this owner UID is ever considered, and the delete that ends the reclaim is
// still preconditioned on the object UID that resolution returned.
//
// An empty result is only clean once the owner is confirmed gone. Until then it is indistinguishable
// from an owner that has not yet claimed the reservation it is about to claim, so the owner delete is
// driven to a confirmed absent-or-terminating state before the empty list is allowed to mean anything.
func deferReservationCleanupForOwner(ownerKind, namespace, ownerName string, ownerUID types.UID) {
	Expect(ownerUID).NotTo(BeEmpty(), "an owner-scoped reservation cleanup needs the created owner's UID")
	owner := reservationRecord{
		OwnerKind: ownerKind,
		Namespace: namespace,
		OwnerName: ownerName,
		OwnerUID:  ownerUID,
	}
	DeferCleanup(func() {
		// Both reads share one budget, and teardown gets it first. A transiently unreadable cluster must
		// not consume the whole of it on the list while the owner remains live and able to claim a
		// reservation the list was too early to see.
		deadline := time.Now().Add(reservationReleaseTimeout)
		ownerGone := ensureReservationOwnerDeleted(owner, deadline)
		held, listed := reservationsHeldByOwner(owner, deadline)
		if !listed {
			By(fmt.Sprintf("could not resolve reservations for owner %s %s/%s: the cluster-scoped list stayed "+
				"unreadable for the whole cleanup budget", owner.OwnerKind, owner.Namespace, owner.OwnerName))
		}
		if len(held) == 0 {
			// Either nothing cluster-scoped is attributed to this owner or the list could not be read, and
			// neither is grounds for deleting a reservation. What the empty list is worth depends entirely
			// on the owner: gone, and its own finalizer has already released anything it held, so there is
			// nothing to reclaim; still standing, and the list proves nothing at all, because the owner can
			// claim a reservation the moment after this cleanup looked and there is no one left to reclaim
			// it. That case is reported rather than acted on — a reservation this cleanup never saw is one
			// it cannot safely delete.
			if !ownerGone {
				By(fmt.Sprintf("owner %s %s/%s was neither confirmed torn down nor observed holding a "+
					"reservation: any reservation it claims from here outlives the run",
					owner.OwnerKind, owner.Namespace, owner.OwnerName))
			}
			return
		}
		for _, record := range held {
			reclaimReservation(record)
		}
	})
}

// reclaimReservation deletes one recorded reservation without committing the fault these specs exist
// to catch. Deleting it outright would drop the reservation while its owner is still running and its
// validator still signing, which is exactly the double-signing window the reservation exists to close.
// So the reclaim follows the controller's own order instead: delete the recorded owner, wait for that
// owner and the signing surface of its claim to be gone, and only then delete the reservation. Every
// step is guarded by a UID precondition, so a reservation or owner that was already released and
// replaced under the same name is left untouched.
//
// A teardown that does not converge leaves the reservation in place: leaking one cluster-scoped object
// is the safe failure here, deleting a live consensus-key reservation is not.
//
// Every read below shares one budget. Owner teardown starts without depending on a reservation read;
// after the owner is terminating and the signing surface is drained, only a definite "still the
// recorded object" answer earns the fallback delete. An unreadable reservation is no proof of what
// would be destroyed.
func reclaimReservation(record reservationRecord) {
	deadline := time.Now().Add(reservationReleaseTimeout)
	if !ensureReservationOwnerDeleted(record, deadline) || !confirmReservationOwnerTeardown(record, deadline) {
		By(fmt.Sprintf("leaving ConsensusKeyReservation %q in place: teardown of owner %s %s/%s was not confirmed",
			record.Name, record.OwnerKind, record.Namespace, record.OwnerName))
		return
	}
	switch deleteRecordedReservation(record, deadline) {
	case reservationReleased:
	case reservationHeld:
		By(fmt.Sprintf("ConsensusKeyReservation %q was still the recorded object %q when the cleanup budget ran "+
			"out, so the reclaim outlives the run", record.Name, record.UID))
	case reservationUnknown:
		By(fmt.Sprintf("could not confirm ConsensusKeyReservation %q was reclaimed: it stayed unreadable for the "+
			"rest of the cleanup budget", record.Name))
	}
}

// deleteRecordedReservation deletes the exact recorded reservation and reports what the cluster last
// said about it. A single Delete call is a request, not a release: it can fail transiently, it can be
// swallowed by a finalizer that holds the object in Terminating, and even a successful one is
// unconfirmed until a later read says the object is gone. So the delete is retried to the deadline and
// the verdict comes from a read, never from the delete's own return.
//
// Retrying is safe because every attempt carries the recorded object UID as a precondition. Once the
// recorded reservation is gone, no attempt can reach a replacement that took its name — which is also
// why a different UID under that name counts as released here: the object this record was taken from
// is provably no longer there, and its successor belongs to whoever claimed it.
func deleteRecordedReservation(record reservationRecord, deadline time.Time) reservationLookup {
	for {
		current, lookup := reservationStillRecorded(record, deadline)
		if lookup != reservationHeld {
			return lookup
		}
		uid := current.UID
		// The delete's error is deliberately not the answer. NotFound and a failed precondition are
		// already the released verdict the next read returns, and a transient failure is evidence of
		// nothing in either direction.
		_ = Framework().Client().Delete(Framework().Context(), current, client.Preconditions{UID: &uid})
		if !time.Now().Before(deadline) {
			// Last word: read once more so a delete that did land is reported as the release it was,
			// rather than as the unconfirmed state it was in before it.
			_, lookup = reservationStillRecorded(record, deadline)
			return lookup
		}
		time.Sleep(reservationSampleInterval)
	}
}

// reservationsHeldByOwner resolves every reservation the cluster currently attributes to owner into a
// record the reclaim can act on. Attribution is by owner UID, which a reservation's immutable spec can
// never have retargeted, so a reservation that merely shares a name with one this owner once held is
// never picked up.
//
// A list error is transient until proven otherwise, so it is retried to the deadline rather than read
// as an empty cluster. Only the answer is reported alongside the records: an unreadable list is no
// evidence that nothing is reclaimable, and the caller must not treat it as one.
func reservationsHeldByOwner(owner reservationRecord, deadline time.Time) ([]reservationRecord, bool) {
	for {
		list := &appsv1.ConsensusKeyReservationList{}
		err := Framework().Client().List(Framework().Context(), list)
		if err == nil {
			var held []reservationRecord
			for i := range list.Items {
				reservation := &list.Items[i]
				if reservation.Spec.OwnerUID != owner.OwnerUID {
					continue
				}
				held = append(held, reservationRecord{
					Name:      reservation.Name,
					UID:       reservation.UID,
					OwnerUID:  reservation.Spec.OwnerUID,
					OwnerKind: reservation.Spec.OwnerKind,
					Namespace: reservation.Spec.Namespace,
					OwnerName: reservation.Spec.OwnerName,
					ChainID:   reservation.Spec.ChainID,
					PublicKey: reservation.Spec.PublicKey,
					Claim:     reservation.Spec.Claim,
				})
			}
			return held, true
		}
		if !time.Now().Before(deadline) {
			return nil, false
		}
		time.Sleep(reservationSampleInterval)
	}
}

// reservationLookup is what a cleanup managed to establish about the exact object a record was taken
// from. Unknown is the zero value so a lookup that was never resolved cannot be mistaken for either
// definite answer.
type reservationLookup int

const (
	// reservationUnknown means every read failed transiently within the budget, so the cluster said
	// nothing in either direction.
	reservationUnknown reservationLookup = iota
	// reservationHeld means the recorded object UID is still there.
	reservationHeld
	// reservationReleased means the API server answered definitively that the recorded object is not:
	// either NotFound, or a different object now living under the same name.
	reservationReleased
)

// reservationStillRecorded resolves whether the cluster still holds the exact object the record was
// taken from, retrying to deadline so a transient API error is not read as a released reservation.
// That distinction is the whole point: treating an unreadable Get as "already gone" would let a
// cleanup walk away from a reservation it is still responsible for, and treating it as "still held"
// would let one delete an object it never actually saw.
func reservationStillRecorded(record reservationRecord, deadline time.Time) (*appsv1.ConsensusKeyReservation, reservationLookup) {
	for {
		current := &appsv1.ConsensusKeyReservation{}
		err := Framework().Client().Get(Framework().Context(), client.ObjectKey{Name: record.Name}, current)
		switch {
		case err == nil && current.UID == record.UID:
			return current, reservationHeld
		case err == nil, apierrors.IsNotFound(err):
			return nil, reservationReleased
		}
		if !time.Now().Before(deadline) {
			return nil, reservationUnknown
		}
		time.Sleep(reservationSampleInterval)
	}
}

// reservationOwnerObject returns an empty object of the recorded owner's kind. An unrecognised kind
// leaves the reservation alone: with no way to confirm the owner is gone there is no safe delete.
func reservationOwnerObject(kind string) (client.Object, bool) {
	switch kind {
	case "ChainNode":
		return &appsv1.ChainNode{}, true
	case "ChainNodeSet":
		return &appsv1.ChainNodeSet{}, true
	}
	return nil, false
}

// deleteReservationOwner starts teardown of the CR that holds the reservation, which is what makes
// releasing it legitimate: the owner's own finalizer is what tears the signing path down and drops
// the reservation. The delete is preconditioned on the recorded owner UID so a same-named object
// belonging to anything else is never touched, and every error is ignored — this is only a nudge,
// and confirmReservationOwnerTeardown is what decides whether the reservation may be deleted.
func deleteReservationOwner(record reservationRecord) {
	owner, ok := reservationOwnerObject(record.OwnerKind)
	if !ok {
		return
	}
	key := client.ObjectKey{Namespace: record.Namespace, Name: record.OwnerName}
	if err := Framework().Client().Get(Framework().Context(), key, owner); err != nil {
		return
	}
	if owner.GetUID() != record.OwnerUID {
		return
	}
	uid := record.OwnerUID
	_ = Framework().Client().Delete(Framework().Context(), owner, client.Preconditions{UID: &uid})
}

// ensureReservationOwnerDeleted drives the exact recorded owner into a state where it can no longer
// claim a reservation, and reports whether it got there within the budget.
//
// One call to deleteReservationOwner is not that. It swallows both its Get and its Delete error, so a
// transient failure in either leaves a live owner behind and says nothing about it — and a live owner
// that simply has not reconciled yet presents exactly the same empty reservation list as an owner with
// nothing left to release. Only a confirmed absent-or-terminating owner tells those two apart, so the
// delete is retried until the cluster shows one.
//
// Every attempt is preconditioned on the recorded owner UID, which is what makes retrying safe: once
// that object is gone, no later attempt can reach whatever has taken its name.
func ensureReservationOwnerDeleted(record reservationRecord, deadline time.Time) bool {
	for {
		deleteReservationOwner(record)
		if reservationOwnerReleasable(record) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(reservationSampleInterval)
	}
}

// confirmReservationOwnerTeardown blocks until the recorded owner has reached a state its reservation
// may be deleted from and the signing surface of its claim has drained, and reports whether that
// happened within the shared budget. The claim is the validator node name in every spec here, which is
// also its pod name, so the same surface the release assertions watch is reachable from the record
// alone. An owner whose kind cannot be resolved is refused up front rather than waited out, since no
// read would ever confirm anything about it.
//
// The claim pod is not the whole gate. Production will not release a reservation while any pod
// carrying that ChainNode's deterministic signing-pod name is still up — a create-validator pod
// submitting a staking tx, or a signer-import pod loading the consensus key — so waiting only for the
// long-lived pod would let this fallback delete a reservation at a moment the controller itself would
// refuse to. The surface is widened to those pods here, scoped to the exact node they must belong to.
//
// A drained surface is necessary but not sufficient, and the claim's own node is the rest of the gate.
// Production releases a set-owned reservation only once finalizeReservationOwnerChildren has driven
// that child ChainNode out of the cluster, because a child that is still live reconciles on its own
// clock: a moment with no pod and no endpoint is then a gap between two pods rather than the end of the
// signing path, and the child brings its signing pod straight back under a key this cleanup has just
// deleted. So an empty surface only counts once the claim's node is itself gone, or terminating under
// the exact recorded owner and therefore past the point its controller creates pods. What is left is
// the case the surface was always the gate for: an owner or child already committed to teardown, with
// nothing up that could sign under this key.
//
// It reports rather than asserts on purpose: this only ever runs after something else already failed,
// and a cleanup that cannot confirm teardown has exactly one correct move, which is to leave the
// reservation alone rather than to add a second failure by deleting it.
func confirmReservationOwnerTeardown(record reservationRecord, deadline time.Time) bool {
	if _, ok := reservationOwnerObject(record.OwnerKind); !ok {
		return false
	}
	for {
		if reservationOwnerReleasable(record) {
			// Resolved per pass, not once: the claim's node may still be terminating when this starts
			// and gone by the time it ends, and only the answer that holds for the pass being decided
			// on may scope it.
			if owners, settled := claimSigningPodOwners(record); settled {
				surface := signingSurface{
					namespace:        record.Namespace,
					claimPods:        []string{record.Claim},
					signingPodOwners: owners,
				}
				observed, err := surface.observe()
				if err == nil && len(observed.pods) == 0 && len(observed.endpoints) == 0 {
					return true
				}
			}
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(reservationSampleInterval)
	}
}

// claimSigningPodOwners resolves the claim side of the teardown gate from a single read: the ChainNode
// whose temporary signing pods the fallback must wait out for this record, and whether that node has
// settled far enough for an empty signing surface to mean anything. One read answers both because they
// are one question about one object; two reads could answer it about two different moments.
//
// The claim names a ChainNode in both owner shapes this suite records: a standalone validator claims
// under its own name, and a ChainNodeSet claims under its validator child's name. The two are
// resolved differently on purpose. For the standalone shape the exact UID is already in the record,
// so no read is needed and no same-named replacement can be picked up by mistake — and that node is
// the owner the caller has already confirmed absent-or-terminating, so there is nothing further to
// gate on. For the set shape the child is a separate object the record does not carry, with a
// lifecycle of its own that outlives its parent's deletion timestamp, so it must be read and gated.
func claimSigningPodOwners(record reservationRecord) ([]signingPodOwner, bool) {
	if record.OwnerKind == "ChainNode" && record.Claim == record.OwnerName {
		return []signingPodOwner{{chainNode: record.Claim, uid: record.OwnerUID}}, true
	}
	child := &appsv1.ChainNode{}
	err := Framework().Client().Get(
		Framework().Context(), client.ObjectKey{Namespace: record.Namespace, Name: record.Claim}, child,
	)
	return claimChildSigningPodOwners(record, child, err)
}

// claimChildSigningPodOwners turns one read of the claim's ChainNode into that answer. For a set-owned
// claim, only a child attributed to the exact recorded parent UID is this record's to reason about, and
// only two states of it let an empty surface count:
//
//   - NotFound. There is no such node, so nothing can be attributed to it — and production confirmed
//     that node's signing pods were gone before it let the node itself go, so there is nothing left to
//     wait for.
//   - Terminating under the recorded owner. Its controller runs the finalizer path from here and
//     creates no pods, so the pods it still has can only drain; the caller's surface check is what
//     waits them out, scoped to this child's UID.
//
// A live child under the recorded owner is the case this gate exists for, and settles nothing however
// empty the surface looks. Its parent being Terminating does not stop it: production deletes it in
// finalizeReservationOwnerChildren and waits for it to be gone before releasing, and until that lands
// the child reconciles normally and brings the signing pod back a moment later — under a key this
// cleanup would already have dropped.
//
// Everything else resolves nothing and the caller retries rather than treating silence as an all-clear:
// a read error, an object with no UID, or a child this record cannot attribute — controlled by some
// other UID, or by nothing at all. A same-named replacement is another parent's child and may be
// signing under this very key, so it is never grounds for dropping the reservation either.
func claimChildSigningPodOwners(record reservationRecord, child *appsv1.ChainNode, err error) ([]signingPodOwner, bool) {
	switch {
	case apierrors.IsNotFound(err):
		return nil, true
	case err != nil, child.GetUID() == "":
		return nil, false
	}
	owners := []signingPodOwner{{chainNode: record.Claim, uid: child.GetUID()}}
	// Only a set-owned claim is gated on its child. A standalone owner claims under its own name and is
	// handled by the caller above; its one other claim shape, a cosmosigner claim, names no ChainNode,
	// so that read comes back NotFound.
	if record.OwnerKind != "ChainNodeSet" {
		return owners, true
	}
	controller := metav1.GetControllerOf(child)
	if controller == nil || controller.UID != record.OwnerUID || child.GetDeletionTimestamp().IsZero() {
		return nil, false
	}
	return owners, true
}

// reservationOwnerReleasable reports whether the recorded owner has reached a state in which deleting
// its reservation cannot strand a live one. Exactly two states qualify, and only for the exact
// recorded UID:
//
//   - Absent. The owner finished deleting, so its finalizer already released everything it held and a
//     reservation still standing is one nothing will come back for.
//   - Terminating. Requiring absence alone would make this fallback unreachable on the one path it
//     exists for: the reservation-owner finalizer holds the exact owner in Terminating precisely until
//     the reservation is released, so an owner that is stuck mid-finalize never becomes NotFound and
//     the reservation it is stuck on would leak past the run. An owner carrying a deletion timestamp
//     cannot start signing again, and the caller pairs this with a fully drained signing surface.
//
// Everything else keeps the reservation off limits. A read error proves nothing. An owner with no
// deletion timestamp is live and may still be signing under this key. A different UID under the same
// name is a replacement, whose own lifecycle this cleanup has no standing to act on — and whose
// reservation this is not, since attribution is by owner UID.
func reservationOwnerReleasable(record reservationRecord) bool {
	owner, ok := reservationOwnerObject(record.OwnerKind)
	if !ok {
		return false
	}
	err := Framework().Client().Get(
		Framework().Context(), client.ObjectKey{Namespace: record.Namespace, Name: record.OwnerName}, owner,
	)
	if err != nil {
		return apierrors.IsNotFound(err)
	}
	return owner.GetUID() == record.OwnerUID && !owner.GetDeletionTimestamp().IsZero()
}

// signingSurface describes what must be gone before a reservation may be released: the pods that
// sign for its claims, and the Service endpoints still routing to them.
//
// The specs here run plain validators with no cosmosigner and no TmKMS, so there is no signer
// StatefulSet and no signer-target or cosmosigner replica pod to find. Mirroring the controller's
// label and StatefulSet-name predicates would therefore only ever match the claim pods this already
// tracks by name, so the surface is kept to what is actually load-bearing.
type signingSurface struct {
	namespace string

	// claimPods are the deterministic pod names the reservation's claims cover. They are matched by
	// name rather than by label because a plain validator pod carries no owner label (only signer
	// targets do), and because an endpoint outlives the pod whose labels it was derived from.
	claimPods []string

	// signingPodOwners widens the pod half of the surface to the temporary signing pods production
	// blocks a reservation release on, for callers that must gate on the whole of what the controller
	// gates on rather than only on the long-lived claim pod. The spec-level assertions leave it empty
	// on purpose: they pin one named pod and must keep proving their point against that pod alone,
	// never against some unrelated helper that happened to be up at the same moment.
	signingPodOwners []signingPodOwner
}

// signingPodOwner is one ChainNode whose temporary signing pods must be gone before its reservation
// may be deleted: the node name their deterministic names are derived from, and the exact object UID
// they must belong to.
type signingPodOwner struct {
	chainNode string
	uid       types.UID
}

// chainNodeTemporaryPodSuffixes mirrors temporaryPodSuffixes in
// internal/controllers/chainnode/predicate.go. It is copied rather than imported because the
// production list and its matcher are unexported and the e2e suite has no business widening the
// controller's API surface to read them. Keep it in sync: a suffix added there and missing here
// narrows the fallback teardown gate back toward the claim pod alone, which is the gap this list
// exists to close.
var chainNodeTemporaryPodSuffixes = []string{
	"config-generator", "data-init", "init-data", "genesis-init", "tmkms-vault-upload", "tmkms-generate-identity",
	"write-file", "create-validator", "signer-pubkey", "signer-import",
}

// chainNodeSigningPodName mirrors isChainNodeSigningPodName: the ChainNode's own pod, plus every
// temporary pod whose deterministic name is derived from it — matched with the trailing "-" prefix
// form too, since a Job names its pods by appending a generated suffix.
func chainNodeSigningPodName(name, chainNodeName string) bool {
	if name == chainNodeName {
		return true
	}
	for _, suffix := range chainNodeTemporaryPodSuffixes {
		base := chainNodeName + "-" + suffix
		if name == base || strings.HasPrefix(name, base+"-") {
			return true
		}
	}
	return false
}

// blocksAsSigningPod mirrors finalizeOwnedSigningPods for each exact ChainNode owner. Production waits
// for every pod controlled by the ChainNode, regardless of name, and also refuses release when a pod
// with one of the ChainNode's deterministic signing-path names is not controlled by that exact UID.
// The latter includes Job-generated create-validator and signer-import pod names as well as orphans or
// same-named replacement pods; treating those as somebody else's teardown would be weaker than the
// production safety gate.
func (s signingSurface) blocksAsSigningPod(pod *corev1.Pod) bool {
	controller := metav1.GetControllerOf(pod)
	for _, owner := range s.signingPodOwners {
		owned := controller != nil && controller.UID == owner.uid
		if owned || chainNodeSigningPodName(pod.Name, owner.chainNode) {
			return true
		}
	}
	return false
}

type signingObservation struct {
	pods      []string
	endpoints []string
}

func (o signingObservation) String() string {
	return fmt.Sprintf("pods=%v endpoints=%v", o.pods, o.endpoints)
}

func (s signingSurface) observe() (signingObservation, error) {
	return s.observeAt("")
}

// observeAt reads the surface as of the store revision resourceVersion names, or as of whatever is
// current when it is empty.
//
// A revision makes the read a historical fact rather than a sample: resourceVersionMatch=Exact is
// served from the backing store at that revision instead of from the API server's watch cache, so
// what comes back is the surface as it stood then and nothing that happened after. That is what lets
// a release be ordered against the surface rather than merely followed by a look at it. The revision
// need not come from these resources: a list resourceVersion names a revision of the whole backing
// store rather than a per-resource counter, so the revision a reservation was deleted at pins pod and
// endpoint reads to that same instant.
func (s signingSurface) observeAt(resourceVersion string) (signingObservation, error) {
	var observed signingObservation

	names := make(map[string]struct{}, len(s.claimPods))
	for _, name := range s.claimPods {
		names[name] = struct{}{}
	}

	pods := &corev1.PodList{}
	if err := Framework().Client().List(
		Framework().Context(), pods, signingSurfaceListOptions(s.namespace, resourceVersion)...,
	); err != nil {
		return observed, err
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		_, claimed := names[pod.Name]
		if !claimed && !s.blocksAsSigningPod(pod) {
			continue
		}
		// A Terminating pod still counts: the signing path is torn down only once the object is
		// gone, which is exactly the gate the controller waits on before releasing.
		observed.pods = append(observed.pods, pod.Name)
	}
	sort.Strings(observed.pods)

	endpoints, err := s.observeEndpoints(names, resourceVersion)
	if err != nil {
		return observed, err
	}
	observed.endpoints = endpoints
	return observed, nil
}

// signingSurfaceListOptions scopes a surface read to the namespace, and to one exact store revision
// when resourceVersion is set. They are rebuilt per list because flattening them mutates the raw
// options they carry.
//
// An empty resourceVersion is left meaning "whatever is current" rather than being refused here:
// most surface reads are ordinary progress checks with no revision to anchor to. The one caller that
// must anchor to a revision asserts it has one before it calls.
func signingSurfaceListOptions(namespace, resourceVersion string) []client.ListOption {
	options := []client.ListOption{client.InNamespace(namespace)}
	if resourceVersion != "" {
		options = append(options, &client.ListOptions{Raw: &metav1.ListOptions{
			ResourceVersion:      resourceVersion,
			ResourceVersionMatch: metav1.ResourceVersionMatchExact,
		}})
	}
	return options
}

// observeEndpoints collects every Service endpoint still targeting one of the claim pods, from
// both EndpointSlices and the legacy Endpoints API — the same pair the signer teardown itself waits
// on. Terminating pods stay published because the internal Service sets PublishNotReadyAddresses.
//
// Both lists are read at the caller's revision, so an endpoint half read at one moment and a pod half
// read at another can never be reported as one observation.
func (s signingSurface) observeEndpoints(podNames map[string]struct{}, resourceVersion string) ([]string, error) {
	var refs []string

	slices := &discoveryv1.EndpointSliceList{}
	if err := Framework().Client().List(
		Framework().Context(), slices, signingSurfaceListOptions(s.namespace, resourceVersion)...,
	); err != nil {
		return nil, err
	}
	for i := range slices.Items {
		slice := &slices.Items[i]
		for j := range slice.Endpoints {
			ref := slice.Endpoints[j].TargetRef
			if ref == nil || ref.Kind != "Pod" {
				continue
			}
			if _, ok := podNames[ref.Name]; !ok {
				continue
			}
			refs = append(refs, fmt.Sprintf("endpointslice/%s->%s", slice.Name, ref.Name))
		}
	}

	legacy := &corev1.EndpointsList{}
	if err := Framework().Client().List(
		Framework().Context(), legacy, signingSurfaceListOptions(s.namespace, resourceVersion)...,
	); err != nil {
		return nil, err
	}
	for i := range legacy.Items {
		item := &legacy.Items[i]
		for j := range item.Subsets {
			addresses := append(append([]corev1.EndpointAddress(nil), item.Subsets[j].Addresses...), item.Subsets[j].NotReadyAddresses...)
			for _, address := range addresses {
				if address.TargetRef == nil || address.TargetRef.Kind != "Pod" {
					continue
				}
				if _, ok := podNames[address.TargetRef.Name]; !ok {
					continue
				}
				refs = append(refs, fmt.Sprintf("endpoints/%s->%s", item.Name, address.TargetRef.Name))
			}
		}
	}

	sort.Strings(refs)
	return refs, nil
}

// assertSigningSurfaceServing pins the pre-deletion baseline. Without it the survival window below
// could pass vacuously against a surface that was never observable in the first place.
func assertSigningSurfaceServing(surface signingSurface) {
	Eventually(func(g Gomega) {
		observed, err := surface.observe()
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(observed.pods).NotTo(BeEmpty(), "no signing workload was found before deletion")
		g.Expect(observed.endpoints).NotTo(BeEmpty(), "no Service endpoint routed to the signing workload before deletion")
	}).Should(Succeed())
}

// assertReservationHeldDuringSigningTeardown proves the reservation outlives the old signing surface
// during the first observable teardown phase, while the caller's hold pins that surface in place.
//
// Each sample reads the reservation first (t1) and the signing surface second (t2 > t1), so a sample
// that shows the reservation already gone at t1 while the surface is still up at t2 is unambiguous
// proof of an early release: the surface cannot have appeared after the release. The loop samples
// directly instead of through Eventually because a retry would let the surface drain in the meantime
// and hide the very violation being looked for.
//
// The per-sample liveness check is on the claim pod, not on the whole surface: an EndpointSlice entry
// can outlive the pod it points at, so accepting a stale endpoint as proof that the hold still stands
// would let the window pass after the pinned pod had already gone. Endpoint coverage is asserted
// separately, once, via sawEndpoints — every sample must show the pod, and at least one must show an
// endpoint routing to it.
func assertReservationHeldDuringSigningTeardown(surface signingSurface, want reservationRecord, window time.Duration) {
	var samples int
	var sawEndpoints bool
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		current := &appsv1.ConsensusKeyReservation{}
		getErr := Framework().Client().Get(Framework().Context(), client.ObjectKey{Name: want.Name}, current)
		observed, observeErr := surface.observe()
		// A transient API error makes the pair of reads prove nothing in either direction, so the
		// sample is dropped rather than failed. Dropping every sample is still caught below.
		if observeErr != nil || (getErr != nil && !apierrors.IsNotFound(getErr)) {
			time.Sleep(reservationSampleInterval)
			continue
		}

		Expect(observed.pods).NotTo(BeEmpty(),
			"the pinned signing pod must still exist for the whole window; the hold was lost")
		Expect(apierrors.IsNotFound(getErr)).To(BeFalse(),
			"reservation %q was released while the old signing surface was still up (%s)", want.Name, observed)
		Expect(current.UID).To(Equal(want.UID),
			"reservation %q was released and recreated during teardown", want.Name)
		Expect(current.Spec.OwnerUID).To(Equal(want.OwnerUID))
		Expect(current.Spec.ChainID).To(Equal(want.ChainID))
		Expect(current.Spec.PublicKey).To(Equal(want.PublicKey))

		sawEndpoints = sawEndpoints || len(observed.endpoints) > 0
		samples++
		time.Sleep(reservationSampleInterval)
	}
	Expect(samples).To(BeNumerically(">", 0), "the survival window observed no samples")
	Expect(sawEndpoints).To(BeTrue(),
		"no Service endpoint referenced the terminating signing pod, so the endpoint half of the surface went unproven")
}

// reservationReleaseWatch drives the second teardown phase, once the hold has been lifted, from a
// Kubernetes watch that is armed while the caller still has the signing surface pinned.
//
// Arming order is what makes the ordering assertion sound instead of lucky. A polled wait only ever
// learns of a release at its next sample, so a release that beat the endpoint drain by less than one
// sampling interval had already left nothing to see by the time the test looked. Here instead:
//
//   - The reservation is read once, by exact object UID, before the stream opens, and the stream starts
//     from that read's resourceVersion. No transition can fall into the gap between the two: everything
//     after the read is delivered as an event.
//   - That read happens while the hold is still on. The controller cannot release before the claim pod
//     object is gone, and the pod object cannot go while the test's finalizer is on it, so a reservation
//     present here is proof that nothing was released across the whole held phase.
//   - The Deleted event is handled inline, in the same goroutine: the surface is read immediately after
//     it arrives rather than on the next tick of a poll.
//
// Pods and endpoints are both hard gates on the ordering. A published endpoint is a live route into the
// signing workload, so releasing the key while one still targets a claim pod is the same fault as
// releasing it while the pod exists.
//
// Both directions are closed, because the surface is not sampled after the release but read at it. The
// Deleted event carries the reservation as of the store revision its delete was committed at, and the
// surface is listed at exactly that revision, so what comes back is the pod and endpoint state at the
// instant the reservation ceased to exist. A read of "now" could only ever answer a later question, and
// since the surface drains monotonically during teardown it would answer it more favourably; anchoring
// removes that drift entirely. A violation shorter than the round trip that follows the event — the one
// a post-hoc look would have missed — is therefore still reported.
type reservationReleaseWatch struct {
	surface  signingSurface
	want     reservationRecord
	stream   watch.Interface
	stopOnce sync.Once
}

// armReservationReleaseWatch asserts the recorded reservation is still held and opens the watch that
// will observe its release. It must be called before the caller lifts its hold on the signing surface.
func armReservationReleaseWatch(surface signingSurface, want reservationRecord) *reservationReleaseWatch {
	armed := &reservationReleaseWatch{surface: surface, want: want}

	current := &appsv1.ConsensusKeyReservation{}
	Expect(Framework().Client().Get(
		Framework().Context(), client.ObjectKey{Name: want.Name}, current,
	)).To(Succeed(), "reservation %q must still be held when the release watch is armed", want.Name)
	Expect(current.UID).To(Equal(want.UID),
		"reservation %q was released and recreated before the release watch was armed", want.Name)

	watcher, err := client.NewWithWatch(Framework().RestConfig(), client.Options{
		Scheme: Framework().Client().Scheme(),
		Mapper: Framework().Client().RESTMapper(),
	})
	Expect(err).NotTo(HaveOccurred(), "create watch client for reservation %q", want.Name)
	stream, err := watcher.Watch(Framework().Context(), &appsv1.ConsensusKeyReservationList{}, &client.ListOptions{
		FieldSelector: fields.OneTermEqualSelector("metadata.name", want.Name),
		Raw:           &metav1.ListOptions{ResourceVersion: current.ResourceVersion},
	})
	Expect(err).NotTo(HaveOccurred(), "open release watch for reservation %q", want.Name)
	armed.stream = stream
	return armed
}

// awaitRelease blocks until the recorded reservation is released and asserts the signing surface was
// already gone at that moment. Anything that ends the stream without a Deleted event — a closed
// channel from an API-server restart or a compacted resourceVersion, or an Error event — proves
// nothing in either direction. The spec fails instead of silently falling back to sampling that can
// miss the exact short ordering regression this test exists to catch.
func (w *reservationReleaseWatch) awaitRelease() {
	deadline := time.Now().Add(reservationReleaseTimeout)
	defer w.stop()

	expired := time.NewTimer(time.Until(deadline))
	defer expired.Stop()
	for {
		select {
		case event, open := <-w.stream.ResultChan():
			if !open {
				Fail(fmt.Sprintf("release watch on reservation %q ended before the release", w.want.Name))
				return
			}
			switch event.Type {
			case watch.Deleted:
				w.assertReleasedAfterSigningTeardown(event, deadline)
				return
			case watch.Error:
				Fail(fmt.Sprintf("release watch on reservation %q reported %v", w.want.Name, event.Object))
				return
			case watch.Added, watch.Modified:
				// The stream is filtered to this one name, and the watch was armed from a revision at which
				// the recorded object existed, so any object arriving here before its Deleted event is the
				// recorded one. A different UID means it was released and recreated in place.
				reservation, ok := event.Object.(*appsv1.ConsensusKeyReservation)
				Expect(ok).To(BeTrue(), "unexpected %T on the watch for reservation %q", event.Object, w.want.Name)
				Expect(reservation.UID).To(Equal(w.want.UID),
					"reservation %q was released and recreated during teardown", w.want.Name)
			}
		case <-expired.C:
			Fail(fmt.Sprintf("reservation %q was not released after signing teardown", w.want.Name))
			return
		}
	}
}

// stop makes the watch lifetime safe across every failure boundary between arming and awaiting it.
// Both the call-site cleanup and awaitRelease own a stop, and sync.Once keeps that ownership idempotent.
func (w *reservationReleaseWatch) stop() {
	w.stopOnce.Do(w.stream.Stop)
}

// assertReleasedAfterSigningTeardown checks the surface against the release the watch just reported,
// read at the revision that release was committed at rather than at whatever is current by the time
// the read lands. An event carrying no resourceVersion is refused instead of being read as "now": an
// unanchored read is the sampling this watch exists to replace, and silently falling back to it would
// turn a proof into a guess without anything saying so.
func (w *reservationReleaseWatch) assertReleasedAfterSigningTeardown(event watch.Event, deadline time.Time) {
	reservation, ok := event.Object.(*appsv1.ConsensusKeyReservation)
	Expect(ok).To(BeTrue(), "unexpected %T on the watch for reservation %q", event.Object, w.want.Name)
	Expect(reservation.UID).To(Equal(w.want.UID),
		"a reservation other than the recorded %q was released during teardown", w.want.Name)
	Expect(reservation.ResourceVersion).NotTo(BeEmpty(),
		"the release of reservation %q arrived without a resourceVersion, so it cannot be ordered against the signing surface",
		w.want.Name)

	observed, err := observeSigningSurfaceAt(w.surface, reservation.ResourceVersion, deadline)
	Expect(err).NotTo(HaveOccurred(),
		"the signing surface could not be read at the revision reservation %q was released at, so the release could not be ordered against it",
		w.want.Name)
	Expect(observed.pods).To(BeEmpty(),
		"reservation %q was released while signing pods were still up (%s)", w.want.Name, observed)
	Expect(observed.endpoints).To(BeEmpty(),
		"reservation %q was released while Service endpoints still routed to the signing workload (%s)",
		w.want.Name, observed)
}

// observeSigningSurfaceAt reads the surface as of resourceVersion, retrying transient API errors to
// deadline. Retrying costs nothing in soundness: the state at a fixed revision is immutable, so a later
// attempt returns exactly what the first one would have.
//
// A compacted revision is the one error that is not transient. Once the backing store has dropped that
// revision no retry can ever answer, and no other read answers the same question — "now" is a different
// moment, and refusing to substitute it is the entire point of anchoring. So it is returned at once and
// the caller fails the spec, rather than spending the budget on a read that cannot succeed or passing on
// the strength of one that was never asked.
func observeSigningSurfaceAt(surface signingSurface, resourceVersion string, deadline time.Time) (signingObservation, error) {
	for {
		observed, err := surface.observeAt(resourceVersion)
		if err == nil {
			return observed, nil
		}
		if apierrors.IsResourceExpired(err) || apierrors.IsGone(err) || !time.Now().Before(deadline) {
			return observed, err
		}
		time.Sleep(reservationSampleInterval)
	}
}

func waitForSigningSurfaceGone(surface signingSurface) {
	Eventually(func(g Gomega) {
		observed, err := surface.observe()
		g.Expect(err).NotTo(HaveOccurred())
		g.Expect(observed.pods).To(BeEmpty())
		g.Expect(observed.endpoints).To(BeEmpty())
	}).Should(Succeed())
}

func assertReservationAbsent(want reservationRecord) {
	Eventually(func() bool {
		err := Framework().Client().Get(
			Framework().Context(), client.ObjectKey{Name: want.Name}, &appsv1.ConsensusKeyReservation{},
		)
		return apierrors.IsNotFound(err)
	}).Should(BeTrue(),
		"reservation %q must be released once its owner and signing surface are gone", want.Name)
}

// assertReservationStable proves a reacquisition settled into a clean claim instead of a
// release/reacquire loop, which would show up as a changing object UID. The 10s window is two full
// reconciles at the 5s period the e2e apps configure, which is the floor for calling anything
// steady: a shorter window is not guaranteed to span even one complete pass.
func assertReservationStable(want reservationRecord) {
	Consistently(func(g Gomega) {
		current := &appsv1.ConsensusKeyReservation{}
		g.Expect(Framework().Client().Get(
			Framework().Context(), client.ObjectKey{Name: want.Name}, current,
		)).To(Succeed())
		g.Expect(current.UID).To(Equal(want.UID), "the reacquired reservation must not churn")
		g.Expect(current.Spec.OwnerUID).To(Equal(want.OwnerUID))
		g.Expect(current.Spec.Claim).To(Equal(want.Claim))
	}, 10*time.Second, reservationSampleInterval).Should(Succeed())
}

func signingPodUID(namespace, name string) types.UID {
	var uid types.UID
	Eventually(func(g Gomega) {
		pod := &corev1.Pod{}
		g.Expect(Framework().Client().Get(
			Framework().Context(), client.ObjectKey{Namespace: namespace, Name: name}, pod,
		)).To(Succeed())
		g.Expect(pod.UID).NotTo(BeEmpty())
		uid = pod.UID
	}).Should(Succeed())
	return uid
}

// waitForPodTerminating blocks until the held pod carries a deletion timestamp, so the survival
// window starts inside the teardown rather than before it has been observed at all.
func waitForPodTerminating(namespace, name string, uid types.UID) {
	Eventually(func() bool {
		pod := &corev1.Pod{}
		if err := Framework().Client().Get(
			Framework().Context(), client.ObjectKey{Namespace: namespace, Name: name}, pod,
		); err != nil {
			return false
		}
		return pod.UID == uid && pod.DeletionTimestamp != nil
	}).Should(BeTrue())
}

// onceReleaseHold returns an idempotent hold release so the spec can lift the hold at the right
// moment and still register it as cleanup for the paths that fail before reaching that point.
//
// Only a removal that actually returned counts as released. setPodTestFinalizer fails the spec by
// panicking, so an in-spec call that dies leaves the flag down and the cleanup copy retries it;
// marking the hold released first would strand the finalizer on the pod and wedge the namespace on
// the one path where the retry is the whole point.
func onceReleaseHold(namespace, name string) func() {
	released := false
	return func() {
		if released {
			return
		}
		setPodTestFinalizer(namespace, name, false)
		released = true
	}
}

func attributedPVCCount(namespace string, uid types.UID) int {
	list := &corev1.PersistentVolumeClaimList{}
	if err := Framework().Client().List(Framework().Context(), list, client.InNamespace(namespace)); err != nil {
		return -1
	}
	count := 0
	for i := range list.Items {
		if list.Items[i].Annotations[resourcecleanup.AnnotationRootOwnerUID] == string(uid) {
			count++
		}
	}
	return count
}

func attributedSecretCount(namespace string, uid types.UID) int {
	list := &corev1.SecretList{}
	if err := Framework().Client().List(Framework().Context(), list, client.InNamespace(namespace)); err != nil {
		return -1
	}
	count := 0
	for i := range list.Items {
		if list.Items[i].Annotations[resourcecleanup.AnnotationRootOwnerUID] == string(uid) {
			count++
		}
	}
	return count
}
