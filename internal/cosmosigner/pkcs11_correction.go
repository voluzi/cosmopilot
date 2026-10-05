package cosmosigner

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1 "github.com/voluzi/cosmopilot/v5/api/v1"
	"github.com/voluzi/cosmopilot/v5/pkg/utils"
)

// PrepareInitialPKCS11KeyCorrection releases a mistaken first-deploy pin only after its signing
// path stops. Status loss after rollout must not authorize release, and Raft state stays untouched.
func PrepareInitialPKCS11KeyCorrection(ctx context.Context, reader client.Reader, c client.Client, owner client.Object, params Params, holder ReservationHolder, recorded bool) (bool, error) {
	if params.Backend.PKCS11 == nil || recorded {
		return false, nil
	}
	sts, live, err := liveSigningConfig(ctx, reader, owner, params.Namespace, params.Name)
	if err != nil || live == nil || live.ExpectedPublicKey == params.ExpectedPublicKey {
		return false, err
	}
	if live.Backend.Type != backendPKCS11 || statefulSetEverRolledOut(sts) {
		return false, fmt.Errorf("%w: cosmosigner %q cannot correct its initial public key after rollout", ErrRecoveredIdentityMismatch, params.Name)
	}
	quiesced, err := ScaleDown(ctx, c, owner, params.Namespace, params.Name)
	if err != nil {
		return false, err
	}
	if quiesced {
		quiesced, err = SignerPodsGone(ctx, reader, params.Namespace, params.Name)
		if err != nil {
			return false, err
		}
	}
	if !quiesced {
		return false, fmt.Errorf("cosmosigner %q is stopping before correcting its initial PKCS#11 public key", params.Name)
	}
	reservation := &appsv1.ConsensusKeyReservation{}
	err = reader.Get(ctx, client.ObjectKey{Name: ConsensusKeyReservationName(params.ChainID, live.ExpectedPublicKey)}, reservation)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if holder.Claim == "signer-"+utils.Sha256("pkcs11\x00"+params.ExpectedPublicKey) {
		holder.Claim = "signer-" + utils.Sha256("pkcs11\x00"+live.ExpectedPublicKey)
	}
	if err := reservationOwnedBy(reservation, params.ChainID, live.ExpectedPublicKey, holder); err != nil {
		return false, err
	}
	released, err := ReleaseConsensusKeyReservationClaim(ctx, reader, c, owner, reservation)
	if err != nil {
		return false, err
	}
	if !released {
		return false, fmt.Errorf("cosmosigner %q is waiting for its initial PKCS#11 reservation release", params.Name)
	}
	return true, nil
}
