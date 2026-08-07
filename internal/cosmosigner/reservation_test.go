package cosmosigner

import (
	"context"
	"errors"
	"strings"
	"testing"

	appsk8sv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	appsv1 "github.com/voluzi/cosmopilot/v3/api/v1"
)

const reservationTestPublicKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
const reservationOtherPublicKey = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="

func TestEnsureConsensusKeyReservationIsAtomicAcrossOwners(t *testing.T) {
	scheme := reservationScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	first := ReservationHolder{UID: types.UID("owner-a"), Kind: "ChainNode", Namespace: "a", Name: "validator-a", Claim: "signer"}
	second := ReservationHolder{UID: types.UID("owner-b"), Kind: "ChainNode", Namespace: "b", Name: "validator-b", Claim: "signer"}

	requireReservation(t, c, c, "chain-1", reservationTestPublicKey, first)
	if err := EnsureConsensusKeyReservation(context.Background(), c, c, "chain-1", reservationTestPublicKey, second); err == nil {
		t.Fatal("a second owner must not acquire the same chain/public-key reservation")
	} else if !errors.Is(err, ErrConsensusKeyReservationConflict) {
		t.Fatalf("reservation ownership conflict must be identifiable, got %v", err)
	}

	reservation := &appsv1.ConsensusKeyReservation{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: ConsensusKeyReservationName("chain-1", reservationTestPublicKey)}, reservation); err != nil {
		t.Fatal(err)
	}
	if reservation.Spec.OwnerUID != first.UID {
		t.Fatalf("reservation owner changed: got %q want %q", reservation.Spec.OwnerUID, first.UID)
	}
}

func TestEnsureConsensusKeyReservationRejectsClaimConflictWithStaleCachedLists(t *testing.T) {
	scheme := reservationScheme(t)
	direct := fake.NewClientBuilder().WithScheme(scheme).Build()
	cached := &staleReservationListClient{Client: direct}
	holder := ReservationHolder{UID: types.UID("owner-a"), Kind: "ChainNode", Namespace: "a", Name: "validator-a", Claim: "signer"}

	requireReservation(t, direct, cached, "chain-1", reservationTestPublicKey, holder)
	err := EnsureConsensusKeyReservation(context.Background(), direct, cached, "chain-1", reservationOtherPublicKey, holder)
	if !errors.Is(err, ErrConsensusKeyReservationConflict) {
		t.Fatalf("a stale cached reservation list must not allow one claim to reserve two keys, got %v", err)
	}
}

func TestEnsureConsensusKeyReservationBlocksLegacyStatusOwner(t *testing.T) {
	scheme := reservationScheme(t)
	legacy := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "legacy", Namespace: "legacy", UID: "legacy-uid"},
		Status: appsv1.ChainNodeStatus{
			ChainID:              "chain-1",
			CosmosignerPublicKey: reservationTestPublicKey,
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(legacy).Build()
	holder := ReservationHolder{UID: types.UID("new-owner"), Kind: "ChainNodeSet", Namespace: "new", Name: "new", Claim: "signer"}

	err := EnsureConsensusKeyReservation(context.Background(), c, c, "chain-1", reservationTestPublicKey, holder)
	if err == nil {
		t.Fatal("legacy status serving the key must block reservation acquisition")
	}
	if got := err.Error(); got == "" || !containsAll(got, "legacy", "already") {
		t.Fatalf("unexpected conflict error: %v", err)
	}
}

func TestEnsureConsensusKeyReservationRejectsSiblingClaimLegacyChild(t *testing.T) {
	scheme := reservationScheme(t)
	legacy := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{
			Name: "nodes-validator-a", Namespace: "default", UID: "child-a",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNodeSet", Name: "nodes",
				UID: "nodeset-uid", Controller: reservationBoolPtr(true),
			}},
		},
		Status: appsv1.ChainNodeStatus{
			ChainID: "chain-1", PubKey: `{"key":"` + reservationTestPublicKey + `"}`,
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(legacy).Build()
	holder := ReservationHolder{
		UID: "nodeset-uid", Kind: "ChainNodeSet", Namespace: "default", Name: "nodes", Claim: "nodes-validator-b",
	}

	err := EnsureConsensusKeyReservation(context.Background(), c, c, "chain-1", reservationTestPublicKey, holder)
	if !errors.Is(err, ErrConsensusKeyReservationConflict) {
		t.Fatalf("a sibling claim must not reuse a same-root child validator key, got %v", err)
	}
}

