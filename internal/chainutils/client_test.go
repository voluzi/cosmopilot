package chainutils

import (
	"context"
	"errors"
	"strings"
	"testing"

	coretypes "github.com/cometbft/cometbft/rpc/core/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fakeCometRPCClient struct {
	txResult *coretypes.ResultTx
	txErr    error
	txHash   []byte
	txProve  bool
	txCalls  int
}

func (f *fakeCometRPCClient) Status(context.Context) (*coretypes.ResultStatus, error) {
	return nil, nil
}

func (f *fakeCometRPCClient) ABCIInfo(context.Context) (*coretypes.ResultABCIInfo, error) {
	return nil, nil
}

func (f *fakeCometRPCClient) Tx(_ context.Context, hash []byte, prove bool) (*coretypes.ResultTx, error) {
	f.txCalls++
	f.txHash = append([]byte(nil), hash...)
	f.txProve = prove
	return f.txResult, f.txErr
}

func TestQueryTx(t *testing.T) {
	t.Parallel()

	validHash := strings.Repeat("ab", 32)
	tests := []struct {
		name       string
		hash       string
		rpcResult  *coretypes.ResultTx
		rpcErr     error
		wantResult *coretypes.ResultTx
		wantError  string
		wantCalls  int
	}{
		{
			name:       "queries exact hash without proof",
			hash:       validHash,
			rpcResult:  &coretypes.ResultTx{Height: 42},
			wantResult: &coretypes.ResultTx{Height: 42},
			wantCalls:  1,
		},
		{
			name:      "rejects malformed hex",
			hash:      "not-hex",
			wantError: "decode transaction hash",
		},
		{
			name:      "rejects wrong hash length",
			hash:      "abcd",
			wantError: "32 bytes",
		},
		{
			name:      "wraps rpc failure",
			hash:      validHash,
			rpcErr:    errors.New("transaction indexing is disabled"),
			wantError: "querying transaction " + validHash,
			wantCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			rpcClient := &fakeCometRPCClient{txResult: tt.rpcResult, txErr: tt.rpcErr}
			client := &Client{rpcClient: rpcClient}

			result, err := client.QueryTx(t.Context(), tt.hash)
			if tt.wantError != "" {
				require.Error(t, err)
				assert.ErrorContains(t, err, tt.wantError)
			} else {
				require.NoError(t, err)
				assert.Equal(t, tt.wantResult, result)
			}
			assert.Equal(t, tt.wantCalls, rpcClient.txCalls)
			if tt.wantCalls > 0 {
				assert.Len(t, rpcClient.txHash, 32)
				assert.False(t, rpcClient.txProve)
			}
		})
	}
}
