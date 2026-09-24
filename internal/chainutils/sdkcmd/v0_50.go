package sdkcmd

import (
	"encoding/base64"
	"encoding/json"
	"fmt"

	appsv1 "github.com/voluzi/cosmopilot/v4/api/v1"
)

func init() {
	RegisterSDK(appsv1.V0_50, func(globalOptions ...Option) SDK {
		return newV0_50(globalOptions...)
	})
}

func newV0_50(globalOptions ...Option) *v0_50 {
	return &v0_50{v0_47: *newV0_47(globalOptions...)}
}

type v0_50 struct {
	v0_47
}

type createValidatorJSON struct {
	Amount                  string          `json:"amount"`
	PubKey                  json.RawMessage `json:"pubkey"`
	Moniker                 string          `json:"moniker"`
	CommissionRate          string          `json:"commission-rate"`
	CommissionMaxRate       string          `json:"commission-max-rate"`
	CommissionMaxChangeRate string          `json:"commission-max-change-rate"`
	MinSelfDelegation       string          `json:"min-self-delegation"`
	Identity                *string         `json:"identity,omitempty"`
	Website                 *string         `json:"website,omitempty"`
	Details                 *string         `json:"details,omitempty"`
}

type validatorPubKeyJSON struct {
	Type string `json:"@type"`
	Key  string `json:"key"`
}

func (sdk *v0_50) CreateValidatorCommand(validatorFile, account, pubKey, moniker, stakeAmount, chainID, gasPrices string, options ...*ArgOption) (CreateValidatorCommand, error) {
	var pubKeyObject validatorPubKeyJSON
	if err := json.Unmarshal([]byte(pubKey), &pubKeyObject); err != nil {
		return CreateValidatorCommand{}, fmt.Errorf("decode validator public key: %w", err)
	}
	if pubKeyObject.Type == "" {
		return CreateValidatorCommand{}, fmt.Errorf("decode validator public key: non-empty @type is required")
	}
	keyMaterial, err := base64.StdEncoding.DecodeString(pubKeyObject.Key)
	if err != nil {
		return CreateValidatorCommand{}, fmt.Errorf("decode validator public key: decode key material: %w", err)
	}
	if len(keyMaterial) == 0 {
		return CreateValidatorCommand{}, fmt.Errorf("decode validator public key: non-empty key material is required")
	}

	payload := createValidatorJSON{
		Amount:            stakeAmount,
		PubKey:            json.RawMessage(pubKey),
		Moniker:           moniker,
		MinSelfDelegation: "1",
	}
	args := []string{
		"tx", "staking", "create-validator", validatorFile,
		"--chain-id", chainID,
		"--gas-prices", gasPrices,
		"--from", account,
		"--keyring-backend", "test",
		"--yes",
	}
	for _, option := range options {
		if option == nil {
			continue
		}
		switch option.Key {
		case CommissionRate:
			payload.CommissionRate = option.Value
		case CommissionMaxRate:
			payload.CommissionMaxRate = option.Value
		case CommissionMaxChangeRate:
			payload.CommissionMaxChangeRate = option.Value
		case MinSelfDelegation:
			payload.MinSelfDelegation = option.Value
		case Identity:
			value := option.Value
			payload.Identity = &value
		case Website:
			value := option.Value
			payload.Website = &value
		case Details:
			value := option.Value
			payload.Details = &value
		default:
			args = applyArgOption(args, option)
		}
	}

	validatorJSON, err := json.Marshal(payload)
	if err != nil {
		return CreateValidatorCommand{}, fmt.Errorf("encode validator JSON: %w", err)
	}
	return CreateValidatorCommand{
		Args:          append(args, sdk.options.GlobalArgs...),
		ValidatorJSON: validatorJSON,
	}, nil
}

func (sdk *v0_50) GenesisSetExpeditedVotingPeriodCmd(votingPeriod, genesisFile string) string {
	return fmt.Sprintf("jq '.app_state.gov.params.expedited_voting_period = %q' %s > /tmp/genesis.tmp && mv /tmp/genesis.tmp %s",
		votingPeriod, genesisFile, genesisFile,
	)
}