func TestEnsureConsensusKeyReservationAllowsHAValidatorStatusAliases(t *testing.T) {
	scheme := reservationScheme(t)
	legacy := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "nodes", Namespace: "default", UID: "nodeset-uid"},
		Status: appsv1.ChainNodeSetStatus{
			ChainID: "chain-1",
			Validators: []appsv1.ChainNodeSetValidatorStatus{
				{Name: "nodes-validators-0", Group: "validators", PubKey: `{"key":"` + reservationTestPublicKey + `"}`},
				{Name: "nodes-validators-1", Group: "validators", PubKey: `{"key":"` + reservationTestPublicKey + `"}`},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(legacy).Build()
	holder := ReservationHolder{
		UID: "nodeset-uid", Kind: "ChainNodeSet", Namespace: "default", Name: "nodes", Claim: "nodes-validators-0",
		LegacyNodeNames: []string{"nodes-validators-0", "nodes-validators-1"},
	}

	if err := EnsureConsensusKeyReservation(context.Background(), c, c, "chain-1", reservationTestPublicKey, holder); err != nil {
		t.Fatalf("redundant validator endpoints in one logical signer claim must share the reservation: %v", err)
	}
}

func TestEnsureConsensusKeyReservationAllowsExactPlacementReplacementStatuses(t *testing.T) {
	scheme := reservationScheme(t)
	legacy := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "nodes", Namespace: "default", UID: "nodeset-uid"},
		Status: appsv1.ChainNodeSetStatus{
			ChainID: "chain-1",
			Cosmosigners: []appsv1.CosmosignerStatus{
				{Name: "nodes-validators-signer", PublicKey: reservationTestPublicKey},
				{Name: "nodes-signer", PublicKey: reservationTestPublicKey, ResourceName: "nodes-validators-signer"},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(legacy).Build()
	holder := ReservationHolder{
		UID: "nodeset-uid", Kind: "ChainNodeSet", Namespace: "default", Name: "nodes", Claim: "nodes-validators-0",
		LegacyStatusNames: []string{"nodes-signer", "nodes-validators-signer"},
	}

	if err := EnsureConsensusKeyReservation(context.Background(), c, c, "chain-1", reservationTestPublicKey, holder); err != nil {
		t.Fatalf("one logical signer placement replacement must share its reservation: %v", err)
	}
}

func TestEnsureConsensusKeyReservationAllowsGeneratedValidatorChildAfterSignerRollout(t *testing.T) {
	scheme := reservationScheme(t)
	set := generatedValidatorChildNodeSet("nodes", "default", "nodeset-uid", appsv1.ReservedValidatorGroupName, "nodes-validator")
	child := generatedValidatorChildNode("nodes-validator", "default", "nodeset-uid")
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&appsv1.ChainNodeSet{}).WithObjects(set, child).Build()
	holder := ReservationHolder{
		UID: "nodeset-uid", Kind: "ChainNodeSet", Namespace: "default", Name: "nodes", Claim: "nodes-validator",
	}

	// TmKMS era: the generated child claims its key under the root it is controlled by.
	requireReservation(t, c, c, "chain-1", reservationTestPublicKey, holder)

	// The top-level Cosmosigner rolls out over the same Vault key and the root records it against the
	// signer serving that validator group.
	recordCosmosignerStatus(t, c, set, appsv1.CosmosignerStatus{
		Name: "cosmosigner", PublicKey: reservationTestPublicKey, ServingGroup: appsv1.ReservedValidatorGroupName,
	})

	// The child reconciles again inside the break-before-make window, before it is stamped as a remote
	// signer target. Its holder cannot name the signer's status entry, so only the claim proves the two
	// describe one validator.
	requireReservation(t, c, c, "chain-1", reservationTestPublicKey, holder)

	reservation := &appsv1.ConsensusKeyReservation{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: ConsensusKeyReservationName("chain-1", reservationTestPublicKey)}, reservation); err != nil {
		t.Fatal(err)
	}
	if reservation.Spec.OwnerUID != holder.UID || reservation.Spec.Claim != holder.Claim {
		t.Fatalf("the generated child must keep its root-owned claim: %#v", reservation.Spec)
	}
}

