package e2e

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	appsv1 "github.com/voluzi/cosmopilot/v4/api/v1"
	"github.com/voluzi/cosmopilot/v4/internal/chainutils"
	chainnodecontroller "github.com/voluzi/cosmopilot/v4/internal/controllers/chainnode"
	"github.com/voluzi/cosmopilot/v4/test/e2e/apps"
)

const (
	joiningValidatorGroup      = "joining"
	joiningValidatorMoniker    = "allora-e2e-joining-validator"
	joiningValidatorDetails    = `post-genesis "joining" validator created by cosmopilot e2e`
	joiningValidatorWebsite    = "https://example.com/cosmopilot-e2e"
	joiningValidatorIdentity   = "A1B2C3D4E5F60708"
	joiningCommissionRate      = "0.05"
	joiningCommissionMaxRate   = "0.20"
	joiningCommissionMaxChange = "0.01"
	joiningMinSelfDelegation   = "1"
	joiningAccountSecretName   = "joining-validator-account"
	joiningValidatorStake      = "10allo"
	joiningValidatorStakeUallo = "10000000000000000000"
	bondStatusBonded           = "BOND_STATUS_BONDED"
	decimalCommissionRate      = "0.050000000000000000"
	decimalCommissionMaxRate   = "0.200000000000000000"
	decimalCommissionMaxChange = "0.010000000000000000"
)

type stakingValidatorsResponse struct {
	Validators []stakingValidator `json:"validators"`
}

type bankBalancesResponse struct {
	Balances []bankBalance `json:"balances"`
}

type bankBalance struct {
	Denom  string `json:"denom"`
	Amount string `json:"amount"`
}

type stakingValidator struct {
	OperatorAddress string          `json:"operator_address"`
	ConsensusPubKey json.RawMessage `json:"consensus_pubkey"`
	Jailed          bool            `json:"jailed"`
	Status          string          `json:"status"`
	Tokens          string          `json:"tokens"`
	Description     struct {
		Moniker  string `json:"moniker"`
		Identity string `json:"identity"`
		Website  string `json:"website"`
		Details  string `json:"details"`
	} `json:"description"`
	Commission struct {
		CommissionRates struct {
			Rate          string `json:"rate"`
			MaxRate       string `json:"max_rate"`
			MaxChangeRate string `json:"max_change_rate"`
		} `json:"commission_rates"`
	} `json:"commission"`
	MinSelfDelegation string `json:"min_self_delegation"`
}

type normalizedConsensusPubKey struct {
	Type string
	Key  []byte
}

func normalizeConsensusPubKey(raw json.RawMessage) (normalizedConsensusPubKey, error) {
	var value struct {
		ProtoType  string `json:"@type"`
		LegacyType string `json:"type"`
		Key        string `json:"key"`
		Value      string `json:"value"`
	}
	if err := json.Unmarshal(raw, &value); err != nil {
		return normalizedConsensusPubKey{}, fmt.Errorf("decode consensus public key JSON: %w", err)
	}
	if value.ProtoType != "" && value.LegacyType != "" && value.ProtoType != value.LegacyType {
		return normalizedConsensusPubKey{}, fmt.Errorf("conflicting consensus public key types %q and %q", value.ProtoType, value.LegacyType)
	}
	keyType := value.ProtoType
	if keyType == "" {
		keyType = value.LegacyType
	}
	if keyType == "" {
		return normalizedConsensusPubKey{}, fmt.Errorf("consensus public key type is required")
	}
	if value.Key != "" && value.Value != "" && value.Key != value.Value {
		return normalizedConsensusPubKey{}, fmt.Errorf("conflicting consensus public key values")
	}
	keyValue := value.Key
	if keyValue == "" {
		keyValue = value.Value
	}
	key, err := base64.StdEncoding.DecodeString(keyValue)
	if err != nil {
		return normalizedConsensusPubKey{}, fmt.Errorf("decode consensus public key value: %w", err)
	}
	if len(key) == 0 {
		return normalizedConsensusPubKey{}, fmt.Errorf("consensus public key value is required")
	}
	return normalizedConsensusPubKey{Type: keyType, Key: key}, nil
}

