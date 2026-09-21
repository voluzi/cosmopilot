package chainnode

import (
	"bytes"
	"context"
	"fmt"
	"time"

	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	"github.com/cosmos/cosmos-sdk/codec"
	codecTypes "github.com/cosmos/cosmos-sdk/codec/types"
	cryptoCodec "github.com/cosmos/cosmos-sdk/crypto/codec"
	cryptoTypes "github.com/cosmos/cosmos-sdk/crypto/types"
	stakingTypes "github.com/cosmos/cosmos-sdk/x/staking/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/log"

	appsv1 "github.com/voluzi/cosmopilot/v3/api/v1"
	"github.com/voluzi/cosmopilot/v3/internal/chainutils"
	"github.com/voluzi/cosmopilot/v3/internal/cometbft"
)

type validatorConfirmationClient interface {
	QueryValidator(context.Context, string) (*stakingTypes.Validator, error)
	QueryTx(context.Context, string) (*coretypes.ResultTx, error)
}

type validatorConfirmationPolicy struct {
	Timeout         time.Duration
	PollInterval    time.Duration
	TxLookupTimeout time.Duration
}

type validatorConfirmationResult struct {
	TxHash         string
	AlreadyPresent bool
}

var defaultValidatorConfirmationPolicy = validatorConfirmationPolicy{
	Timeout:         time.Minute,
	PollInterval:    time.Second,
	TxLookupTimeout: 5 * time.Second,
}

func (r *Reconciler) createValidator(ctx context.Context, app *chainutils.App, chainNode *appsv1.ChainNode) error {
	logger := log.FromContext(ctx)

	logger.Info("ensuring validator exists on-chain")
	params := &chainutils.Params{
		ChainID:                 chainNode.Status.ChainID,
		StakeAmount:             chainNode.Spec.Validator.CreateValidator.StakeAmount,
		CommissionMaxChangeRate: chainNode.Spec.Validator.GetCommissionMaxChangeRate(),
		CommissionMaxRate:       chainNode.Spec.Validator.GetCommissionMaxRate(),
		CommissionRate:          chainNode.Spec.Validator.GetCommissionRate(),
		MinSelfDelegation:       chainNode.Spec.Validator.GetMinSelfDelegation(),
		GasPrices:               chainNode.Spec.Validator.CreateValidator.GasPrices,
	}

	accountSecret := &corev1.Secret{}
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: chainNode.GetNamespace(),
		Name:      chainNode.Spec.Validator.GetAccountSecretName(chainNode),
	}, accountSecret); err != nil {
		return err
	}

	account, err := chainutils.AccountFromMnemonic(
		string(accountSecret.Data[MnemonicKey]),
		chainNode.Spec.Validator.GetAccountPrefix(),
		chainNode.Spec.Validator.GetValPrefix(),
		chainNode.Spec.Validator.GetAccountHDPath(),
	)
	if err != nil {
		return err
	}
	client, err := r.getChainNodeClient(chainNode)
	if err != nil {
		return err
	}

	// Gather validator info
	nodeInfo := &chainutils.NodeInfo{}
	nodeInfo.Moniker = chainNode.GetMoniker()
	if chainNode.Spec.Validator.Info != nil {
		nodeInfo.Details = chainNode.Spec.Validator.Info.Details
		nodeInfo.Website = chainNode.Spec.Validator.Info.Website
		nodeInfo.Identity = chainNode.Spec.Validator.Info.Identity
	}

	result, err := confirmValidator(
		ctx,
		client,
		account.ValidatorAddress,
		chainNode.Status.PubKey,
		func(ctx context.Context) (string, error) {
			return app.CreateValidator(ctx,
				chainNode.Status.PubKey,
				account,
				nodeInfo,
				params,
				fmt.Sprintf("tcp://%s:%d", chainNode.GetNodeFQDN(), chainutils.RpcPort),
			)
		},
		defaultValidatorConfirmationPolicy,
	)
	if err != nil {
		r.recorder.Eventf(chainNode,
			corev1.EventTypeWarning,
			appsv1.ReasonCreateValidatorFailure,
			"failed to create-validator: %s", err.Error())
		return err
	}

	message := fmt.Sprintf("create-validator transaction %s confirmed on-chain", result.TxHash)
	if result.AlreadyPresent {
		message = "validator was already present and confirmed on-chain"
	}
	r.recorder.Event(chainNode,
		corev1.EventTypeNormal,
		appsv1.ReasonCreateValidatorSuccess,
		message)
	return nil
}

