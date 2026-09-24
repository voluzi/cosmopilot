package chainnode

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	abci "github.com/cometbft/cometbft/abci/types"
	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	"github.com/cosmos/cosmos-sdk/codec/types"
	"github.com/cosmos/cosmos-sdk/crypto/keys/ed25519"
	stakingTypes "github.com/cosmos/cosmos-sdk/x/staking/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	appsv1 "github.com/voluzi/cosmopilot/v4/api/v1"
)

const testValidatorPubKeyJSON = `{"@type":"/cosmos.crypto.ed25519.PubKey","key":"oWg2ISpLF405Jcm2vXV+2v4fnjodh6aafuIdeoW+rUw="}`

type validatorQueryResult struct {
	validator *stakingTypes.Validator
	err       error
}

type fakeValidatorConfirmationClient struct {
	queries      []validatorQueryResult
	fallback     validatorQueryResult
	queryCalls   int
	txResult     *coretypes.ResultTx
	txErr        error
	txCalls      int
	queriedHash  string
	queriedAddrs []string
}

func (f *fakeValidatorConfirmationClient) QueryValidator(_ context.Context, address string) (*stakingTypes.Validator, error) {
	f.queriedAddrs = append(f.queriedAddrs, address)
	result := f.fallback
	if f.queryCalls < len(f.queries) {
		result = f.queries[f.queryCalls]
	}
	f.queryCalls++
	return result.validator, result.err
}

func (f *fakeValidatorConfirmationClient) QueryTx(_ context.Context, hash string) (*coretypes.ResultTx, error) {
	f.txCalls++
	f.queriedHash = hash
	return f.txResult, f.txErr
}

func TestConfirmValidatorSubmitsOnceAndWaitsForMatchingState(t *testing.T) {
	t.Parallel()

	address := "cosmosvaloper1validator"
	client := &fakeValidatorConfirmationClient{
		queries: []validatorQueryResult{
			{err: status.Error(codes.NotFound, "not found")},
			{err: status.Error(codes.NotFound, "not found")},
			{validator: matchingValidator(t, address)},
		},
		txErr: errors.New("transaction indexing is disabled"),
	}
	submissions := 0
	submit := func(context.Context) (string, error) {
		submissions++
		return strings.Repeat("ab", 32), nil
	}

	policy := testValidatorConfirmationPolicy()
	policy.Timeout = time.Second
	result, err := confirmValidator(t.Context(), client, address, testValidatorPubKeyJSON, submit, policy)

	require.NoError(t, err)
	assert.False(t, result.AlreadyPresent)
	assert.Equal(t, strings.Repeat("ab", 32), result.TxHash)
	assert.Equal(t, 1, submissions)
	assert.Zero(t, client.txCalls, "successful state confirmation must not depend on transaction indexing")
	assert.Equal(t, []string{address, address, address}, client.queriedAddrs)
}

func TestConfirmValidatorMatchingPreflightSkipsSubmission(t *testing.T) {
	t.Parallel()

	address := "cosmosvaloper1validator"
	client := &fakeValidatorConfirmationClient{queries: []validatorQueryResult{{validator: matchingValidator(t, address)}}}
	submissions := 0

	result, err := confirmValidator(t.Context(), client, address, testValidatorPubKeyJSON, func(context.Context) (string, error) {
		submissions++
		return "", errors.New("must not submit")
	}, testValidatorConfirmationPolicy())

	require.NoError(t, err)
	assert.True(t, result.AlreadyPresent)
	assert.Zero(t, submissions)
	assert.Zero(t, client.txCalls)
}

func TestConfirmValidatorRejectsPreflightKeyConflict(t *testing.T) {
	t.Parallel()

	address := "cosmosvaloper1validator"
	validator := matchingValidator(t, address)
	validator.ConsensusPubkey = validatorAny(t, []byte("different-validator-key-000000"))
	client := &fakeValidatorConfirmationClient{queries: []validatorQueryResult{{validator: validator}}}
	submissions := 0

	_, err := confirmValidator(t.Context(), client, address, testValidatorPubKeyJSON, func(context.Context) (string, error) {
		submissions++
		return strings.Repeat("ab", 32), nil
	}, testValidatorConfirmationPolicy())

	require.Error(t, err)
	assert.ErrorContains(t, err, "consensus public key")
	assert.Zero(t, submissions)
}

func TestConfirmValidatorDoesNotSubmitAfterPreflightFailure(t *testing.T) {
	t.Parallel()

	client := &fakeValidatorConfirmationClient{queries: []validatorQueryResult{{err: status.Error(codes.PermissionDenied, "denied")}}}
	submissions := 0

	_, err := confirmValidator(t.Context(), client, "cosmosvaloper1validator", testValidatorPubKeyJSON, func(context.Context) (string, error) {
		submissions++
		return strings.Repeat("ab", 32), nil
	}, testValidatorConfirmationPolicy())

	require.Error(t, err)
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
	assert.Zero(t, submissions)
}

func TestConfirmValidatorDoesNotPollAfterSubmissionFailure(t *testing.T) {
	t.Parallel()

	wantErr := errors.New("CheckTx rejected with code 5")
	client := &fakeValidatorConfirmationClient{
		fallback: validatorQueryResult{err: status.Error(codes.NotFound, "not found")},
	}

	_, err := confirmValidator(t.Context(), client, "cosmosvaloper1validator", testValidatorPubKeyJSON, func(context.Context) (string, error) {
		return "", wantErr
	}, testValidatorConfirmationPolicy())

	require.ErrorIs(t, err, wantErr)
	assert.Equal(t, 1, client.queryCalls)
	assert.Zero(t, client.txCalls)
}

