package sdkcmd

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	appsv1 "github.com/voluzi/cosmopilot/v4/api/v1"
)

const testValidatorPubKey = `{"@type":"/cosmos.crypto.ed25519.PubKey","key":"oWg2ISpLF405Jcm2vXV+2v4fnjodh6aafuIdeoW+rUw="}`

func TestCreateValidatorCommandSDKVersions(t *testing.T) {
	t.Parallel()

	legacyArgs := []string{
		"tx", "staking", "create-validator",
		"--amount", "1000stake",
		"--moniker", "validator",
		"--chain-id", "chain-1",
		"--pubkey", testValidatorPubKey,
		"--gas-prices", "0.025stake",
		"--from", "account",
		"--keyring-backend", "test",
		"--yes",
		"--commission-max-change-rate", "0.01",
		"--commission-max-rate", "0.20",
		"--commission-rate", "0.10",
		"--min-self-delegation", "7",
		"--details", "details",
		"--website", "https://example.com",
		"--identity", "identity",
		"--node", "tcp://node:26657",
		"--home", "/home/app",
	}
	modernArgs := []string{
		"tx", "staking", "create-validator", "/home/app/validator.json",
		"--chain-id", "chain-1",
		"--gas-prices", "0.025stake",
		"--from", "account",
		"--keyring-backend", "test",
		"--yes",
		"--node", "tcp://node:26657",
		"--home", "/home/app",
	}

	tests := []struct {
		name       string
		sdkVersion appsv1.SdkVersion
		wantArgs   []string
		wantJSON   bool
	}{
		{name: "v0.45", sdkVersion: appsv1.V0_45, wantArgs: legacyArgs},
		{name: "v0.47", sdkVersion: appsv1.V0_47, wantArgs: legacyArgs},
		{name: "v0.50", sdkVersion: appsv1.V0_50, wantArgs: modernArgs, wantJSON: true},
		{name: "v0.53", sdkVersion: appsv1.V0_53, wantArgs: modernArgs, wantJSON: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sdk, err := GetSDK(tt.sdkVersion, WithGlobalArg(Home, "/home/app"))
			require.NoError(t, err)
			command, err := sdk.CreateValidatorCommand(
				"/home/app/validator.json",
				"account",
				testValidatorPubKey,
				"validator",
				"1000stake",
				"chain-1",
				"0.025stake",
				WithArg(CommissionMaxChangeRate, "0.01"),
				WithArg(CommissionMaxRate, "0.20"),
				WithArg(CommissionRate, "0.10"),
				WithArg(MinSelfDelegation, "7"),
				WithArg(Details, "details"),
				WithArg(Website, "https://example.com"),
				WithArg(Identity, "identity"),
				nil,
				WithArg(Node, "tcp://node:26657"),
			)
			require.NoError(t, err)
			assert.Equal(t, tt.wantArgs, command.Args)
			if tt.wantJSON {
				assert.NotEmpty(t, command.ValidatorJSON)
			} else {
				assert.Nil(t, command.ValidatorJSON)
			}
		})
	}
}

func TestModernCreateValidatorCommandPayload(t *testing.T) {
	t.Parallel()

	identity := "validator-identity"
	website := "https://validator.example.com"
	details := "quotes: \"hello\"\nbacktick: ` dollar: $ expansion: $(NAME) unicode: Olá"
	sdk, err := GetSDK(appsv1.V0_53)
	require.NoError(t, err)
	command, err := sdk.CreateValidatorCommand(
		"/validator.json", "account", testValidatorPubKey, "validator", "1000stake", "chain-1", "0.025stake",
		WithArg(CommissionRate, "0.11"),
		WithArg(CommissionMaxRate, "0.22"),
		WithArg(CommissionMaxChangeRate, "0.03"),
		WithArg(MinSelfDelegation, "9"),
		WithArg(Identity, identity),
		WithArg(Website, website),
		WithArg(Details, details),
	)
	require.NoError(t, err)

	var payload struct {
		Amount                  string                     `json:"amount"`
		PubKey                  map[string]json.RawMessage `json:"pubkey"`
		Moniker                 string                     `json:"moniker"`
		CommissionRate          string                     `json:"commission-rate"`
		CommissionMaxRate       string                     `json:"commission-max-rate"`
		CommissionMaxChangeRate string                     `json:"commission-max-change-rate"`
		MinSelfDelegation       string                     `json:"min-self-delegation"`
		Identity                *string                    `json:"identity"`
		Website                 *string                    `json:"website"`
		Details                 *string                    `json:"details"`
	}
	require.NoError(t, json.Unmarshal(command.ValidatorJSON, &payload))
	assert.Equal(t, "1000stake", payload.Amount)
	assert.Equal(t, "validator", payload.Moniker)
	assert.Equal(t, "0.11", payload.CommissionRate)
	assert.Equal(t, "0.22", payload.CommissionMaxRate)
	assert.Equal(t, "0.03", payload.CommissionMaxChangeRate)
	assert.Equal(t, "9", payload.MinSelfDelegation)
	require.NotNil(t, payload.Identity)
	require.NotNil(t, payload.Website)
	require.NotNil(t, payload.Details)
	assert.Equal(t, identity, *payload.Identity)
	assert.Equal(t, website, *payload.Website)
	assert.Equal(t, details, *payload.Details)
	pubKeyJSON, err := json.Marshal(payload.PubKey)
	require.NoError(t, err)
	assert.JSONEq(t, testValidatorPubKey, string(pubKeyJSON))
}