func confirmValidator(
	ctx context.Context,
	client validatorConfirmationClient,
	operatorAddress string,
	expectedPubKeyJSON string,
	submit func(context.Context) (string, error),
	policy validatorConfirmationPolicy,
) (validatorConfirmationResult, error) {
	expectedPubKey, err := decodeValidatorPubKey(expectedPubKeyJSON)
	if err != nil {
		return validatorConfirmationResult{}, err
	}

	validator, err := client.QueryValidator(ctx, operatorAddress)
	if err == nil {
		if err := validateValidatorIdentity(validator, operatorAddress, expectedPubKey); err != nil {
			return validatorConfirmationResult{}, err
		}
		return validatorConfirmationResult{AlreadyPresent: true}, nil
	}
	if status.Code(err) != codes.NotFound {
		return validatorConfirmationResult{}, fmt.Errorf("preflight validator query: %w", err)
	}

	txHash, err := submit(ctx)
	if err != nil {
		return validatorConfirmationResult{}, err
	}

	confirmationCtx, cancel := context.WithTimeout(ctx, policy.Timeout)
	defer cancel()
	lastQueryErr := err
	for {
		validator, queryErr := client.QueryValidator(confirmationCtx, operatorAddress)
		if queryErr == nil {
			if err := validateValidatorIdentity(validator, operatorAddress, expectedPubKey); err != nil {
				return validatorConfirmationResult{}, err
			}
			return validatorConfirmationResult{TxHash: txHash}, nil
		}
		lastQueryErr = queryErr
		if confirmationCtx.Err() != nil {
			break
		}
		if !isRetryableValidatorQueryError(queryErr) {
			return validatorConfirmationResult{}, fmt.Errorf("confirming validator %s: %w", operatorAddress, queryErr)
		}

		timer := time.NewTimer(policy.PollInterval)
		select {
		case <-confirmationCtx.Done():
			timer.Stop()
		case <-timer.C:
			continue
		}
		break
	}

	if err := ctx.Err(); err != nil {
		return validatorConfirmationResult{}, fmt.Errorf("confirming validator %s after transaction %s: %w", operatorAddress, txHash, err)
	}
	confirmationErr := fmt.Errorf(
		"confirming validator %s after transaction %s; last query: %v: %w",
		operatorAddress,
		txHash,
		lastQueryErr,
		context.DeadlineExceeded,
	)
	return validatorConfirmationResult{}, diagnoseValidatorConfirmationFailure(ctx, client, txHash, policy.TxLookupTimeout, confirmationErr)
}

func decodeValidatorPubKey(value string) (cryptoTypes.PubKey, error) {
	registry := codecTypes.NewInterfaceRegistry()
	cryptoCodec.RegisterInterfaces(registry)
	protoCodec := codec.NewProtoCodec(registry)
	var pubKey cryptoTypes.PubKey
	if err := protoCodec.UnmarshalInterfaceJSON([]byte(value), &pubKey); err != nil {
		return nil, fmt.Errorf("decode expected validator consensus public key: %w", err)
	}
	if pubKey == nil {
		return nil, fmt.Errorf("decode expected validator consensus public key: key is required")
	}
	return pubKey, nil
}

func validateValidatorIdentity(validator *stakingTypes.Validator, operatorAddress string, expectedPubKey cryptoTypes.PubKey) error {
	if validator == nil {
		return fmt.Errorf("validator query for %s returned no validator", operatorAddress)
	}
	if validator.OperatorAddress != operatorAddress {
		return fmt.Errorf("validator query for %s returned operator address %s", operatorAddress, validator.OperatorAddress)
	}
	if validator.ConsensusPubkey == nil {
		return fmt.Errorf("validator %s consensus public key is required", operatorAddress)
	}
	actualPubKey, err := cometbft.UnpackPubKey(validator.ConsensusPubkey)
	if err != nil {
		return fmt.Errorf("decode validator %s consensus public key: %w", operatorAddress, err)
	}
	if actualPubKey == nil {
		return fmt.Errorf("validator %s consensus public key is required", operatorAddress)
	}
	if actualPubKey.Type() != expectedPubKey.Type() || !bytes.Equal(actualPubKey.Bytes(), expectedPubKey.Bytes()) {
		return fmt.Errorf("validator %s consensus public key does not match the requested key", operatorAddress)
	}
	return nil
}

func isRetryableValidatorQueryError(err error) bool {
	switch status.Code(err) {
	case codes.NotFound, codes.Unavailable, codes.ResourceExhausted, codes.Aborted, codes.DeadlineExceeded:
		return true
	default:
		return false
	}
}