func TestEnsureConsensusKeyReservationAllowsGeneratedValidatorChildInNamedGroup(t *testing.T) {
	scheme := reservationScheme(t)
	set := generatedValidatorChildNodeSet("nodes", "default", "nodeset-uid", "vals", "nodes-vals-0")
	set.Status.Cosmosigners = []appsv1.CosmosignerStatus{
		{Name: "vals-signer", PublicKey: reservationTestPublicKey, ServingGroup: "vals"},
	}
	child := generatedValidatorChildNode("nodes-vals-0", "default", "nodeset-uid")
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(set, child).Build()
	holder := ReservationHolder{
		UID: "nodeset-uid", Kind: "ChainNodeSet", Namespace: "default", Name: "nodes", Claim: "nodes-vals-0",
	}

	requireReservation(t, c, c, "chain-1", reservationTestPublicKey, holder)
}

func TestEnsureConsensusKeyReservationBlocksSiblingGroupCosmosignerStatus(t *testing.T) {
	scheme := reservationScheme(t)
	set := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "nodes", Namespace: "default", UID: "nodeset-uid"},
		Status: appsv1.ChainNodeSetStatus{
			ChainID: "chain-1",
			Cosmosigners: []appsv1.CosmosignerStatus{
				{Name: "other-signer", PublicKey: reservationTestPublicKey, ServingGroup: "other"},
			},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(set).Build()
	holder := ReservationHolder{
		UID: "nodeset-uid", Kind: "ChainNodeSet", Namespace: "default", Name: "nodes", Claim: "nodes-validator",
	}

	err := EnsureConsensusKeyReservation(context.Background(), c, c, "chain-1", reservationTestPublicKey, holder)
	if !errors.Is(err, ErrConsensusKeyReservationConflict) {
		t.Fatalf("a signer serving a sibling validator group must not share another claim's key, got %v", err)
	}
}

