package cosmosigner

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1 "github.com/voluzi/cosmopilot/v3/api/v1"
	"github.com/voluzi/cosmopilot/v3/pkg/utils"
)

// ErrConsensusKeyReservationConflict marks an ownership or identity conflict that means another
// signing path may already control the requested chain/public-key pair.
var ErrConsensusKeyReservationConflict = errors.New("consensus key reservation conflict")

// ReservationHolder identifies the root CR and logical signer claiming a consensus key.
type ReservationHolder struct {
	UID               types.UID
	Kind              string
	Namespace         string
	Name              string
	Claim             string
	LegacyStatusNames []string
	LegacyNodeNames   []string
}

// ConsensusKeyReservationName returns the cluster-scoped name for a chain/public-key pair.
func ConsensusKeyReservationName(chainID, publicKey string) string {
	return "consensus-key-" + utils.Sha256(chainID + "\x00" + publicKey)[:48]
}

// EnsureConsensusKeyReservation atomically claims a consensus key for one controller root. The
// reader must bypass the controller cache so conflict scans around the create observe one ordered
// API-server history even when concurrent claims use different reservation names.
func EnsureConsensusKeyReservation(ctx context.Context, reader client.Reader, writer client.Writer, chainID, publicKey string, holder ReservationHolder) error {
	_, err := EnsureConsensusKeyReservationWithResult(ctx, reader, writer, chainID, publicKey, holder)
	return err
}

// ConsensusKeyReservationEnsureResult reports a stale reservation replaced during acquisition.
type ConsensusKeyReservationEnsureResult struct {
	RecoveredReservation string
}

// EnsureConsensusKeyReservationWithResult claims a consensus key and reports safe stale-owner
// recovery so controllers can surface it as an event.
func EnsureConsensusKeyReservationWithResult(ctx context.Context, reader client.Reader, writer client.Writer, chainID, publicKey string, holder ReservationHolder) (ConsensusKeyReservationEnsureResult, error) {
	result := ConsensusKeyReservationEnsureResult{}
	if holder.UID == "" || chainID == "" || holder.Claim == "" {
		return result, fmt.Errorf("consensus-key reservation requires owner UID, chain ID, and claim")
	}
	if err := validateCanonicalPublicKey(publicKey); err != nil {
		return result, err
	}
	name := ConsensusKeyReservationName(chainID, publicKey)
	for {
		existing := &appsv1.ConsensusKeyReservation{}
		if err := reader.Get(ctx, client.ObjectKey{Name: name}, existing); err == nil {
			if err := reservationOwnedBy(existing, chainID, publicKey, holder); err != nil {
				recovered, recoveryErr := recoverStaleConsensusKeyReservation(ctx, reader, writer, existing, chainID, publicKey, holder)
				if recoveryErr != nil {
					return result, recoveryErr
				}
				if recovered {
					result.RecoveredReservation = existing.GetName()
					continue
				}
				return result, err
			}
			if err := ensureNoConflictingReservationClaim(ctx, reader, chainID, publicKey, holder); err != nil {
				return result, err
			}
			return result, ensureNoLegacyConsensusKeyOwner(ctx, reader, chainID, publicKey, holder)
		} else if !apierrors.IsNotFound(err) {
			return result, err
		}
		if err := ensureNoConflictingReservationClaim(ctx, reader, chainID, publicKey, holder); err != nil {
			return result, err
		}
		if err := ensureNoLegacyConsensusKeyOwner(ctx, reader, chainID, publicKey, holder); err != nil {
			return result, err
		}
		reservation := &appsv1.ConsensusKeyReservation{
			ObjectMeta: metav1.ObjectMeta{Name: name},
			Spec: appsv1.ConsensusKeyReservationSpec{
				ChainID: chainID, PublicKey: publicKey, OwnerUID: holder.UID,
				OwnerKind: holder.Kind, Namespace: holder.Namespace, OwnerName: holder.Name, Claim: holder.Claim,
			},
		}
		if err := writer.Create(ctx, reservation); err == nil {
			if err := ensureNoConflictingReservationClaim(ctx, reader, chainID, publicKey, holder); err != nil {
				return result, err
			}
			return result, ensureNoLegacyConsensusKeyOwner(ctx, reader, chainID, publicKey, holder)
		} else if !apierrors.IsAlreadyExists(err) {
			return result, err
		}
	}
}

