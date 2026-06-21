package chainutils

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/chainutils/sdkcmd"
)

func newTestGenesisApp(t *testing.T) *App {
	t.Helper()
	cmd, err := sdkcmd.GetSDK(appsv1.V0_47)
	require.NoError(t, err)
	return &App{
		cmd:        cmd,
		owner:      &metav1.ObjectMeta{Name: "test-node", Namespace: "default"},
		binary:     "appd",
		image:      "example/app:latest",
		sdkVersion: appsv1.V0_47,
	}
}

// TestBuildGenesisPodContainerNamesUnique verifies the genesis init pod has unique init-container
// names even when regular extra accounts and extra genesis validators are both present. A regular
// "add-account-N" must never collide with an extra validator's account container (which would
// produce an invalid pod spec).
func TestBuildGenesisPodContainerNamesUnique(t *testing.T) {
	a := newTestGenesisApp(t)

	account := &Account{Mnemonic: "owner mnemonic", Address: "addr-owner"}
	params := &Params{
		ChainID:     "test-chain",
		Assets:      []string{"1000000stake"},
		StakeAmount: "900000stake",
		// Two extra funded accounts -> add-account-0, add-account-1.
		Accounts: []AccountAssets{
			{Address: "addr-a", Assets: []string{"1stake"}},
			{Address: "addr-b", Assets: []string{"1stake"}},
		},
	}
	// Two extra genesis validators -> add-validator-account-1, add-validator-account-2.
	extraValidators := []*GenesisValidator{
		{
			PrivKeySecret: "v1-priv-key",
			Account:       &Account{Mnemonic: "v1 mnemonic", Address: "addr-v1"},
			NodeInfo:      &NodeInfo{Moniker: "v1"},
			StakeAmount:   "900000stake",
			Assets:        []string{"1000000stake"},
		},
		{
			PrivKeySecret: "v2-priv-key",
			Account:       &Account{Mnemonic: "v2 mnemonic", Address: "addr-v2"},
			NodeInfo:      &NodeInfo{Moniker: "v2"},
			StakeAmount:   "900000stake",
			Assets:        []string{"1000000stake"},
		},
	}

	pod := a.buildGenesisPod("owner-priv-key", account, &NodeInfo{Moniker: "owner"}, params, extraValidators, nil)

	seen := map[string]bool{}
	for _, c := range pod.Spec.InitContainers {
		require.NotEmpty(t, c.Name)
		assert.Falsef(t, seen[c.Name], "duplicate init container name %q", c.Name)
		seen[c.Name] = true
	}

	// The regular and validator account containers must both be present and distinct.
	for _, name := range []string{
		"add-account-0", "add-account-1",
		"add-validator-account-1", "add-validator-account-2",
		"load-account-1", "load-account-2",
		"load-priv-key-1", "load-priv-key-2",
		"gentx-1", "gentx-2",
		"collect-gentxs",
	} {
		assert.Containsf(t, seen, name, "expected init container %q", name)
	}
}

// TestBuildGenesisPodPerValidatorWiring verifies each extra validator gets its own consensus-key
// volume and a gentx written to a distinct output document, so collected gentxs do not overwrite
// each other.
func TestBuildGenesisPodPerValidatorWiring(t *testing.T) {
	a := newTestGenesisApp(t)

	extraValidators := []*GenesisValidator{
		{PrivKeySecret: "v1-priv-key", Account: &Account{Address: "addr-v1"}, NodeInfo: &NodeInfo{Moniker: "v1"}, StakeAmount: "1stake"},
		{PrivKeySecret: "v2-priv-key", Account: &Account{Address: "addr-v2"}, NodeInfo: &NodeInfo{Moniker: "v2"}, StakeAmount: "1stake"},
	}

	pod := a.buildGenesisPod("owner-priv-key",
		&Account{Address: "addr-owner"}, &NodeInfo{Moniker: "owner"},
		&Params{ChainID: "test-chain", StakeAmount: "1stake"}, extraValidators, nil)

	// Each extra validator's priv-key secret is mounted via a distinct volume.
	for idx, ev := range extraValidators {
		volName := fmt.Sprintf("priv-key-%d", idx+1)
		var found bool
		for _, v := range pod.Spec.Volumes {
			if v.Name == volName {
				require.NotNil(t, v.Secret)
				assert.Equal(t, ev.PrivKeySecret, v.Secret.SecretName)
				found = true
			}
		}
		assert.Truef(t, found, "expected volume %q for extra validator", volName)
	}

	// Each extra validator's gentx writes to its own output document.
	for idx := range extraValidators {
		name := fmt.Sprintf("gentx-%d", idx+1)
		var args []string
		for _, c := range pod.Spec.InitContainers {
			if c.Name == name {
				args = c.Args
			}
		}
		require.NotEmpty(t, args, "missing gentx container %q", name)
		assert.Contains(t, strings.Join(args, " "), fmt.Sprintf("gentx-%d.json", idx+1))
	}
}