func consensusPubKeysMatch(left, right json.RawMessage) (bool, error) {
	leftKey, err := normalizeConsensusPubKey(left)
	if err != nil {
		return false, err
	}
	rightKey, err := normalizeConsensusPubKey(right)
	if err != nil {
		return false, err
	}
	return leftKey.Type == rightKey.Type && bytes.Equal(leftKey.Key, rightKey.Key), nil
}

func TestNormalizeConsensusPubKeyEquivalentRepresentations(t *testing.T) {
	t.Parallel()

	expected, err := normalizeConsensusPubKey(json.RawMessage(`{"@type":"/cosmos.crypto.ed25519.PubKey","key":"oWg2ISpLF405Jcm2vXV+2v4fnjodh6aafuIdeoW+rUw="}`))
	if err != nil {
		t.Fatal(err)
	}
	actual, err := normalizeConsensusPubKey(json.RawMessage(`{"type":"/cosmos.crypto.ed25519.PubKey","value":"oWg2ISpLF405Jcm2vXV+2v4fnjodh6aafuIdeoW+rUw="}`))
	if err != nil {
		t.Fatal(err)
	}
	if expected.Type != actual.Type || !bytes.Equal(expected.Key, actual.Key) {
		t.Fatalf("equivalent consensus keys differ: expected %#v, actual %#v", expected, actual)
	}
}

func TestNormalizeConsensusPubKeyRejectsInvalidInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		raw       string
		wantError string
	}{
		{
			name:      "missing type",
			raw:       `{"key":"YQ=="}`,
			wantError: "type is required",
		},
		{
			name:      "conflicting types",
			raw:       `{"@type":"proto","type":"legacy","key":"YQ=="}`,
			wantError: "conflicting consensus public key types",
		},
		{
			name:      "empty key",
			raw:       `{"@type":"proto"}`,
			wantError: "value is required",
		},
		{
			name:      "invalid base64",
			raw:       `{"@type":"proto","key":"%%%"}`,
			wantError: "decode consensus public key value",
		},
		{
			name:      "conflicting values",
			raw:       `{"@type":"proto","key":"YQ==","value":"Yg=="}`,
			wantError: "conflicting consensus public key values",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := normalizeConsensusPubKey(json.RawMessage(tt.raw))
			if err == nil || !bytes.Contains([]byte(err.Error()), []byte(tt.wantError)) {
				t.Fatalf("normalizeConsensusPubKey() error = %v, want error containing %q", err, tt.wantError)
			}
		})
	}
}