func reservationOwnedBy(reservation *appsv1.ConsensusKeyReservation, chainID, publicKey string, holder ReservationHolder) error {
	if reservation.Spec.ChainID != chainID || reservation.Spec.PublicKey != publicKey ||
		reservation.GetName() != ConsensusKeyReservationName(chainID, publicKey) {
		return fmt.Errorf("%w: reservation %q has inconsistent immutable identity fields", ErrConsensusKeyReservationConflict, reservation.GetName())
	}
	if reservation.Spec.OwnerUID == holder.UID && reservation.Spec.Claim == holder.Claim {
		return nil
	}
	if reservation.Spec.OwnerUID == holder.UID {
		return fmt.Errorf("%w: consensus key on chain %q is already reserved for independent claim %q in %s %s/%s",
			ErrConsensusKeyReservationConflict, reservation.Spec.ChainID, reservation.Spec.Claim,
			reservation.Spec.OwnerKind, reservation.Spec.Namespace, reservation.Spec.OwnerName)
	}
	return fmt.Errorf("%w: consensus key on chain %q is already reserved by %s %s/%s (UID %s)",
		ErrConsensusKeyReservationConflict,
		reservation.Spec.ChainID, reservation.Spec.OwnerKind, reservation.Spec.Namespace,
		reservation.Spec.OwnerName, reservation.Spec.OwnerUID)
}

func ensureNoConflictingReservationClaim(ctx context.Context, reader client.Reader, chainID, publicKey string, holder ReservationHolder) error {
	reservations := &appsv1.ConsensusKeyReservationList{}
	if err := reader.List(ctx, reservations); err != nil {
		return err
	}
	for i := range reservations.Items {
		reservation := &reservations.Items[i]
		if reservation.Spec.OwnerUID != holder.UID || reservation.Spec.ChainID != chainID || reservation.Spec.Claim != holder.Claim {
			continue
		}
		if reservation.Spec.PublicKey == publicKey && reservation.GetName() == ConsensusKeyReservationName(chainID, publicKey) {
			continue
		}
		return fmt.Errorf("%w: claim %q in %s %s/%s already reserves another consensus key on chain %q",
			ErrConsensusKeyReservationConflict, holder.Claim, holder.Kind, holder.Namespace, holder.Name, chainID)
	}
	return nil
}

func ensureNoLegacyConsensusKeyOwner(ctx context.Context, reader client.Reader, chainID, publicKey string, holder ReservationHolder) error {
	nodes := &appsv1.ChainNodeList{}
	if err := reader.List(ctx, nodes); err != nil {
		return err
	}
	for i := range nodes.Items {
		node := &nodes.Items[i]
		if node.Status.ChainID != chainID || legacyChainNodeMatchesClaim(node, holder) {
			continue
		}
		if node.Status.CosmosignerPublicKey == publicKey || canonicalSDKPublicKey(node.Status.PubKey) == publicKey {
			return fmt.Errorf("%w: consensus key on chain %q is already recorded by ChainNode %s/%s",
				ErrConsensusKeyReservationConflict, chainID, node.Namespace, node.Name)
		}
	}

	sets := &appsv1.ChainNodeSetList{}
	if err := reader.List(ctx, sets); err != nil {
		return err
	}
	for i := range sets.Items {
		set := &sets.Items[i]
		if set.Status.ChainID != chainID {
			continue
		}
		for _, signer := range set.Status.Cosmosigners {
			if signer.PublicKey == publicKey {
				if set.UID == holder.UID &&
					(holder.matchesLegacyStatus(signer.Name) || legacyCosmosignerStatusMatchesClaim(set, signer.ServingGroup, holder.Claim)) {
					continue
				}
				return fmt.Errorf("%w: consensus key on chain %q is already recorded by ChainNodeSet %s/%s",
					ErrConsensusKeyReservationConflict, chainID, set.Namespace, set.Name)
			}
		}
		if canonicalSDKPublicKey(set.Status.PubKey) == publicKey &&
			!legacyChainNodeSetAliasMatchesClaim(set, holder, publicKey) {
			return fmt.Errorf("%w: consensus key on chain %q is already recorded by ChainNodeSet %s/%s",
				ErrConsensusKeyReservationConflict, chainID, set.Namespace, set.Name)
		}
		for _, validator := range set.Status.Validators {
			if canonicalSDKPublicKey(validator.PubKey) == publicKey {
				if set.UID == holder.UID && (validator.Name == holder.Claim || holder.matchesLegacyNode(validator.Name)) {
					continue
				}
				return fmt.Errorf("%w: consensus key on chain %q is already recorded by ChainNodeSet %s/%s",
					ErrConsensusKeyReservationConflict, chainID, set.Namespace, set.Name)
			}
		}
	}
	return nil
}