func TestEnsureConsensusKeyReservationBlocksCosmosignerStatusWithoutServedGroup(t *testing.T) {
	scheme := reservationScheme(t)
	set := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "nodes", Namespace: "default", UID: "nodeset-uid"},
		Status: appsv1.ChainNodeSetStatus{
			ChainID:      "chain-1",
			Cosmosigners: []appsv1.CosmosignerStatus{{Name: "sentry-signer", PublicKey: reservationTestPublicKey}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(set).Build()
	holder := ReservationHolder{
		UID: "nodeset-uid", Kind: "ChainNodeSet", Namespace: "default", Name: "nodes", Claim: "nodes-validator",
	}

	err := EnsureConsensusKeyReservation(context.Background(), c, c, "chain-1", reservationTestPublicKey, holder)
	if !errors.Is(err, ErrConsensusKeyReservationConflict) {
		t.Fatalf("a signer status with no recorded served group proves no claim and must fail closed, got %v", err)
	}
}

func TestEnsureConsensusKeyReservationBlocksForeignRootCosmosignerStatusAlias(t *testing.T) {
	scheme := reservationScheme(t)
	// Same object name in another namespace, so the alias derived from the served group collides with
	// the holder's claim and only the owner UID separates the two roots.
	set := generatedValidatorChildNodeSet("nodes", "other", "other-uid", appsv1.ReservedValidatorGroupName, "nodes-validator")
	// Isolate the serving-group alias path: no validator status may claim the key first.
	set.Status.Validators = nil
	set.Status.PubKey = ""
	set.Status.Cosmosigners = []appsv1.CosmosignerStatus{
		{Name: "cosmosigner", PublicKey: reservationTestPublicKey, ServingGroup: appsv1.ReservedValidatorGroupName},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(set).Build()
	holder := ReservationHolder{
		UID: "nodeset-uid", Kind: "ChainNodeSet", Namespace: "default", Name: "nodes", Claim: "nodes-validator",
	}

	err := EnsureConsensusKeyReservation(context.Background(), c, c, "chain-1", reservationTestPublicKey, holder)
	if !errors.Is(err, ErrConsensusKeyReservationConflict) {
		t.Fatalf("a matching generated-child name in a foreign root must not alias its signer status, got %v", err)
	}
}

func TestEnsureConsensusKeyReservationBlocksLegacySingletonAlias(t *testing.T) {
	scheme := reservationScheme(t)
	legacy := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "legacy", Namespace: "legacy", UID: "legacy-uid"},
		Status: appsv1.ChainNodeSetStatus{
			ChainID: "chain-1", PubKey: `{"key":"` + reservationTestPublicKey + `"}`,
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(legacy).Build()
	holder := ReservationHolder{UID: "new-owner", Kind: "ChainNode", Namespace: "new", Name: "new", Claim: "new"}

	err := EnsureConsensusKeyReservation(context.Background(), c, c, "chain-1", reservationTestPublicKey, holder)
	if !errors.Is(err, ErrConsensusKeyReservationConflict) {
		t.Fatalf("a legacy singleton alias must block another root from reserving its live key, got %v", err)
	}
}

func TestEnsureConsensusKeyReservationAllowsSameRootOwner(t *testing.T) {
	scheme := reservationScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	holder := ReservationHolder{UID: types.UID("nodeset-uid"), Kind: "ChainNodeSet", Namespace: "default", Name: "nodes", Claim: "validator"}
	requireReservation(t, c, c, "chain-1", reservationTestPublicKey, holder)
	requireReservation(t, c, c, "chain-1", reservationTestPublicKey, holder)
}

func TestEnsureConsensusKeyReservationRejectsConflictingSiblingWhenExactReservationExists(t *testing.T) {
	scheme := reservationScheme(t)
	holder := ReservationHolder{UID: types.UID("nodeset-uid"), Kind: "ChainNodeSet", Namespace: "default", Name: "nodes", Claim: "validator"}
	exact := &appsv1.ConsensusKeyReservation{
		ObjectMeta: metav1.ObjectMeta{Name: ConsensusKeyReservationName("chain-1", reservationTestPublicKey)},
		Spec: appsv1.ConsensusKeyReservationSpec{
			ChainID: "chain-1", PublicKey: reservationTestPublicKey, OwnerUID: holder.UID,
			OwnerKind: holder.Kind, Namespace: holder.Namespace, OwnerName: holder.Name, Claim: holder.Claim,
		},
	}
	sibling := &appsv1.ConsensusKeyReservation{
		ObjectMeta: metav1.ObjectMeta{Name: ConsensusKeyReservationName("chain-1", reservationOtherPublicKey)},
		Spec: appsv1.ConsensusKeyReservationSpec{
			ChainID: "chain-1", PublicKey: reservationOtherPublicKey, OwnerUID: holder.UID,
			OwnerKind: holder.Kind, Namespace: holder.Namespace, OwnerName: holder.Name, Claim: holder.Claim,
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(exact, sibling).Build()

	err := EnsureConsensusKeyReservation(context.Background(), c, c, "chain-1", reservationTestPublicKey, holder)
	if !errors.Is(err, ErrConsensusKeyReservationConflict) {
		t.Fatalf("an exact reservation must not hide a sibling reservation for the same claim, got %v", err)
	}
}

func TestEnsureConsensusKeyReservationRejectsDifferentClaimWithinSameRoot(t *testing.T) {
	scheme := reservationScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	first := ReservationHolder{UID: types.UID("nodeset-uid"), Kind: "ChainNodeSet", Namespace: "default", Name: "nodes", Claim: "validator-a"}
	second := first
	second.Claim = "validator-b"
	requireReservation(t, c, c, "chain-1", reservationTestPublicKey, first)

	err := EnsureConsensusKeyReservation(context.Background(), c, c, "chain-1", reservationTestPublicKey, second)
	if !errors.Is(err, ErrConsensusKeyReservationConflict) {
		t.Fatalf("independent claims in one root must not share slash-protection state, got %v", err)
	}
}

func TestEnsureConsensusKeyReservationRejectsDifferentKeyForSameClaim(t *testing.T) {
	scheme := reservationScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	holder := ReservationHolder{UID: types.UID("nodeset-uid"), Kind: "ChainNodeSet", Namespace: "default", Name: "nodes", Claim: "validator-a"}
	requireReservation(t, c, c, "chain-1", reservationTestPublicKey, holder)

	err := EnsureConsensusKeyReservation(context.Background(), c, c, "chain-1", reservationOtherPublicKey, holder)
	if !errors.Is(err, ErrConsensusKeyReservationConflict) {
		t.Fatalf("a logical validator with an older reservation must not claim another key, got %v", err)
	}
}

func TestEnsureConsensusKeyReservationRejectsInconsistentExistingObject(t *testing.T) {
	scheme := reservationScheme(t)
	holder := ReservationHolder{UID: types.UID("nodeset-uid"), Kind: "ChainNodeSet", Namespace: "default", Name: "nodes", Claim: "validator"}
	reservation := &appsv1.ConsensusKeyReservation{
		ObjectMeta: metav1.ObjectMeta{Name: ConsensusKeyReservationName("chain-1", reservationTestPublicKey)},
		Spec: appsv1.ConsensusKeyReservationSpec{
			ChainID: "different-chain", PublicKey: reservationTestPublicKey, OwnerUID: holder.UID,
			OwnerKind: holder.Kind, Namespace: holder.Namespace, OwnerName: holder.Name, Claim: holder.Claim,
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(reservation).Build()

	if err := EnsureConsensusKeyReservation(context.Background(), c, c, "chain-1", reservationTestPublicKey, holder); err == nil {
		t.Fatal("an existing reservation with mismatched identity fields must fail closed")
	}
}

func reservationScheme(t *testing.T) *runtime.Scheme {
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

func requireReservation(t *testing.T, reader client.Reader, writer client.Writer, chainID, publicKey string, holder ReservationHolder) {
	t.Helper()
	if err := EnsureConsensusKeyReservation(context.Background(), reader, writer, chainID, publicKey, holder); err != nil {
		t.Fatal(err)
	}
}

// generatedValidatorChildNodeSet builds a ChainNodeSet whose recorded validator is the child
// ChainNode it generates for one validator group, as the set looks once that validator is live.
func generatedValidatorChildNodeSet(name, namespace string, uid types.UID, group, childName string) *appsv1.ChainNodeSet {
	set := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, UID: uid},
		Status: appsv1.ChainNodeSetStatus{
			ChainID: "chain-1",
			Validators: []appsv1.ChainNodeSetValidatorStatus{
				{Name: childName, Group: group, PubKey: `{"key":"` + reservationTestPublicKey + `"}`},
			},
		},
	}
	if group == appsv1.ReservedValidatorGroupName {
		set.Status.PubKey = `{"key":"` + reservationTestPublicKey + `"}`
	}
	return set
}

func generatedValidatorChildNode(name, namespace string, root types.UID) *appsv1.ChainNode {
	return &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: namespace, UID: types.UID(name + "-uid"),
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: appsv1.GroupVersion.String(), Kind: "ChainNodeSet", Name: "nodes",
				UID: root, Controller: reservationBoolPtr(true),
			}},
		},
		Status: appsv1.ChainNodeStatus{ChainID: "chain-1", PubKey: `{"key":"` + reservationTestPublicKey + `"}`},
	}
}

func recordCosmosignerStatus(t *testing.T, c client.Client, set *appsv1.ChainNodeSet, status appsv1.CosmosignerStatus) {
	t.Helper()
	live := &appsv1.ChainNodeSet{}
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(set), live); err != nil {
		t.Fatal(err)
	}
	live.Status.Cosmosigners = append(live.Status.Cosmosigners, status)
	if err := c.Status().Update(context.Background(), live); err != nil {
		t.Fatal(err)
	}
}

func containsAll(value string, values ...string) bool {
	for _, candidate := range values {
		if !strings.Contains(value, candidate) {
			return false
		}
	}
	return true
}

type staleReservationListClient struct {
	client.Client
}

func (c *staleReservationListClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	if reservations, ok := list.(*appsv1.ConsensusKeyReservationList); ok {
		reservations.Items = nil
		return nil
	}
	return c.Client.List(ctx, list, opts...)
}

func reservationBoolPtr(value bool) *bool { return &value }