var _ = Describe("ChainNodeSet Post-Genesis Validator", func() {
	for _, app := range apps.All() {
		if app.Name != "Allora" || app.AppSpec.Version == nil || *app.AppSpec.Version != "v0.14.0" ||
			app.AppSpec.SdkVersion == nil || *app.AppSpec.SdkVersion != appsv1.V0_50 {
			continue
		}

		It("should create and bond an Allora SDK v0.50 validator after genesis", Serial, Label(apps.LabelPerApp), WithNs(func(ns *corev1.Namespace) {
			joiningAccount, err := chainutils.CreateAccount(
				app.ValidatorConfig.AccountPrefix,
				app.ValidatorConfig.ValPrefix,
				appsv1.DefaultHDPath,
			)
			Expect(err).NotTo(HaveOccurred())

			Expect(Framework().Client().Create(Framework().Context(), &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: joiningAccountSecretName, Namespace: ns.Name},
				Data: map[string][]byte{
					chainnodecontroller.MnemonicKey: []byte(joiningAccount.Mnemonic),
				},
			})).To(Succeed())

			chainNodeSet := app.BuildChainNodeSet(ns.Name, 0)
			chainNodeSet.Spec.Nodes = []appsv1.NodeGroupSpec{}
			chainNodeSet.Spec.Validator.Init.Accounts = append(chainNodeSet.Spec.Validator.Init.Accounts, appsv1.AccountAssets{
				Address: joiningAccount.Address,
				Assets:  append([]string{}, app.ValidatorConfig.Assets...),
			})
			Expect(Framework().Client().Create(Framework().Context(), chainNodeSet)).To(Succeed())

			WaitForChainNodeSetRunning(chainNodeSet)
			WaitForChainNodeSetHeight(chainNodeSet, 1)
			RefreshChainNodeSet(chainNodeSet)

			genesisValidatorName := chainNodeSet.GeneratedValidatorNodeName(appsv1.ReservedValidatorGroupName, 0)
			Eventually(func(g Gomega) {
				response, err := queryStakingValidators(ns.Name, genesisValidatorName, app.AppSpec.App)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(response.Validators).To(HaveLen(1))
				g.Expect(response.Validators[0].Status).To(Equal(bondStatusBonded))
				_, found := findStakingValidator(response.Validators, joiningAccount.ValidatorAddress)
				g.Expect(found).To(BeFalse())
			}).Should(Succeed())

			Eventually(func(g Gomega) {
				response, err := queryBankBalances(ns.Name, genesisValidatorName, app.AppSpec.App, joiningAccount.Address)
				g.Expect(err).NotTo(HaveOccurred())
				balance, found := findBankBalance(response.Balances, app.ValidatorConfig.Denom)
				g.Expect(found).To(BeTrue(), "missing pre-funded %s balance", app.ValidatorConfig.Denom)
				balanceAmount, ok := new(big.Int).SetString(balance, 10)
				g.Expect(ok).To(BeTrue())
				stakeAmount, ok := new(big.Int).SetString(joiningValidatorStakeUallo, 10)
				g.Expect(ok).To(BeTrue())
				g.Expect(balanceAmount.Cmp(stakeAmount)).To(BeNumerically(">", 0))
			}).Should(Succeed())

			joiningGroup := appsv1.NodeGroupSpec{
				Name:      joiningValidatorGroup,
				Instances: ptr.To(1),
				Validator: &appsv1.NodeSetValidatorConfig{
					Info: &appsv1.ValidatorInfo{
						Moniker:  ptr.To(joiningValidatorMoniker),
						Details:  ptr.To(joiningValidatorDetails),
						Website:  ptr.To(joiningValidatorWebsite),
						Identity: ptr.To(joiningValidatorIdentity),
					},
					Config:        chainNodeSet.Spec.Validator.Config,
					Persistence:   chainNodeSet.Spec.Validator.Persistence,
					AccountPrefix: ptr.To(app.ValidatorConfig.AccountPrefix),
					ValPrefix:     ptr.To(app.ValidatorConfig.ValPrefix),
					CreateValidator: &appsv1.CreateValidatorConfig{
						AccountMnemonicSecret:   ptr.To(joiningAccountSecretName),
						CommissionRate:          ptr.To(joiningCommissionRate),
						CommissionMaxRate:       ptr.To(joiningCommissionMaxRate),
						CommissionMaxChangeRate: ptr.To(joiningCommissionMaxChange),
						MinSelfDelegation:       ptr.To(joiningMinSelfDelegation),
						StakeAmount:             joiningValidatorStake,
						GasPrices:               "10" + app.ValidatorConfig.Denom,
					},
				},
			}

			Eventually(func() error {
				current := &appsv1.ChainNodeSet{}
				if err := Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(chainNodeSet), current); err != nil {
					return err
				}
				for _, group := range current.Spec.Nodes {
					if group.Name == joiningValidatorGroup {
						return nil
					}
				}
				current.Spec.Nodes = append(current.Spec.Nodes, joiningGroup)
				return Framework().Client().Update(Framework().Context(), current)
			}).Should(Succeed())

			joiningValidatorName := chainNodeSet.GeneratedValidatorNodeName(joiningValidatorGroup, 0)
			joiningNode := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: joiningValidatorName, Namespace: ns.Name}}
			Eventually(func() error {
				return Framework().Client().Get(Framework().Context(), client.ObjectKeyFromObject(joiningNode), joiningNode)
			}).Should(Succeed())

			Expect(joiningNode.Spec.Genesis).NotTo(BeNil())
			Expect(joiningNode.Spec.Genesis.ConfigMap).NotTo(BeNil())
			Expect(*joiningNode.Spec.Genesis.ConfigMap).To(Equal(fmt.Sprintf("%s-genesis", chainNodeSet.Status.ChainID)))
			Expect(joiningNode.Spec.Validator).NotTo(BeNil())
			Expect(joiningNode.Spec.Validator.Init).To(BeNil())
			Expect(joiningNode.Spec.Validator.CreateValidator).NotTo(BeNil())
			Expect(joiningNode.Spec.Validator.CreateValidator.AccountMnemonicSecret).To(Equal(ptr.To(joiningAccountSecretName)))
			Expect(metav1.IsControlledBy(joiningNode, chainNodeSet)).To(BeTrue())

			Eventually(func(g Gomega) {
				g.Expect(Framework().Client().Get(
					Framework().Context(), client.ObjectKeyFromObject(joiningNode), joiningNode,
				)).To(Succeed())
				g.Expect(joiningNode.Status.Phase).To(Equal(appsv1.PhaseChainNodeRunning))
				g.Expect(joiningNode.Status.AccountAddress).To(Equal(joiningAccount.Address))
				g.Expect(joiningNode.Status.PubKey).NotTo(BeEmpty())
			}).Should(Succeed())
			expectedJoiningPubKey := joiningNode.Status.PubKey

			var joinedValidator stakingValidator
			Eventually(func(g Gomega) {
				response, err := queryStakingValidators(ns.Name, genesisValidatorName, app.AppSpec.App)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(response.Validators).To(HaveLen(2))

				validator, found := findStakingValidator(response.Validators, joiningAccount.ValidatorAddress)
				g.Expect(found).To(BeTrue())
				g.Expect(validator.Status).To(Equal(bondStatusBonded))
				g.Expect(validator.Jailed).To(BeFalse())
				pubKeysMatch, err := consensusPubKeysMatch(validator.ConsensusPubKey, json.RawMessage(expectedJoiningPubKey))
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(pubKeysMatch).To(BeTrue())
				g.Expect(validator.Description.Moniker).To(Equal(joiningValidatorMoniker))
				g.Expect(validator.Description.Details).To(Equal(joiningValidatorDetails))
				g.Expect(validator.Description.Website).To(Equal(joiningValidatorWebsite))
				g.Expect(validator.Description.Identity).To(Equal(joiningValidatorIdentity))
				g.Expect(validator.Commission.CommissionRates.Rate).To(Equal(decimalCommissionRate))
				g.Expect(validator.Commission.CommissionRates.MaxRate).To(Equal(decimalCommissionMaxRate))
				g.Expect(validator.Commission.CommissionRates.MaxChangeRate).To(Equal(decimalCommissionMaxChange))
				g.Expect(validator.MinSelfDelegation).To(Equal(joiningMinSelfDelegation))

				tokens, ok := new(big.Int).SetString(validator.Tokens, 10)
				g.Expect(ok).To(BeTrue())
				g.Expect(tokens.Sign()).To(BeNumerically(">", 0))
				joinedValidator = *validator
			}).Should(Succeed())

			Eventually(func(g Gomega) {
				currentNode := &appsv1.ChainNode{}
				g.Expect(Framework().Client().Get(
					Framework().Context(), client.ObjectKeyFromObject(joiningNode), currentNode,
				)).To(Succeed())
				g.Expect(currentNode.Status.ValidatorAddress).To(Equal(joinedValidator.OperatorAddress))
				g.Expect(currentNode.Status.Validator).To(BeTrue())
				g.Expect(currentNode.Status.AccountAddress).To(Equal(joiningAccount.Address))
				g.Expect(currentNode.Status.ValidatorStatus).To(Equal(appsv1.ValidatorStatus(appsv1.ValidatorStatusBonded)))
				g.Expect(currentNode.Status.Jailed).To(BeFalse())
				pubKeysMatch, err := consensusPubKeysMatch(json.RawMessage(currentNode.Status.PubKey), joinedValidator.ConsensusPubKey)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(pubKeysMatch).To(BeTrue())

				currentSet := &appsv1.ChainNodeSet{}
				g.Expect(Framework().Client().Get(
					Framework().Context(), client.ObjectKeyFromObject(chainNodeSet), currentSet,
				)).To(Succeed())
				g.Expect(currentSet.Status.Phase).To(Equal(appsv1.PhaseChainNodeSetRunning))
				g.Expect(currentSet.Status.Instances).To(Equal(2))
				g.Expect(currentSet.Status.ReadyInstances).To(Equal(2))
				g.Expect(currentSet.Status.Nodes).To(HaveLen(2))
				g.Expect(currentSet.Status.Validators).To(HaveLen(2))

				var parentValidator *appsv1.ChainNodeSetValidatorStatus
				for i := range currentSet.Status.Validators {
					if currentSet.Status.Validators[i].Name == joiningValidatorName {
						parentValidator = &currentSet.Status.Validators[i]
						break
					}
				}
				g.Expect(parentValidator).NotTo(BeNil())
				g.Expect(parentValidator.Group).To(Equal(joiningValidatorGroup))
				g.Expect(parentValidator.Init).To(BeFalse())
				g.Expect(parentValidator.Address).To(Equal(joinedValidator.OperatorAddress))
				g.Expect(parentValidator.Status).To(Equal(appsv1.ValidatorStatus(appsv1.ValidatorStatusBonded)))
				pubKeysMatch, err = consensusPubKeysMatch(json.RawMessage(parentValidator.PubKey), joinedValidator.ConsensusPubKey)
				g.Expect(err).NotTo(HaveOccurred())
				g.Expect(pubKeysMatch).To(BeTrue())
			}).Should(Succeed())

			genesisNode := &appsv1.ChainNode{}
			Expect(Framework().Client().Get(
				Framework().Context(), client.ObjectKey{Namespace: ns.Name, Name: genesisValidatorName}, genesisNode,
			)).To(Succeed())
			RefreshChainNode(joiningNode)
			RefreshChainNodeSet(chainNodeSet)
			genesisHeight := genesisNode.Status.LatestHeight
			joiningHeight := joiningNode.Status.LatestHeight
			chainNodeSetHeight := chainNodeSet.Status.LatestHeight

			Eventually(func(g Gomega) {
				g.Expect(Framework().Client().Get(
					Framework().Context(), client.ObjectKeyFromObject(genesisNode), genesisNode,
				)).To(Succeed())
				g.Expect(Framework().Client().Get(
					Framework().Context(), client.ObjectKeyFromObject(joiningNode), joiningNode,
				)).To(Succeed())
				g.Expect(Framework().Client().Get(
					Framework().Context(), client.ObjectKeyFromObject(chainNodeSet), chainNodeSet,
				)).To(Succeed())
				g.Expect(genesisNode.Status.LatestHeight).To(BeNumerically(">", genesisHeight))
				g.Expect(joiningNode.Status.LatestHeight).To(BeNumerically(">", joiningHeight))
				g.Expect(chainNodeSet.Status.LatestHeight).To(BeNumerically(">", chainNodeSetHeight))
			}).Should(Succeed())
		}))
	}
})