func diagnoseValidatorConfirmationFailure(
	ctx context.Context,
	client validatorConfirmationClient,
	txHash string,
	timeout time.Duration,
	confirmationErr error,
) error {
	diagnosticCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	result, err := client.QueryTx(diagnosticCtx, txHash)
	if err != nil {
		return fmt.Errorf("transaction %s diagnostics unavailable: %v: %w", txHash, err, confirmationErr)
	}
	if result == nil {
		return fmt.Errorf("transaction %s diagnostics returned no result: %w", txHash, confirmationErr)
	}
	if result.TxResult.Code != 0 {
		return fmt.Errorf(
			"transaction %s failed at height %d with code %d, codespace %s: %s: %w",
			txHash,
			result.Height,
			result.TxResult.Code,
			result.TxResult.Codespace,
			result.TxResult.Log,
			confirmationErr,
		)
	}
	return fmt.Errorf(
		"transaction %s committed successfully at height %d but validator is absent: %w",
		txHash,
		result.Height,
		confirmationErr,
	)
}

func (r *Reconciler) updateValidatorStatus(ctx context.Context, chainNode *appsv1.ChainNode) error {
	logger := log.FromContext(ctx)

	client, err := r.getChainNodeClient(chainNode)
	if err != nil {
		return err
	}

	var validator *stakingTypes.Validator
	if chainNode.Status.ValidatorAddress == "" {
		status, err := client.GetNodeStatus(ctx)
		if err != nil {
			return err
		}

		validators, err := client.GetValidators(ctx)
		if err != nil {
			return err
		}

		found := false
		for _, val := range validators {
			pk, err := cometbft.UnpackPubKey(val.ConsensusPubkey)
			if err != nil {
				return err
			}
			if bytes.Equal(status.ValidatorInfo.PubKey.Address().Bytes(), pk.Address().Bytes()) {
				chainNode.Status.ValidatorAddress = val.OperatorAddress
				validator = &val
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("validator not found")
		}
	} else {
		validator, err = client.QueryValidator(ctx, chainNode.Status.ValidatorAddress)
		if err != nil {
			return err
		}
	}

	pk, err := cometbft.UnpackPubKey(validator.ConsensusPubkey)
	if err != nil {
		return err
	}

	pkStr, err := cometbft.PubKeyToString(pk)
	if err != nil {
		return err
	}

	accountAddr, err := chainutils.AccountAddressFromValidatorAddress(validator.OperatorAddress,
		chainNode.Spec.Validator.GetValPrefix(),
		chainNode.Spec.Validator.GetAccountPrefix(),
	)
	if err != nil {
		return err
	}

	validatorStatus := getValidatorStatus(validator.Status)

	if !chainNode.Status.Validator ||
		chainNode.Status.ValidatorAddress == "" ||
		chainNode.Status.ValidatorStatus != validatorStatus ||
		chainNode.Status.AccountAddress != accountAddr ||
		chainNode.Status.ValidatorAddress != validator.OperatorAddress ||
		chainNode.Status.Jailed != validator.Jailed ||
		chainNode.Status.PubKey != pkStr {
		if chainNode.Status.Jailed != validator.Jailed {
			logger.Info("updating jailed status", "jailed", validator.Jailed)

			if validator.Jailed {
				r.recorder.Eventf(chainNode,
					corev1.EventTypeWarning,
					appsv1.ReasonValidatorJailed,
					"Validator is jailed",
				)
			} else {
				r.recorder.Eventf(chainNode,
					corev1.EventTypeNormal,
					appsv1.ReasonValidatorUnjailed,
					"Validator was successfully unjailed",
				)
			}
		}
		chainNode.Status.ValidatorAddress = validator.OperatorAddress
		chainNode.Status.AccountAddress = accountAddr
		chainNode.Status.Jailed = validator.Jailed
		chainNode.Status.ValidatorStatus = validatorStatus
		chainNode.Status.PubKey = pkStr
		chainNode.Status.Validator = true
		return r.Status().Update(ctx, chainNode)
	}

	return nil
}

func getValidatorStatus(status stakingTypes.BondStatus) appsv1.ValidatorStatus {
	switch status {
	case stakingTypes.Bonded:
		return appsv1.ValidatorStatusBonded
	case stakingTypes.Unbonding:
		return appsv1.ValidatorStatusUnbonding
	case stakingTypes.Unbonded:
		return appsv1.ValidatorStatusUnbonded
	case stakingTypes.Unspecified:
		fallthrough
	default:
		return appsv1.ValidatorStatusUnknown
	}
}