func TestModernCreateValidatorCommandDefaultsAndDoesNotLeakOptions(t *testing.T) {
	t.Parallel()

	sdk, err := GetSDK(appsv1.V0_50)
	require.NoError(t, err)
	first, err := sdk.CreateValidatorCommand(
		"/validator.json", "account", testValidatorPubKey, "first", "1stake", "chain", "0stake",
		WithArg(Identity, "first-identity"),
	)
	require.NoError(t, err)
	second, err := sdk.CreateValidatorCommand(
		"/validator.json", "account", testValidatorPubKey, "second", "2stake", "chain", "0stake",
		WithArg(MinSelfDelegation, ""),
	)
	require.NoError(t, err)

	var firstPayload, secondPayload map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(first.ValidatorJSON, &firstPayload))
	require.NoError(t, json.Unmarshal(second.ValidatorJSON, &secondPayload))
	assert.JSONEq(t, `"1"`, string(firstPayload[MinSelfDelegation]))
	assert.Contains(t, firstPayload, Identity)
	assert.JSONEq(t, `""`, string(secondPayload[MinSelfDelegation]))
	assert.NotContains(t, secondPayload, Identity)
	assert.Equal(t, "second", mustJSONString(t, secondPayload["moniker"]))
	assert.NotContains(t, second.Args, "--min-self-delegation")
	assert.NotContains(t, second.Args, "--identity")
}

func TestCreateValidatorCommandPubKeyValidation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		sdkVersion appsv1.SdkVersion
		pubKey     string
		wantErr    bool
	}{
		{name: "legacy leaves validation to CLI", sdkVersion: appsv1.V0_47, pubKey: "not-json"},
		{name: "modern rejects malformed JSON", sdkVersion: appsv1.V0_50, pubKey: "not-json", wantErr: true},
		{name: "modern rejects JSON array", sdkVersion: appsv1.V0_53, pubKey: `[]`, wantErr: true},
		{name: "modern rejects JSON null", sdkVersion: appsv1.V0_53, pubKey: `null`, wantErr: true},
		{name: "modern rejects empty object", sdkVersion: appsv1.V0_53, pubKey: `{}`, wantErr: true},
		{name: "modern rejects missing type", sdkVersion: appsv1.V0_53, pubKey: `{"key":"YQ=="}`, wantErr: true},
		{name: "modern rejects empty type", sdkVersion: appsv1.V0_53, pubKey: `{"@type":"","key":"YQ=="}`, wantErr: true},
		{name: "modern rejects missing key", sdkVersion: appsv1.V0_53, pubKey: `{"@type":"/cosmos.crypto.ed25519.PubKey"}`, wantErr: true},
		{name: "modern rejects empty key", sdkVersion: appsv1.V0_53, pubKey: `{"@type":"/cosmos.crypto.ed25519.PubKey","key":""}`, wantErr: true},
		{name: "modern rejects invalid key base64", sdkVersion: appsv1.V0_53, pubKey: `{"@type":"/cosmos.crypto.ed25519.PubKey","key":"not-base64"}`, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			sdk, err := GetSDK(tt.sdkVersion)
			require.NoError(t, err)
			_, err = sdk.CreateValidatorCommand("/validator.json", "account", tt.pubKey, "validator", "1stake", "chain", "0stake")
			if tt.wantErr {
				require.Error(t, err)
				assert.ErrorContains(t, err, "decode validator public key")
				return
			}
			require.NoError(t, err)
		})
	}
}

func mustJSONString(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var value string
	require.NoError(t, json.Unmarshal(raw, &value))
	return value
}