func queryStakingValidators(namespace, podName, appBinary string) (*stakingValidatorsResponse, error) {
	output, err := Framework().PodExec(namespace, podName, appBinary,
		appBinary, "query", "staking", "validators", "--home", "/home/app",
		"--node", "tcp://localhost:26657", "--output", "json")
	if err != nil {
		return nil, fmt.Errorf("query staking validators: %w", err)
	}

	response := &stakingValidatorsResponse{}
	if err := json.Unmarshal([]byte(output), response); err != nil {
		return nil, fmt.Errorf("decode staking validators output: %w", err)
	}
	return response, nil
}

func queryBankBalances(namespace, podName, appBinary, address string) (*bankBalancesResponse, error) {
	output, err := Framework().PodExec(namespace, podName, appBinary,
		appBinary, "query", "bank", "balances", address, "--home", "/home/app",
		"--node", "tcp://localhost:26657", "--output", "json")
	if err != nil {
		return nil, fmt.Errorf("query bank balances for %s: %w", address, err)
	}

	response := &bankBalancesResponse{}
	if err := json.Unmarshal([]byte(output), response); err != nil {
		return nil, fmt.Errorf("decode bank balances output: %w", err)
	}
	return response, nil
}

func findStakingValidator(validators []stakingValidator, operatorAddress string) (*stakingValidator, bool) {
	for i := range validators {
		if validators[i].OperatorAddress == operatorAddress {
			return &validators[i], true
		}
	}
	return nil, false
}

func findBankBalance(balances []bankBalance, denom string) (string, bool) {
	for _, balance := range balances {
		if balance.Denom == denom {
			return balance.Amount, true
		}
	}
	return "", false
}