func (holder ReservationHolder) matchesLegacyStatus(name string) bool {
	for _, allowed := range holder.LegacyStatusNames {
		if name == allowed {
			return true
		}
	}
	return false
}

func (holder ReservationHolder) matchesLegacyNode(name string) bool {
	for _, allowed := range holder.LegacyNodeNames {
		if name == allowed {
			return true
		}
	}
	return false
}

func legacyChainNodeMatchesClaim(node *appsv1.ChainNode, holder ReservationHolder) bool {
	if node.GetUID() == holder.UID {
		return true
	}
	return belongsToRoot(node, holder.UID) &&
		(node.GetName() == holder.Claim || holder.matchesLegacyNode(node.GetName()))
}

// legacyCosmosignerStatusMatchesClaim reports whether a managed signer recorded in a ChainNodeSet's
// status protects exactly the consensus identity the holder is claiming. It is only consulted for a
// same-root status entry (the caller compares owner UIDs), so it never relaxes cross-root conflicts.
//
// A ChainNodeSet-generated validator child acquires its reservation under the root's UID with its own
// node name as the claim, while the root records the signer serving it under the signer's own name —
// which the child cannot enumerate in LegacyStatusNames. The recorded ServingGroup resolves back to
// the same generated instance-0 child name the root's signer preflight uses as its claim, so exact
// equality with the holder's claim proves both name one logical validator. An entry with no recorded
// ServingGroup (a sentry signer, or one whose served group predates the field) proves nothing and
// fails closed.
func legacyCosmosignerStatusMatchesClaim(set *appsv1.ChainNodeSet, servingGroup, claim string) bool {
	return servingGroup != "" && set.GeneratedValidatorNodeName(servingGroup, 0) == claim
}

func legacyChainNodeSetAliasMatchesClaim(set *appsv1.ChainNodeSet, holder ReservationHolder, publicKey string) bool {
	if set.GetUID() != holder.UID {
		return false
	}
	for _, validator := range set.Status.Validators {
		if validator.Name == holder.Claim && canonicalSDKPublicKey(validator.PubKey) == publicKey {
			return true
		}
	}
	return len(set.Status.Validators) == 0 && holder.Claim == set.GetName()+"-validator"
}

func belongsToRoot(obj metav1.Object, uid types.UID) bool {
	if obj.GetUID() == uid {
		return true
	}
	for _, owner := range obj.GetOwnerReferences() {
		if owner.UID == uid {
			return true
		}
	}
	return false
}

func validateCanonicalPublicKey(publicKey string) error {
	raw, err := base64.StdEncoding.DecodeString(publicKey)
	if err != nil || len(raw) != 32 || base64.StdEncoding.EncodeToString(raw) != publicKey {
		return fmt.Errorf("consensus public key must be canonical base64-encoded Ed25519 key material")
	}
	return nil
}

func canonicalSDKPublicKey(value string) string {
	var key struct {
		Key string `json:"key"`
	}
	if json.Unmarshal([]byte(value), &key) != nil || validateCanonicalPublicKey(key.Key) != nil {
		return ""
	}
	return key.Key
}

// CanonicalSDKPublicKey extracts canonical base64 Ed25519 material from the Cosmos SDK public-key
// JSON stored in validator status. It returns an empty string for an absent or malformed value.
func CanonicalSDKPublicKey(value string) string {
	return canonicalSDKPublicKey(value)
}
