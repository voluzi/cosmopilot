package sdkcmd

import (
	"fmt"
	"strings"

	appsv1 "github.com/voluzi/cosmopilot/api/v1"
)

func init() {
	RegisterSDK(appsv1.V0_47, func(globalOptions ...Option) SDK {
		return newV0_47(globalOptions...)
	})
}

func newV0_47(globalOptions ...Option) *v0_47 {
	return &v0_47{v0_45: *newV0_45(globalOptions...)}
}

type v0_47 struct {
	v0_45
}

func (sdk *v0_47) AddGenesisAccountArgs(account string, assets []string) []string {
	return append(
		[]string{"genesis", "add-genesis-account", account, strings.Join(assets, ",")},
		sdk.options.GlobalArgs...,
	)
}

func (sdk *v0_47) GenTxArgs(account, moniker, stakeAmount, chainID string, options ...*ArgOption) []string {
	args := []string{
		"genesis", "gentx", account, stakeAmount,
		"--moniker", moniker,
		"--chain-id", chainID,
		"--keyring-backend", "test",
		"--yes",
	}
	args = applyArgOptions(args, options)
	return append(args, sdk.options.GlobalArgs...)
}

func (sdk *v0_47) CollectGenTxsArgs() []string {
	return append(
		[]string{"genesis", "collect-gentxs"},
		sdk.options.GlobalArgs...,
	)
}

func (sdk *v0_47) GenesisSetVotingPeriodCmd(votingPeriod, genesisFile string) string {
	return fmt.Sprintf("jq '.app_state.gov.params.voting_period = %q' %s > /tmp/genesis.tmp && mv /tmp/genesis.tmp %s",
		votingPeriod, genesisFile, genesisFile,
	)
}