func TestConfirmValidatorCancellationStopsPolling(t *testing.T) {
	t.Parallel()

	client := &fakeValidatorConfirmationClient{fallback: validatorQueryResult{err: status.Error(codes.NotFound, "not found")}}
	ctx, cancel := context.WithCancel(t.Context())
	submissions := 0

	_, err := confirmValidator(ctx, client, "cosmosvaloper1validator", testValidatorPubKeyJSON, func(context.Context) (string, error) {
		submissions++
		cancel()
		return strings.Repeat("ab", 32), nil
	}, validatorConfirmationPolicy{Timeout: time.Second, PollInterval: time.Millisecond, TxLookupTimeout: time.Millisecond})

	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 1, submissions)
	assert.Zero(t, client.txCalls)
}

func TestConfirmValidatorTimeoutRetainsHashWhenIndexingIsDisabled(t *testing.T) {
	t.Parallel()

	hash := strings.Repeat("ab", 32)
	client := &fakeValidatorConfirmationClient{
		fallback: validatorQueryResult{err: status.Error(codes.NotFound, "not found")},
		txErr:    errors.New("transaction indexing is disabled"),
	}

	_, err := confirmValidator(t.Context(), client, "cosmosvaloper1validator", testValidatorPubKeyJSON, func(context.Context) (string, error) {
		return hash, nil
	}, testValidatorConfirmationPolicy())

	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.ErrorContains(t, err, hash)
	assert.ErrorContains(t, err, "transaction indexing is disabled")
	assert.Equal(t, 1, client.txCalls)
	assert.Equal(t, hash, client.queriedHash)
}

func TestConfirmValidatorSurfacesIndexedExecutionFailure(t *testing.T) {
	t.Parallel()

	hash := strings.Repeat("ab", 32)
	client := &fakeValidatorConfirmationClient{
		fallback: validatorQueryResult{err: status.Error(codes.NotFound, "not found")},
		txResult: &coretypes.ResultTx{
			Height: 27,
			TxResult: abci.ResponseDeliverTx{
				Code:      11,
				Codespace: "staking",
				Log:       "insufficient self delegation",
			},
		},
	}

	_, err := confirmValidator(t.Context(), client, "cosmosvaloper1validator", testValidatorPubKeyJSON, func(context.Context) (string, error) {
		return hash, nil
	}, testValidatorConfirmationPolicy())

	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	for _, want := range []string{hash, "height 27", "code 11", "codespace staking", "insufficient self delegation"} {
		assert.ErrorContains(t, err, want)
	}
}

func TestConfirmValidatorTimeoutAfterSuccessfulIndexedExecutionIsInconsistent(t *testing.T) {
	t.Parallel()

	hash := strings.Repeat("ab", 32)
	client := &fakeValidatorConfirmationClient{
		fallback: validatorQueryResult{err: status.Error(codes.NotFound, "not found")},
		txResult: &coretypes.ResultTx{Height: 27},
	}

	_, err := confirmValidator(t.Context(), client, "cosmosvaloper1validator", testValidatorPubKeyJSON, func(context.Context) (string, error) {
		return hash, nil
	}, testValidatorConfirmationPolicy())

	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.ErrorContains(t, err, "committed successfully at height 27")
	assert.ErrorContains(t, err, "validator is absent")
}

func matchingValidator(t *testing.T, address string) *stakingTypes.Validator {
	t.Helper()
	return &stakingTypes.Validator{
		OperatorAddress: address,
		ConsensusPubkey: validatorAny(t, []byte{0xa1, 0x68, 0x36, 0x21, 0x2a, 0x4b, 0x17, 0x8d, 0x39, 0x25, 0xc9, 0xb6, 0xbd, 0x75, 0x7e, 0xda, 0xfe, 0x1f, 0x9e, 0x3a, 0x1d, 0x87, 0xa6, 0x9a, 0x7e, 0xe2, 0x1d, 0x7a, 0x85, 0xbe, 0xad, 0x4c}),
	}
}

func validatorAny(t *testing.T, key []byte) *types.Any {
	t.Helper()
	value, err := types.NewAnyWithValue(&ed25519.PubKey{Key: key})
	require.NoError(t, err)
	return value
}

func testValidatorConfirmationPolicy() validatorConfirmationPolicy {
	return validatorConfirmationPolicy{
		Timeout:         15 * time.Millisecond,
		PollInterval:    time.Millisecond,
		TxLookupTimeout: 5 * time.Millisecond,
	}
}

func TestGetValidatorStatus(t *testing.T) {
	tests := []struct {
		name   string
		status stakingTypes.BondStatus
		want   appsv1.ValidatorStatus
	}{
		{
			name:   "bonded",
			status: stakingTypes.Bonded,
			want:   appsv1.ValidatorStatusBonded,
		},
		{
			name:   "unbonding",
			status: stakingTypes.Unbonding,
			want:   appsv1.ValidatorStatusUnbonding,
		},
		{
			name:   "unbonded",
			status: stakingTypes.Unbonded,
			want:   appsv1.ValidatorStatusUnbonded,
		},
		{
			name:   "unspecified",
			status: stakingTypes.Unspecified,
			want:   appsv1.ValidatorStatusUnknown,
		},
		{
			name:   "invalid status defaults to unknown",
			status: stakingTypes.BondStatus(999),
			want:   appsv1.ValidatorStatusUnknown,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getValidatorStatus(tt.status)
			assert.Equal(t, tt.want, got)
		})
	}
}
