package e2e

import (
	"fmt"
	"sort"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
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

		releaseHold()
		waitForReservationReleasedAfterSigningTeardown(surface, record)
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
		// Running is the acceptance bar here, not a second block: the phase already requires the pod
		// to be up and the node to answer RPC as synced, and the reacquisition itself is proven by
		// recordConsensusKeyReservation below rather than by any height the node reaches.
		WaitForChainNodeRunning(recreated)
		RefreshChainNode(recreated)
		Expect(recreated.UID).NotTo(Equal(node.UID), "the recreated ChainNode must be a new object, not the old one")

		// The reacquired reservation is deliberately left out of deferReservationCleanup: it is live
		// state belonging to a running ChainNode, and that cleanup reclaims a reservation by first
		// deleting its owner, which here would mean tearing down the very node this spec just proved
		// healthy. Namespace teardown deletes the recreated node, whose own reservation finalizer
		// releases it. The pre-deletion record above stays registered and is already a no-op against
		// this object, because its UID precondition no longer matches.
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
		// This spec exercises the legacy validator owner path only. Drop the zero-instance fullnode
		// group because its inherited snapshotNodeIndex is invalid when there are no instances.
		cns.Spec.Nodes = nil
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

		releaseHold()
		waitForReservationReleasedAfterSigningTeardown(surface, record)
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
// The reclaim must not commit the fault these specs exist to catch, though. Deleting the reservation
// outright would drop it while its owner is still running and its validator still signing, which is
// exactly the double-signing window the reservation exists to close. So the cleanup follows the
// controller's own order instead: delete the recorded owner, wait for that owner and the signing
// surface of its claim to be gone, and only then delete the reservation. Every step is guarded by a
// UID precondition, so a reservation or owner that was already released and replaced under the same
// name is left untouched.
//
// Both specs that pin a pod register their hold release after this cleanup, and Ginkgo runs cleanups
// in reverse order of registration, so the hold is already lifted by the time the wait below starts.
// A teardown that still does not converge leaves the reservation in place: leaking one cluster-scoped
// object is the safe failure here, deleting a live consensus-key reservation is not.
func deferReservationCleanup(record reservationRecord) {
	DeferCleanup(func() {
		if _, held := reservationStillRecorded(record); !held {
			return
		}
		deleteReservationOwner(record)
		if !confirmReservationOwnerTeardown(record) {
			By(fmt.Sprintf("leaving ConsensusKeyReservation %q in place: teardown of owner %s %s/%s was not confirmed",
				record.Name, record.OwnerKind, record.Namespace, record.OwnerName))
			return
		}
		current, held := reservationStillRecorded(record)
		if !held {
			return
		}
		uid := current.UID
		_ = Framework().Client().Delete(Framework().Context(), current, client.Preconditions{UID: &uid})
	})
}

// reservationStillRecorded reports whether the cluster still holds the exact object the record was
// taken from. Anything else — released, replaced under the same name, or unreadable — means this
// cleanup has nothing of its own left to reclaim.
func reservationStillRecorded(record reservationRecord) (*appsv1.ConsensusKeyReservation, bool) {
	current := &appsv1.ConsensusKeyReservation{}
	if err := Framework().Client().Get(Framework().Context(), client.ObjectKey{Name: record.Name}, current); err != nil {
		return nil, false
	}
	if current.UID != record.UID {
		return nil, false
	}
	return current, true
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

// confirmReservationOwnerTeardown blocks until the recorded owner CR is gone and the signing surface
// of its claim has drained, and reports whether that happened within the drain budget. The claim is
// the validator node name in every spec here, which is also its pod name, so the same surface the
// release assertions watch is reachable from the record alone. An owner whose kind cannot be resolved
// is refused up front rather than waited out, since no read would ever confirm it gone.
//
// It reports rather than asserts on purpose: this only ever runs after something else already failed,
// and a cleanup that cannot confirm teardown has exactly one correct move, which is to leave the
// reservation alone rather than to add a second failure by deleting it.
func confirmReservationOwnerTeardown(record reservationRecord) bool {
	if _, ok := reservationOwnerObject(record.OwnerKind); !ok {
		return false
	}
	surface := signingSurface{namespace: record.Namespace, claimPods: []string{record.Claim}}
	deadline := time.Now().Add(reservationReleaseTimeout)
	for {
		if reservationOwnerGone(record) {
			observed, err := surface.observe()
			if err == nil && len(observed.pods) == 0 && len(observed.endpoints) == 0 {
				return true
			}
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(reservationSampleInterval)
	}
}

// reservationOwnerGone reports whether the recorded owner CR is absent. Only an explicit NotFound
// counts: a read error proves nothing, and a live object under the same name is something this
// cleanup cannot reason about, so both keep the reservation off limits.
func reservationOwnerGone(record reservationRecord) bool {
	owner, ok := reservationOwnerObject(record.OwnerKind)
	if !ok {
		return false
	}
	err := Framework().Client().Get(
		Framework().Context(), client.ObjectKey{Namespace: record.Namespace, Name: record.OwnerName}, owner,
	)
	return apierrors.IsNotFound(err)
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
}

type signingObservation struct {
	pods      []string
	endpoints []string
}

func (o signingObservation) String() string {
	return fmt.Sprintf("pods=%v endpoints=%v", o.pods, o.endpoints)
}

func (s signingSurface) observe() (signingObservation, error) {
	var observed signingObservation

	names := make(map[string]struct{}, len(s.claimPods))
	for _, name := range s.claimPods {
		names[name] = struct{}{}
	}

	pods := &corev1.PodList{}
	if err := Framework().Client().List(Framework().Context(), pods, client.InNamespace(s.namespace)); err != nil {
		return observed, err
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		if _, claimed := names[pod.Name]; !claimed {
			continue
		}
		// A Terminating pod still counts: the signing path is torn down only once the object is
		// gone, which is exactly the gate the controller waits on before releasing.
		observed.pods = append(observed.pods, pod.Name)
	}
	sort.Strings(observed.pods)

	endpoints, err := s.observeEndpoints(names)
	if err != nil {
		return observed, err
	}
	observed.endpoints = endpoints
	return observed, nil
}

// observeEndpoints collects every Service endpoint still targeting one of the claim pods, from
// both EndpointSlices and the legacy Endpoints API — the same pair the signer teardown itself waits
// on. Terminating pods stay published because the internal Service sets PublishNotReadyAddresses.
func (s signingSurface) observeEndpoints(podNames map[string]struct{}) ([]string, error) {
	var refs []string

	slices := &discoveryv1.EndpointSliceList{}
	if err := Framework().Client().List(Framework().Context(), slices, client.InNamespace(s.namespace)); err != nil {
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
	if err := Framework().Client().List(Framework().Context(), legacy, client.InNamespace(s.namespace)); err != nil {
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

// waitForReservationReleasedAfterSigningTeardown drives the second teardown phase, once the hold has
// been lifted. It keeps the same reservation-then-surface read order, so a sample showing the
// reservation gone while the surface is still up proves an early release rather than a race: the
// surface only ever drains here, so whatever the later read still sees was already there at the
// earlier one.
//
// Pods and endpoints are both hard gates on that ordering. A published endpoint is a live route into
// the signing workload, so releasing the key while one still targets a claim pod is the same fault as
// releasing it while the pod exists. Merely waiting the endpoints out afterwards would accept a
// release that raced ahead of them, which is the half of the surface the claim rests on.
func waitForReservationReleasedAfterSigningTeardown(surface signingSurface, want reservationRecord) {
	var observed signingObservation
	deadline := time.Now().Add(reservationReleaseTimeout)
	for {
		current := &appsv1.ConsensusKeyReservation{}
		getErr := Framework().Client().Get(Framework().Context(), client.ObjectKey{Name: want.Name}, current)
		sampled, observeErr := surface.observe()
		if observeErr == nil {
			observed = sampled
		}
		// As above, a transient API error is dropped rather than failed; the deadline still applies.
		if observeErr == nil && (getErr == nil || apierrors.IsNotFound(getErr)) {
			if apierrors.IsNotFound(getErr) {
				Expect(observed.pods).To(BeEmpty(),
					"reservation %q was released while signing pods were still up (%s)", want.Name, observed)
				Expect(observed.endpoints).To(BeEmpty(),
					"reservation %q was released while Service endpoints still routed to the signing workload (%s)",
					want.Name, observed)
				return
			}
			Expect(current.UID).To(Equal(want.UID),
				"reservation %q was released and recreated during teardown", want.Name)
		}

		if !time.Now().Before(deadline) {
			Fail(fmt.Sprintf("reservation %q was not released after signing teardown (%s)", want.Name, observed))
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
