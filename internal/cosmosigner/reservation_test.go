package cosmosigner

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
)

const reservationTestPublicKey = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
const reservationOtherPublicKey = "AQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQEBAQE="

func TestEnsureConsensusKeyReservationIsAtomicAcrossOwners(t *testing.T) {
	scheme := reservationScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	first := ReservationHolder{UID: types.UID("owner-a"), Kind: "ChainNode", Namespace: "a", Name: "validator-a", Claim: "signer"}
	second := ReservationHolder{UID: types.UID("owner-b"), Kind: "ChainNode", Namespace: "b", Name: "validator-b", Claim: "signer"}

	requireReservation(t, c, "chain-1", reservationTestPublicKey, first)
	if err := EnsureConsensusKeyReservation(context.Background(), c, "chain-1", reservationTestPublicKey, second); err == nil {
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

	err := EnsureConsensusKeyReservation(context.Background(), c, "chain-1", reservationTestPublicKey, holder)
	if err == nil {
		t.Fatal("legacy status serving the key must block reservation acquisition")
	}
	if got := err.Error(); got == "" || !containsAll(got, "legacy", "already") {
		t.Fatalf("unexpected conflict error: %v", err)
	}
}

func TestEnsureConsensusKeyReservationAllowsSameRootOwner(t *testing.T) {
	scheme := reservationScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	holder := ReservationHolder{UID: types.UID("nodeset-uid"), Kind: "ChainNodeSet", Namespace: "default", Name: "nodes", Claim: "validator"}
	requireReservation(t, c, "chain-1", reservationTestPublicKey, holder)
	requireReservation(t, c, "chain-1", reservationTestPublicKey, holder)
}

func TestEnsureConsensusKeyReservationRejectsDifferentClaimWithinSameRoot(t *testing.T) {
	scheme := reservationScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	first := ReservationHolder{UID: types.UID("nodeset-uid"), Kind: "ChainNodeSet", Namespace: "default", Name: "nodes", Claim: "validator-a"}
	second := first
	second.Claim = "validator-b"
	requireReservation(t, c, "chain-1", reservationTestPublicKey, first)

	err := EnsureConsensusKeyReservation(context.Background(), c, "chain-1", reservationTestPublicKey, second)
	if !errors.Is(err, ErrConsensusKeyReservationConflict) {
		t.Fatalf("independent claims in one root must not share slash-protection state, got %v", err)
	}
}

func TestEnsureConsensusKeyReservationRejectsDifferentKeyForSameClaim(t *testing.T) {
	scheme := reservationScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	holder := ReservationHolder{UID: types.UID("nodeset-uid"), Kind: "ChainNodeSet", Namespace: "default", Name: "nodes", Claim: "validator-a"}
	requireReservation(t, c, "chain-1", reservationTestPublicKey, holder)

	err := EnsureConsensusKeyReservation(context.Background(), c, "chain-1", reservationOtherPublicKey, holder)
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

	if err := EnsureConsensusKeyReservation(context.Background(), c, "chain-1", reservationTestPublicKey, holder); err == nil {
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
	return scheme
}

func requireReservation(t *testing.T, c client.Client, chainID, publicKey string, holder ReservationHolder) {
	t.Helper()
	if err := EnsureConsensusKeyReservation(context.Background(), c, chainID, publicKey, holder); err != nil {
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
