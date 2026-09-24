package chainutils

import (
	"bufio"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/cosmos/cosmos-sdk/client"
	"github.com/cosmos/cosmos-sdk/client/keys"
	"github.com/cosmos/cosmos-sdk/codec"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	cryptocodec "github.com/cosmos/cosmos-sdk/crypto/codec"
	"github.com/cosmos/cosmos-sdk/crypto/keyring"
	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/stretchr/testify/require"

	appsv1 "github.com/voluzi/cosmopilot/v4/api/v1"
	"github.com/voluzi/cosmopilot/v4/internal/chainutils/sdkcmd"
)

// TestRecoverAccountArgsRecoverOperatorAccount runs the SDK keys CLI with the generated recovery
// arguments and checks it recovers the account the operator derived, for default and custom paths.
func TestRecoverAccountArgsRecoverOperatorAccount(t *testing.T) {
	for _, hdPath := range []string{"m/44'/118'/0'/0/0", "m/44'/118'/1'/0/0"} {
		t.Run(hdPath, func(t *testing.T) {
			want, err := AccountFromMnemonic(testMnemonic, "cosmos", "cosmosvaloper", hdPath)
			require.NoError(t, err)

			cmdSDK, err := sdkcmd.GetSDK(appsv1.V0_47)
			require.NoError(t, err)
			args := cmdSDK.RecoverAccountArgs("account", want.HDPath)
			require.Equal(t, []string{"keys", "add"}, args[:2])

			home := t.TempDir()
			cmd := keys.AddKeyCommand()
			cmd.Flags().AddFlagSet(keys.Commands(home).PersistentFlags())
			in := bufio.NewReader(strings.NewReader(testMnemonic + "\n"))
			cmd.SetIn(in)
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)
			registry := codectypes.NewInterfaceRegistry()
			cryptocodec.RegisterInterfaces(registry)
			cdc := codec.NewProtoCodec(registry)
			clientCtx := client.Context{}.WithKeyringDir(home).WithInput(in).WithCodec(cdc)
			ctx := context.WithValue(context.Background(), client.ClientContextKey, &clientCtx)

			cmd.SetArgs(append(args[2:], "--home", home))
			require.NoError(t, cmd.ExecuteContext(ctx))

			kb, err := keyring.New(sdk.KeyringServiceName(), keyring.BackendTest, home, nil, cdc)
			require.NoError(t, err)
			record, err := kb.Key("account")
			require.NoError(t, err)
			addr, err := record.GetAddress()
			require.NoError(t, err)
			got, err := sdk.Bech32ifyAddressBytes("cosmos", addr)
			require.NoError(t, err)
			require.Equal(t, want.Address, got)
		})
	}
}

func TestAccountRecoveryPodsPassHDPath(t *testing.T) {
	const hdPath = "m/44'/118'/1'/0/0"
	a := newTestGenesisApp(t)

	extraValidators := []*GenesisValidator{
		{PrivKeySecret: "v1-priv-key", Account: &Account{Address: "addr-v1", HDPath: hdPath}, NodeInfo: &NodeInfo{Moniker: "v1"}, StakeAmount: "1stake"},
	}
	genesisPod := a.buildGenesisPod("owner-priv-key",
		&Account{Address: "addr-owner", HDPath: hdPath}, &NodeInfo{Moniker: "owner"},
		&Params{ChainID: "test-chain", StakeAmount: "1stake"}, extraValidators, nil)
	for _, name := range []string{"load-account", "load-account-1"} {
		assertCommandArg(t, requireContainer(t, genesisPod.Spec.InitContainers, name).Args, "--hd-path", hdPath)
	}

	validatorPod, err := a.buildCreateValidatorPod(validValidatorPubKey, hdPath, &NodeInfo{Moniker: "validator"},
		&Params{ChainID: "test-chain", StakeAmount: "1stake"}, "tcp://node:26657")
	require.NoError(t, err)
	assertCommandArg(t, requireContainer(t, validatorPod.Spec.InitContainers, "load-account").Args, "--hd-path", hdPath)
}
