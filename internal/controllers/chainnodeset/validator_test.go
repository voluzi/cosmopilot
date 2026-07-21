package chainnodeset

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	k8sappsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
)

func newValidatorTestReconciler(t *testing.T, objs ...client.Object) *Reconciler {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, appsv1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, discoveryv1.AddToScheme(scheme))
	require.NoError(t, k8sappsv1.AddToScheme(scheme))

	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&appsv1.ChainNodeSet{}).
		WithObjects(objs...).
		Build()

	return &Reconciler{
		Client:   cl,
		Scheme:   scheme,
		recorder: record.NewFakeRecorder(100),
		opts:     &controllers.ControllerRunOptions{},
	}
}

// TestGetValidatorSpecGroupValidators verifies that group validators produce ChainNode
// specs with the expected per-instance names, validator labels (including group and
// validator=true), and validator config mapped from the group's NodeSetValidatorConfig.
func TestGetValidatorSpecGroupValidators(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-nodeset",
			Namespace: "default",
			UID:       types.UID("test-uid"),
		},
		Spec: appsv1.ChainNodeSetSpec{
			Genesis: &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
			Nodes: []appsv1.NodeGroupSpec{
				{
					Name:      "validators",
					Instances: ptr.To(2),
					Validator: &appsv1.NodeSetValidatorConfig{
						AccountPrefix: ptr.To("cosmos"),
						ValPrefix:     ptr.To("cosmosvaloper"),
					},
				},
			},
		},
		Status: appsv1.ChainNodeSetStatus{ChainID: "test-chain"},
	}

	r := newValidatorTestReconciler(t, nodeSet)

	group := nodeSet.Spec.Nodes[0]
	for i := 0; i < group.GetInstances(); i++ {
		validator, err := r.getValidatorSpec(nodeSet, group.Name, i, group.Validator)
		require.NoError(t, err)

		assert.Equal(t, validatorNodeName(nodeSet, group.Name, i), validator.Name)
		assert.Equal(t, "default", validator.Namespace)

		assert.Equal(t, "test-nodeset", validator.Labels[controllers.LabelChainNodeSet])
		assert.Equal(t, "validators", validator.Labels[controllers.LabelChainNodeSetGroup])
		assert.Equal(t, controllers.StringValueTrue, validator.Labels[controllers.LabelChainNodeSetValidator])

		require.NotNil(t, validator.Spec.Validator)
		require.NotNil(t, validator.Spec.Validator.AccountPrefix)
		assert.Equal(t, "cosmos", *validator.Spec.Validator.AccountPrefix)
		require.NotNil(t, validator.Spec.Validator.ValPrefix)
		assert.Equal(t, "cosmosvaloper", *validator.Spec.Validator.ValPrefix)

		// Each validator ChainNode must be owned by the ChainNodeSet.
		require.Len(t, validator.OwnerReferences, 1)
		assert.Equal(t, "test-nodeset", validator.OwnerReferences[0].Name)
	}

	// The two instances must have distinct names so they never collide.
	v0, err := r.getValidatorSpec(nodeSet, group.Name, 0, group.Validator)
	require.NoError(t, err)
	v1, err := r.getValidatorSpec(nodeSet, group.Name, 1, group.Validator)
	require.NoError(t, err)
	assert.NotEqual(t, v0.Name, v1.Name)
}

// TestDeriveGroupValidatorConfigInitWithMultipleInstances verifies the per-instance
// validator config derivation for a genesis-initializing group with multiple instances:
// instance 0 keeps Init and records the other validators in Init.GenesisValidators (so they
// become actual genesis validators), while instances > 0 get Init cleared.
func TestDeriveGroupValidatorConfigInitWithMultipleInstances(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default"},
	}
	cfg := &appsv1.NodeSetValidatorConfig{
		Init: &appsv1.GenesisInitConfig{
			ChainID:     "test-chain",
			Assets:      []string{"1000000stake"},
			StakeAmount: "900000stake",
		},
	}

	const instances = 3

	// Instance 0 keeps Init and records the other two validators as genesis validators,
	// referencing the deterministic secret names of their generated ChainNodes.
	v0 := deriveGroupValidatorConfig(nodeSet, "validators", 0, instances, cfg)
	require.NotNil(t, v0.Init)
	require.Len(t, v0.Init.GenesisValidators, 2)

	assert.Equal(t, "test-nodeset-validators-1", v0.Init.GenesisValidators[0].Moniker)
	assert.Equal(t, "test-nodeset-validators-1-account", v0.Init.GenesisValidators[0].AccountMnemonicSecret)
	assert.Equal(t, "test-nodeset-validators-1-priv-key", v0.Init.GenesisValidators[0].PrivKeySecret)
	assert.Equal(t, []string{"1000000stake"}, v0.Init.GenesisValidators[0].Assets)
	assert.Equal(t, "900000stake", v0.Init.GenesisValidators[0].StakeAmount)

	assert.Equal(t, "test-nodeset-validators-2", v0.Init.GenesisValidators[1].Moniker)
	assert.Equal(t, "test-nodeset-validators-2-account", v0.Init.GenesisValidators[1].AccountMnemonicSecret)
	assert.Equal(t, "test-nodeset-validators-2-priv-key", v0.Init.GenesisValidators[1].PrivKeySecret)

	// Instances 1 and 2 have Init cleared so they consume the generated genesis.
	for _, i := range []int{1, 2} {
		v := deriveGroupValidatorConfig(nodeSet, "validators", i, instances, cfg)
		assert.Nil(t, v.Init, "instance %d must not initialize genesis", i)
	}

	// The user's original config must not be mutated.
	assert.Empty(t, cfg.Init.GenesisValidators, "original config must not be mutated")
}

// TestGroupGenesisValidators verifies the genesis validator list returned for a group: one
// entry per non-init instance, with deterministic secret names, and nil for groups that do
// not initialize genesis or have a single instance.
func TestGroupGenesisValidators(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default"},
	}
	cfg := &appsv1.NodeSetValidatorConfig{
		Init: &appsv1.GenesisInitConfig{
			ChainID:     "test-chain",
			Assets:      []string{"1000000stake"},
			StakeAmount: "900000stake",
		},
	}

	gvs := groupGenesisValidators(nodeSet, "validators", 3, cfg)
	require.Len(t, gvs, 2)
	assert.Equal(t, "test-nodeset-validators-1", gvs[0].Moniker)
	assert.Equal(t, "test-nodeset-validators-2", gvs[1].Moniker)

	// No genesis validators when the group does not initialize genesis or has a single instance.
	assert.Nil(t, groupGenesisValidators(nodeSet, "validators", 1, cfg))
	assert.Nil(t, groupGenesisValidators(nodeSet, "validators", 3, &appsv1.NodeSetValidatorConfig{}))
}

// TestDeriveGroupValidatorConfigNoInitOrSingleInstance verifies the config is returned
// unchanged for groups that do not initialize genesis or have a single instance.
func TestDeriveGroupValidatorConfigNoInitOrSingleInstance(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset"}}

	noInit := &appsv1.NodeSetValidatorConfig{}
	assert.Same(t, noInit, deriveGroupValidatorConfig(nodeSet, "validators", 1, 3, noInit))

	withInit := &appsv1.NodeSetValidatorConfig{Init: &appsv1.GenesisInitConfig{ChainID: "c"}}
	assert.Same(t, withInit, deriveGroupValidatorConfig(nodeSet, "validators", 0, 1, withInit))
}

// TestDeriveGroupValidatorConfigCosmosignerTargeted verifies the one-identity derivation for a
// multi-instance validator group targeted by a cosmosigner: the group is ONE validator (the
// signer's identity) with redundant signing endpoints, so no sibling genesis validators are
// recorded on instance 0, instances > 0 get Init cleared, and CreateValidator only survives on
// instance 0 (N registration flows would race to register the same pubkey).
func TestDeriveGroupValidatorConfigCosmosignerTargeted(t *testing.T) {
	vaultBackend := appsv1.CosmosignerBackend{Vault: &appsv1.CosmosignerVaultBackend{
		Address:     "https://vault:8200",
		KeyName:     "k",
		TokenSecret: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "vault-token"}, Key: "token"},
	}}
	mk := func(cfg *appsv1.NodeSetValidatorConfig) *appsv1.ChainNodeSet {
		return &appsv1.ChainNodeSet{
			ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default"},
			Spec: appsv1.ChainNodeSetSpec{
				Nodes: []appsv1.NodeGroupSpec{{
					Name:        "validators",
					Instances:   ptr.To(3),
					Validator:   cfg,
					Cosmosigner: &appsv1.Cosmosigner{Backend: vaultBackend},
				}},
			},
		}
	}

	// Genesis-init group: instance 0 keeps Init with NO sibling genesis validators; others cleared.
	initCfg := &appsv1.NodeSetValidatorConfig{
		Init: &appsv1.GenesisInitConfig{ChainID: "test-chain", Assets: []string{"1stake"}, StakeAmount: "1stake"},
	}
	nodeSet := mk(initCfg)
	v0 := deriveGroupValidatorConfig(nodeSet, "validators", 0, 3, initCfg)
	require.NotNil(t, v0.Init)
	assert.Empty(t, v0.Init.GenesisValidators, "no sibling genesis validators for a signer-targeted group")
	for _, i := range []int{1, 2} {
		v := deriveGroupValidatorConfig(nodeSet, "validators", i, 3, initCfg)
		assert.Nil(t, v.Init, "instance %d must not initialize genesis", i)
	}
	// groupGenesisValidators must not expand either (so no per-instance secrets are minted).
	assert.Nil(t, groupGenesisValidators(nodeSet, "validators", 3, initCfg))

	// CreateValidator group: only instance 0 keeps the registration flow.
	cvCfg := &appsv1.NodeSetValidatorConfig{CreateValidator: &appsv1.CreateValidatorConfig{}}
	nodeSet = mk(cvCfg)
	v0 = deriveGroupValidatorConfig(nodeSet, "validators", 0, 3, cvCfg)
	assert.NotNil(t, v0.CreateValidator, "instance 0 keeps createValidator")
	for _, i := range []int{1, 2} {
		v := deriveGroupValidatorConfig(nodeSet, "validators", i, 3, cvCfg)
		assert.Nil(t, v.CreateValidator, "instance %d must not run createValidator", i)
	}
	assert.NotNil(t, cvCfg.CreateValidator, "original config must not be mutated")
}

// TestSignerNameForNodeMultiInstanceValidatorGroup verifies every instance of a signer-targeted
// multi-instance validator group maps to the group's single signer (all N pods are redundant
// signing endpoints of the same identity and must carry the discovery label).
func TestSignerNameForNodeMultiInstanceValidatorGroup(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "cs"},
		Spec: appsv1.ChainNodeSetSpec{
			Nodes: []appsv1.NodeGroupSpec{{
				Name: "vg", Instances: ptr.To(3), Validator: &appsv1.NodeSetValidatorConfig{},
				Cosmosigner: &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{Vault: &appsv1.CosmosignerVaultBackend{
					Address: "https://v:8200", KeyName: "k",
					TokenSecret: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "t"}, Key: "token"},
				}}},
			}},
		},
	}
	name, ok := signerNameForNode(nodeSet, "vg")
	require.True(t, ok)
	assert.Equal(t, "cs-vg-signer", name, "the group's single signer dials every instance")
}

// TestEnsureGenesisValidatorSecrets verifies the extra genesis validators' account and
// priv-key secrets are created deterministically with the expected keys, and that existing
// secrets are left untouched (key material is stable across reconciles).
func TestEnsureGenesisValidatorSecrets(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: types.UID("u")},
	}
	cfg := &appsv1.NodeSetValidatorConfig{
		Init: &appsv1.GenesisInitConfig{
			ChainID:     "test-chain",
			Assets:      []string{"1000000stake"},
			StakeAmount: "900000stake",
		},
	}

	r := newValidatorTestReconciler(t, nodeSet)
	ctx := context.Background()

	gvs := groupGenesisValidators(nodeSet, "validators", 3, cfg)
	require.Len(t, gvs, 2)

	require.NoError(t, r.ensureGenesisValidatorSecrets(ctx, nodeSet, cfg, gvs))

	// Both account and priv-key secrets are created with the expected data keys, owned by the set.
	mnemonics := map[string]string{}
	for _, gv := range gvs {
		acc := &corev1.Secret{}
		require.NoError(t, r.Get(ctx, types.NamespacedName{Namespace: "default", Name: gv.AccountMnemonicSecret}, acc))
		require.Contains(t, acc.Data, mnemonicKey)
		assert.NotEmpty(t, acc.Data[mnemonicKey])
		require.Len(t, acc.OwnerReferences, 1)
		assert.Equal(t, "test-nodeset", acc.OwnerReferences[0].Name)
		mnemonics[gv.AccountMnemonicSecret] = string(acc.Data[mnemonicKey])

		pk := &corev1.Secret{}
		require.NoError(t, r.Get(ctx, types.NamespacedName{Namespace: "default", Name: gv.PrivKeySecret}, pk))
		require.Contains(t, pk.Data, privKeyFilename)
		assert.NotEmpty(t, pk.Data[privKeyFilename])
	}

	// Re-running must not regenerate existing secrets.
	require.NoError(t, r.ensureGenesisValidatorSecrets(ctx, nodeSet, cfg, gvs))
	for _, gv := range gvs {
		acc := &corev1.Secret{}
		require.NoError(t, r.Get(ctx, types.NamespacedName{Namespace: "default", Name: gv.AccountMnemonicSecret}, acc))
		assert.Equal(t, mnemonics[gv.AccountMnemonicSecret], string(acc.Data[mnemonicKey]), "mnemonic must be stable across reconciles")
	}
}

// TestEnsureValidatorDoesNotRegenerateGenesisSecretsAfterGenesis verifies that, once genesis has been
// produced (Status.ChainID set), ensureValidator does not (re)create the genesis validator secrets for
// a multi-instance validator.init group. Their consensus keys are baked into the immutable genesis
// validator set, so regenerating an accidentally-deleted secret would mint a fresh key absent from
// genesis. Before this guard, ensureGenesisValidatorSecrets ran on every reconcile.
func TestEnsureValidatorDoesNotRegenerateGenesisSecretsAfterGenesis(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: types.UID("u")},
		Spec: appsv1.ChainNodeSetSpec{
			Nodes: []appsv1.NodeGroupSpec{{
				Name:      "validators",
				Instances: ptr.To(2),
				Validator: &appsv1.NodeSetValidatorConfig{
					Init: &appsv1.GenesisInitConfig{
						ChainID:     "test-chain",
						Assets:      []string{"1000000stake"},
						StakeAmount: "900000stake",
					},
				},
			}},
		},
		// Genesis already produced: ChainID is set.
		Status: appsv1.ChainNodeSetStatus{ChainID: "test-chain"},
	}

	r := newValidatorTestReconciler(t, nodeSet)
	require.NoError(t, r.ensureValidator(context.Background(), nodeSet))

	// The extra genesis validators' account and priv-key secrets must NOT be created after genesis
	// exists. ensureValidator only creates the validator ChainNodes here; the only path that would
	// create these secrets is ensureGenesisValidatorSecrets, which the guard now skips.
	gvs := groupGenesisValidators(nodeSet, "validators", 2, nodeSet.Spec.Nodes[0].Validator)
	require.NotEmpty(t, gvs, "a 2-instance init group has one extra genesis validator")
	for _, gv := range gvs {
		err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: gv.AccountMnemonicSecret}, &corev1.Secret{})
		assert.True(t, errors.IsNotFound(err), "account secret %s must not be regenerated after genesis", gv.AccountMnemonicSecret)

		err = r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: gv.PrivKeySecret}, &corev1.Secret{})
		assert.True(t, errors.IsNotFound(err), "priv-key secret %s must not be regenerated after genesis", gv.PrivKeySecret)
	}
}

// TestValidatorWaitMode verifies the wait condition for a validator: a single-instance genesis
// validator with no chainID yet waits until running/syncing; a multi-instance genesis validator
// waits only until the genesis is ready (it cannot reach running until its peers exist); and any
// validator either not initializing genesis or running once a chainID is known does not wait.
func TestValidatorWaitMode(t *testing.T) {
	withInit := &appsv1.NodeSetValidatorConfig{Init: &appsv1.GenesisInitConfig{ChainID: "c"}}
	noInit := &appsv1.NodeSetValidatorConfig{}

	emptyChainID := &appsv1.ChainNodeSet{Status: appsv1.ChainNodeSetStatus{ChainID: ""}}
	knownChainID := &appsv1.ChainNodeSet{Status: appsv1.ChainNodeSetStatus{ChainID: "test-chain"}}

	// Single-instance genesis validator with no chainID: wait until running/syncing.
	assert.Equal(t, waitRunningOrSyncing, validatorWaitMode(emptyChainID, withInit, 1, "validators"))
	// Multi-instance genesis validator with no chainID: wait only for genesis readiness.
	assert.Equal(t, waitGenesisReady, validatorWaitMode(emptyChainID, withInit, 3, "validators"))
	// Genesis already known: never wait, regardless of instances.
	assert.Equal(t, waitNone, validatorWaitMode(knownChainID, withInit, 1, "validators"))
	assert.Equal(t, waitNone, validatorWaitMode(knownChainID, withInit, 3, "validators"))
	// Non-init validators never wait.
	assert.Equal(t, waitNone, validatorWaitMode(emptyChainID, noInit, 1, "validators"))
	assert.Equal(t, waitNone, validatorWaitMode(knownChainID, noInit, 3, "validators"))

	// A cosmosigner-targeted single-instance init validator waits only for genesis readiness (it
	// cannot run before the signer is deployed).
	cosmosignerTargeted := &appsv1.ChainNodeSet{
		Spec: appsv1.ChainNodeSetSpec{
			Cosmosigner: &appsv1.Cosmosigner{NodeGroups: []string{"validators"}},
		},
	}
	assert.Equal(t, waitGenesisReady, validatorWaitMode(cosmosignerTargeted, withInit, 1, "validators"))
}

// TestGetValidatorSpecNonInitGenesisFromChainID verifies a non-init group validator's
// spec uses a ConfigMap genesis source derived from the ChainNodeSet chainID and clears
// Init, while the init validator gets a nil genesis (init and genesis are mutually
// exclusive).
func TestGetValidatorSpecNonInitGenesisFromChainID(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: types.UID("u")},
		Spec: appsv1.ChainNodeSetSpec{
			Nodes: []appsv1.NodeGroupSpec{{
				Name:      "validators",
				Instances: ptr.To(3),
				Validator: &appsv1.NodeSetValidatorConfig{
					Init: &appsv1.GenesisInitConfig{ChainID: "test-chain", Assets: []string{"1stake"}},
				},
			}},
		},
		Status: appsv1.ChainNodeSetStatus{ChainID: "test-chain"},
	}
	r := newValidatorTestReconciler(t, nodeSet)
	group := nodeSet.Spec.Nodes[0]

	// Init validator: genesis must be nil.
	cfg0 := deriveGroupValidatorConfig(nodeSet, group.Name, 0, 3, group.Validator)
	v0, err := r.getValidatorSpec(nodeSet, group.Name, 0, cfg0)
	require.NoError(t, err)
	assert.Nil(t, v0.Spec.Genesis)
	require.NotNil(t, v0.Spec.Validator.Init)

	// Non-init validator: genesis must point to the chainID-derived ConfigMap and Init nil.
	cfg1 := deriveGroupValidatorConfig(nodeSet, group.Name, 1, 3, group.Validator)
	v1, err := r.getValidatorSpec(nodeSet, group.Name, 1, cfg1)
	require.NoError(t, err)
	require.NotNil(t, v1.Spec.Genesis)
	require.NotNil(t, v1.Spec.Genesis.ConfigMap)
	assert.Equal(t, "test-chain-genesis", *v1.Spec.Genesis.ConfigMap)
	assert.Nil(t, v1.Spec.Validator.Init)
}

// TestDeriveGroupValidatorConfigPreservesAccountSettings verifies that account derivation
// settings configured under .init are pinned onto derived non-init validators (which clear
// .init), so their account/valoper identity matches the entry the init validator recorded in
// genesis.
func TestDeriveGroupValidatorConfigPreservesAccountSettings(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: types.UID("u")},
	}
	group := appsv1.NodeGroupSpec{
		Name:      "validators",
		Instances: ptr.To(2),
		Validator: &appsv1.NodeSetValidatorConfig{
			Init: &appsv1.GenesisInitConfig{
				ChainID:       "test-chain",
				AccountHDPath: ptr.To("m/44'/60'/0'/0/0"),
				AccountPrefix: ptr.To("evmos"),
				ValPrefix:     ptr.To("evmosvaloper"),
			},
		},
	}

	derived := deriveGroupValidatorConfig(nodeSet, group.Name, 1, 2, group.Validator)
	require.Nil(t, derived.Init)
	require.NotNil(t, derived.AccountHDPath)
	assert.Equal(t, "m/44'/60'/0'/0/0", *derived.AccountHDPath)
	require.NotNil(t, derived.AccountPrefix)
	assert.Equal(t, "evmos", *derived.AccountPrefix)
	require.NotNil(t, derived.ValPrefix)
	assert.Equal(t, "evmosvaloper", *derived.ValPrefix)

	// The resolved settings must match the init validator's, so both derive identical addresses.
	assert.Equal(t, group.Validator.GetAccountHDPath(), derived.GetAccountHDPath())
	assert.Equal(t, group.Validator.GetAccountPrefix(), derived.GetAccountPrefix())
	assert.Equal(t, group.Validator.GetValPrefix(), derived.GetValPrefix())

	// The original config must not be mutated.
	assert.Nil(t, group.Validator.AccountPrefix)
}

// TestGetValidatorSpecGlobalRouteLabels verifies validator-group ChainNodes carry the same global
// ingress/gateway membership labels regular group nodes get, so global Services select them.
func TestGetValidatorSpecGlobalRouteLabels(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: types.UID("u")},
		Spec: appsv1.ChainNodeSetSpec{
			Ingresses: []appsv1.GlobalIngressConfig{
				{Name: "public", Groups: []string{"validators"}},
				{Name: "other", Groups: []string{"fullnodes"}},
			},
			GatewayRoutes: []appsv1.GlobalGatewayConfig{
				{Name: "gw", Groups: []string{"validators"}},
			},
			Nodes: []appsv1.NodeGroupSpec{{
				Name:      "validators",
				Instances: ptr.To(1),
				Validator: &appsv1.NodeSetValidatorConfig{},
			}},
		},
		Status: appsv1.ChainNodeSetStatus{ChainID: "test-chain"},
	}
	r := newValidatorTestReconciler(t, nodeSet)

	v, err := r.getValidatorSpec(nodeSet, "validators", 0, nodeSet.Spec.Nodes[0].Validator)
	require.NoError(t, err)

	ingress := nodeSet.Spec.Ingresses[0]
	assert.Equal(t, "true", v.Labels[ingress.GetName(nodeSet)], "validator node must carry its global ingress label")
	other := nodeSet.Spec.Ingresses[1]
	assert.NotContains(t, v.Labels, other.GetName(nodeSet), "validator node must not carry an unrelated group's ingress label")
	gw := nodeSet.Spec.GatewayRoutes[0]
	assert.Equal(t, "true", v.Labels[gw.GetName(nodeSet)], "validator node must carry its global gateway label")
}

// TestEnsureValidatorPropagatesGroupPeers verifies group-level persistent peers are carried onto
// the generated validator-group ChainNodes so they can join an external network.
func TestEnsureValidatorPropagatesGroupPeers(t *testing.T) {
	peers := []appsv1.Peer{{ID: "abc123", Address: "peer.example.com"}}
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: types.UID("u")},
		Spec: appsv1.ChainNodeSetSpec{
			Genesis: &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
			Nodes: []appsv1.NodeGroupSpec{{
				Name:      "validators",
				Instances: ptr.To(1),
				Peers:     peers,
				Validator: &appsv1.NodeSetValidatorConfig{},
			}},
		},
		Status: appsv1.ChainNodeSetStatus{ChainID: "test-chain"},
	}
	r := newValidatorTestReconciler(t, nodeSet)

	require.NoError(t, r.ensureValidator(context.Background(), nodeSet))

	created := &appsv1.ChainNode{}
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-nodeset-validators-0"}, created))
	assert.Equal(t, peers, created.Spec.Peers)
}

// TestEnsureValidatorPropagatesGroupExpose verifies that group-level P2P exposure
// (.spec.nodes[].expose) is carried onto the generated validator-group ChainNodes, with the same
// per-instance gateway port offset regular group nodes get.
func TestEnsureValidatorPropagatesGroupExpose(t *testing.T) {
	expose := &appsv1.ExposeConfig{
		Gateway: &appsv1.ExposeGatewayConfig{
			GatewayRef: appsv1.GatewayRef{Name: "p2p-gw"},
			Port:       ptr.To(int32(30000)),
		},
	}
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: types.UID("u")},
		Spec: appsv1.ChainNodeSetSpec{
			Genesis: &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
			Nodes: []appsv1.NodeGroupSpec{{
				Name:      "validators",
				Instances: ptr.To(2),
				Expose:    expose,
				Validator: &appsv1.NodeSetValidatorConfig{},
			}},
		},
		Status: appsv1.ChainNodeSetStatus{ChainID: "test-chain"},
	}
	r := newValidatorTestReconciler(t, nodeSet)

	require.NoError(t, r.ensureValidator(context.Background(), nodeSet))

	// Each instance gets the expose config with its gateway port offset by the instance index.
	for i, name := range []string{"test-nodeset-validators-0", "test-nodeset-validators-1"} {
		created := &appsv1.ChainNode{}
		require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, created))
		require.NotNil(t, created.Spec.Expose, "validator %s must carry expose config", name)
		require.NotNil(t, created.Spec.Expose.Gateway)
		require.NotNil(t, created.Spec.Expose.Gateway.Port)
		assert.Equal(t, int32(30000+i), *created.Spec.Expose.Gateway.Port)
	}

	// The user's original expose config must not be mutated by the per-instance offset.
	require.NotNil(t, expose.Gateway.Port)
	assert.Equal(t, int32(30000), *expose.Gateway.Port, "original expose config must not be mutated")
}

// TestEnsureValidatorRemovesStaleValidator verifies that when no validator is desired anymore,
// ensureValidator deletes the leftover validator ChainNode and its status entry.
func TestEnsureValidatorRemovesStaleValidator(t *testing.T) {
	stale := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-nodeset-validator",
			Namespace: "default",
			Labels: map[string]string{
				controllers.LabelChainNodeSet:          "test-nodeset",
				controllers.LabelChainNodeSetValidator: "true",
			},
		},
	}
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: types.UID("u")},
		Spec: appsv1.ChainNodeSetSpec{
			Genesis: &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
			Nodes:   []appsv1.NodeGroupSpec{{Name: "fullnodes", Instances: ptr.To(1)}},
		},
		Status: appsv1.ChainNodeSetStatus{
			ChainID:    "test-chain",
			Validators: []appsv1.ChainNodeSetValidatorStatus{{Name: "test-nodeset-validator", Group: validatorGroupName}},
		},
	}
	r := newValidatorTestReconciler(t, nodeSet, stale)

	require.NoError(t, r.ensureValidator(context.Background(), nodeSet))

	err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "test-nodeset-validator"}, &appsv1.ChainNode{})
	assert.True(t, errors.IsNotFound(err), "stale validator ChainNode must be deleted")
	assert.Empty(t, nodeSet.Status.Validators, "stale validator status must be removed")
}

func TestEnsureValidatorPreservesGenesisBaselineWhileUpdatingLiveStatus(t *testing.T) {
	initCfg := &appsv1.NodeSetValidatorConfig{Init: &appsv1.GenesisInitConfig{
		ChainID:     "test-chain",
		Assets:      []string{"1000000stake"},
		StakeAmount: "900000stake",
	}}
	name := "test-nodeset-validators-0"
	digest := initCfg.GenesisSigningFingerprint(name + "-priv-key")
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: types.UID("u")},
		Spec: appsv1.ChainNodeSetSpec{
			Genesis: &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
			Nodes: []appsv1.NodeGroupSpec{{
				Name:      "validators",
				Instances: ptr.To(1),
				Validator: &appsv1.NodeSetValidatorConfig{CreateValidator: &appsv1.CreateValidatorConfig{}},
			}},
		},
		Status: appsv1.ChainNodeSetStatus{
			ChainID: "test-chain",
			Validators: []appsv1.ChainNodeSetValidatorStatus{{
				Name:             name,
				Group:            "validators",
				Address:          "old-address",
				Status:           appsv1.ValidatorStatusUnbonded,
				PubKey:           "old-pubkey",
				Init:             true,
				SigningKeyDigest: digest,
			}},
		},
	}
	r := newValidatorTestReconciler(t, nodeSet)
	current, err := r.getValidatorSpec(nodeSet, "validators", 0, nodeSet.Spec.Nodes[0].Validator)
	require.NoError(t, err)
	current.Status.ValidatorAddress = "new-address"
	current.Status.ValidatorStatus = appsv1.ValidatorStatusBonded
	current.Status.PubKey = "new-pubkey"
	require.NoError(t, r.Create(context.Background(), current))

	require.NoError(t, r.ensureValidator(context.Background(), nodeSet))

	require.Len(t, nodeSet.Status.Validators, 1)
	got := nodeSet.Status.Validators[0]
	assert.Equal(t, name, got.Name)
	assert.Equal(t, "validators", got.Group)
	assert.True(t, got.Init)
	assert.Equal(t, digest, got.SigningKeyDigest)
	assert.Equal(t, "new-address", got.Address)
	assert.Equal(t, appsv1.ValidatorStatus(appsv1.ValidatorStatusBonded), got.Status)
	assert.Equal(t, "new-pubkey", got.PubKey)
}

func TestEnsureValidatorBackfillsEmptyInitGeneratedBaseline(t *testing.T) {
	initCfg := &appsv1.NodeSetValidatorConfig{Init: &appsv1.GenesisInitConfig{
		ChainID:     "test-chain",
		Assets:      []string{"1000000stake"},
		StakeAmount: "900000stake",
	}}
	name := "test-nodeset-validators-0"
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: types.UID("u")},
		Spec: appsv1.ChainNodeSetSpec{Nodes: []appsv1.NodeGroupSpec{{
			Name:      "validators",
			Instances: ptr.To(1),
			Validator: initCfg,
		}}},
		Status: appsv1.ChainNodeSetStatus{
			ChainID:              "test-chain",
			GenesisInitGenerated: ptr.To(true),
		},
	}
	r := newValidatorTestReconciler(t, nodeSet)

	require.NoError(t, r.ensureValidator(context.Background(), nodeSet))

	require.Len(t, nodeSet.Status.Validators, 1)
	require.Equal(t, name, nodeSet.Status.Validators[0].Name)
	require.True(t, nodeSet.Status.Validators[0].Init)
	require.Equal(t, initCfg.GenesisSigningFingerprint(name+"-priv-key"), nodeSet.Status.Validators[0].SigningKeyDigest)
}

func TestEnsureValidatorPreservesRemovedGenesisBaseline(t *testing.T) {
	stale := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-nodeset-validators-0",
			Namespace: "default",
			Labels: map[string]string{
				controllers.LabelChainNodeSet:          "test-nodeset",
				controllers.LabelChainNodeSetGroup:     "validators",
				controllers.LabelChainNodeSetValidator: controllers.StringValueTrue,
			},
		},
	}
	baseline := appsv1.ChainNodeSetValidatorStatus{
		Name:             stale.Name,
		Group:            "validators",
		Init:             true,
		SigningKeyDigest: "original-digest",
	}
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: types.UID("u")},
		Spec: appsv1.ChainNodeSetSpec{
			Genesis: &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
		},
		Status: appsv1.ChainNodeSetStatus{
			ChainID:    "test-chain",
			Validators: []appsv1.ChainNodeSetValidatorStatus{baseline},
			Nodes:      []appsv1.ChainNodeSetNodeStatus{{Name: stale.Name, Group: "validators"}},
		},
	}
	r := newValidatorTestReconciler(t, nodeSet, stale)

	require.NoError(t, r.ensureValidator(context.Background(), nodeSet))

	err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: stale.Name}, &appsv1.ChainNode{})
	assert.True(t, errors.IsNotFound(err), "stale validator ChainNode must still be deleted")
	assert.Equal(t, []appsv1.ChainNodeSetValidatorStatus{baseline}, nodeSet.Status.Validators)
	assert.Empty(t, nodeSet.Status.Nodes)
}

func TestEnsureValidatorDoesNotExpandRecordedGenesisBaseline(t *testing.T) {
	init := &appsv1.GenesisInitConfig{
		ChainID:     "test-chain",
		Assets:      []string{"1000000stake"},
		StakeAmount: "900000stake",
	}
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: types.UID("u")},
		Spec: appsv1.ChainNodeSetSpec{Nodes: []appsv1.NodeGroupSpec{
			{Name: "original", Instances: ptr.To(1), Validator: &appsv1.NodeSetValidatorConfig{Init: init.DeepCopy()}},
			{Name: "injected", Instances: ptr.To(1), Validator: &appsv1.NodeSetValidatorConfig{Init: init.DeepCopy()}},
		}},
		Status: appsv1.ChainNodeSetStatus{
			ChainID:              "test-chain",
			GenesisInitGenerated: ptr.To(true),
			Validators: []appsv1.ChainNodeSetValidatorStatus{{
				Name:             "test-nodeset-original-0",
				Group:            "original",
				Init:             true,
				SigningKeyDigest: (&appsv1.NodeSetValidatorConfig{Init: init.DeepCopy()}).GenesisSigningFingerprint("test-nodeset-original-0-priv-key"),
			}},
		},
	}
	r := newValidatorTestReconciler(t, nodeSet)

	require.NoError(t, r.ensureValidator(context.Background(), nodeSet))

	require.Len(t, nodeSet.Status.Validators, 2)
	assert.True(t, nodeSet.Status.Validators[0].Init)
	assert.False(t, nodeSet.Status.Validators[1].Init)
	assert.Empty(t, nodeSet.Status.Validators[1].SigningKeyDigest)
}

func TestEnsureValidatorRefreshesGenesisDigestForManagedMigration(t *testing.T) {
	const vaultAddress = "https://vault.example.com:8200"
	const vaultKey = "validator-key"
	init := &appsv1.GenesisInitConfig{
		ChainID:     "test-chain",
		Assets:      []string{"1000000stake"},
		StakeAmount: "900000stake",
	}
	oldCfg := &appsv1.NodeSetValidatorConfig{
		Init: init.DeepCopy(),
		TmKMS: &appsv1.TmKMS{Provider: appsv1.TmKmsProvider{Hashicorp: &appsv1.TmKmsHashicorpProvider{
			Address: vaultAddress,
			Key:     vaultKey,
		}}},
	}

	for _, tc := range []struct {
		name            string
		disableWebhooks bool
		wantRefresh     bool
	}{
		{name: "webhook admitted migration refreshes", wantRefresh: true},
		{name: "no webhook migration refreshes", disableWebhooks: true, wantRefresh: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			currentCfg := &appsv1.NodeSetValidatorConfig{Init: init.DeepCopy()}
			nodeSet := &appsv1.ChainNodeSet{
				ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: types.UID("u")},
				Spec: appsv1.ChainNodeSetSpec{Nodes: []appsv1.NodeGroupSpec{{
					Name:      "validators",
					Instances: ptr.To(1),
					Validator: currentCfg,
					Cosmosigner: &appsv1.Cosmosigner{Backend: appsv1.CosmosignerBackend{
						Vault: &appsv1.CosmosignerVaultBackend{Address: vaultAddress, KeyName: vaultKey},
					}},
				}}},
				Status: appsv1.ChainNodeSetStatus{
					ChainID: "test-chain",
					Validators: []appsv1.ChainNodeSetValidatorStatus{{
						Name:             "test-nodeset-validators-0",
						Group:            "validators",
						Init:             true,
						SigningKeyDigest: oldCfg.GenesisSigningFingerprint("test-nodeset-validators-0-priv-key"),
					}},
				},
			}
			r := newValidatorTestReconciler(t, nodeSet)
			r.opts.DisableWebhooks = tc.disableWebhooks

			require.NoError(t, r.ensureValidator(context.Background(), nodeSet))

			require.Len(t, nodeSet.Status.Validators, 1)
			oldDigest := oldCfg.GenesisSigningFingerprint("test-nodeset-validators-0-priv-key")
			currentDigest := currentCfg.GenesisSigningFingerprint("test-nodeset-validators-0-priv-key")
			require.NotEqual(t, oldDigest, currentDigest)
			if tc.wantRefresh {
				assert.Equal(t, currentDigest, nodeSet.Status.Validators[0].SigningKeyDigest)
			} else {
				assert.Equal(t, oldDigest, nodeSet.Status.Validators[0].SigningKeyDigest)
			}
		})
	}
}

func TestGetNodeSpecNilGenesisUsesGeneratedConfigMap(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: types.UID("u")},
		Spec: appsv1.ChainNodeSetSpec{
			Nodes: []appsv1.NodeGroupSpec{{Name: "fullnodes", Instances: ptr.To(1)}},
		},
		Status: appsv1.ChainNodeSetStatus{ChainID: "test-chain"},
	}
	r := newValidatorTestReconciler(t, nodeSet)

	node, err := r.getNodeSpec(nodeSet, nodeSet.Spec.Nodes[0], 0)
	require.NoError(t, err)
	require.NotNil(t, node.Spec.Genesis)
	require.NotNil(t, node.Spec.Genesis.ConfigMap)
	assert.Equal(t, "test-chain-genesis", *node.Spec.Genesis.ConfigMap)
}

// TestEnsureValidatorPropagatesChainIDBeforeCreatingNonInitValidators reproduces the
// first reconcile of a multi-instance genesis-initializing validator group. The init
// validator reports the chainID while the ChainNodeSet status is still empty; the next
// non-init validator must consume <chainID>-genesis, never -genesis.
func TestEnsureValidatorPropagatesChainIDBeforeCreatingNonInitValidators(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: types.UID("u")},
		Spec: appsv1.ChainNodeSetSpec{
			Nodes: []appsv1.NodeGroupSpec{{
				Name:      "validators",
				Instances: ptr.To(2),
				Validator: &appsv1.NodeSetValidatorConfig{
					Init: &appsv1.GenesisInitConfig{
						ChainID:     "test-chain",
						Assets:      []string{"1000000stake"},
						StakeAmount: "900000stake",
					},
				},
			}},
		},
	}

	r := newValidatorTestReconciler(t, nodeSet)
	group := nodeSet.Spec.Nodes[0]

	initCfg := deriveGroupValidatorConfig(nodeSet, group.Name, 0, 2, group.Validator)
	initValidator, err := r.getValidatorSpec(nodeSet, group.Name, 0, initCfg)
	require.NoError(t, err)
	initValidator.Status.ChainID = "test-chain"

	// ensureValidator updates status immediately after the init validator is ensured, before
	// deriving the non-init validators.
	updateValidatorStatus(nodeSet, initValidator, initCfg, group.Name, true, true, nil, false, false)

	nonInitCfg := deriveGroupValidatorConfig(nodeSet, group.Name, 1, 2, group.Validator)
	nonInit, err := r.getValidatorSpec(nodeSet, group.Name, 1, nonInitCfg)
	require.NoError(t, err)
	require.NotNil(t, nonInit.Spec.Genesis)
	require.NotNil(t, nonInit.Spec.Genesis.ConfigMap)
	assert.Equal(t, "test-chain-genesis", *nonInit.Spec.Genesis.ConfigMap)
	assert.NotEqual(t, "-genesis", *nonInit.Spec.Genesis.ConfigMap)
}

// TestUpdateValidatorStatusLegacyAlias verifies that the legacy singleton status fields
// alias the first validator in spec order, even when that validator has not yet reported
// any status, and that later validators do not overwrite the alias.
func TestUpdateValidatorStatusLegacyAlias(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset"},
	}

	// First validator (spec order) has not reported any status yet.
	first := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset-validators-0"},
	}
	// Second validator already reports status.
	second := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset-validators-1"},
		Status: appsv1.ChainNodeStatus{
			ValidatorAddress: "addr-second",
			ValidatorStatus:  appsv1.ValidatorStatusBonded,
			PubKey:           "pubkey-second",
		},
	}

	updateValidatorStatus(nodeSet, first, nil, "validators", false, true, nil, false, false)
	updateValidatorStatus(nodeSet, second, nil, "validators", false, false, nil, false, false)

	// Legacy alias is pinned to the first validator even though it is empty, instead of
	// latching onto the second validator's reported values.
	assert.Equal(t, "", nodeSet.Status.ValidatorAddress)
	assert.Equal(t, "", nodeSet.Status.PubKey)
	assert.Equal(t, appsv1.ValidatorStatus(""), nodeSet.Status.ValidatorStatus)

	// Both validators are tracked in the full list.
	require.Len(t, nodeSet.Status.Validators, 2)

	// Updating the first validator's status (now reporting) refreshes the alias.
	first.Status.ValidatorAddress = "addr-first"
	first.Status.PubKey = "pubkey-first"
	updateValidatorStatus(nodeSet, first, nil, "validators", false, true, nil, false, false)
	assert.Equal(t, "addr-first", nodeSet.Status.ValidatorAddress)
	assert.Equal(t, "pubkey-first", nodeSet.Status.PubKey)
}

func TestEnsureValidatorLegacyAndGroupValidators(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: types.UID("u")},
		Spec: appsv1.ChainNodeSetSpec{
			Genesis:   &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
			Validator: &appsv1.NodeSetValidatorConfig{},
			Nodes: []appsv1.NodeGroupSpec{{
				Name:      "validators",
				Instances: ptr.To(2),
				Validator: &appsv1.NodeSetValidatorConfig{},
			}},
		},
		Status: appsv1.ChainNodeSetStatus{ChainID: "test-chain"},
	}

	r := newValidatorTestReconciler(t, nodeSet)
	require.NoError(t, r.ensureValidator(context.Background(), nodeSet))

	for _, name := range []string{"test-nodeset-validator", "test-nodeset-validators-0", "test-nodeset-validators-1"} {
		got := &appsv1.ChainNode{}
		require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, got))
	}

	require.Len(t, nodeSet.Status.Validators, 3)
	assert.Equal(t, "test-nodeset-validator", nodeSet.Status.Validators[0].Name)
	assert.Equal(t, validatorGroupName, nodeSet.Status.Validators[0].Group)
	assert.Equal(t, "test-nodeset-validators-0", nodeSet.Status.Validators[1].Name)
	assert.Equal(t, "validators", nodeSet.Status.Validators[1].Group)
	assert.Equal(t, "test-nodeset-validators-1", nodeSet.Status.Validators[2].Name)
	assert.Equal(t, "validators", nodeSet.Status.Validators[2].Group)
}

func TestEnsureValidatorRemovesDeletedGroupValidators(t *testing.T) {
	staleNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-nodeset-validators-0",
			Namespace: "default",
			Labels: map[string]string{
				controllers.LabelChainNodeSet:          "test-nodeset",
				controllers.LabelChainNodeSetGroup:     "validators",
				controllers.LabelChainNodeSetValidator: controllers.StringValueTrue,
			},
		},
	}
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: types.UID("u")},
		Spec:       appsv1.ChainNodeSetSpec{},
		Status: appsv1.ChainNodeSetStatus{
			ChainID: "test-chain",
			Validators: []appsv1.ChainNodeSetValidatorStatus{
				{Name: staleNode.Name, Group: "validators"},
			},
			Nodes: []appsv1.ChainNodeSetNodeStatus{
				{Name: staleNode.Name, Group: "validators"},
			},
		},
	}

	r := newValidatorTestReconciler(t, nodeSet, staleNode)
	require.NoError(t, r.ensureValidator(context.Background(), nodeSet))

	got := &appsv1.ChainNode{}
	err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: staleNode.Name}, got)
	assert.True(t, errors.IsNotFound(err), "stale validator ChainNode should be deleted")
	assert.Empty(t, nodeSet.Status.Validators)
	assert.Empty(t, nodeSet.Status.Nodes)
}

// TestEnsureValidatorKeepsInheritedValidatorLabelRegularNodes verifies that the validator cleanup does
// not delete a regular group node carrying an inherited user "validator=true" label (e.g. after
// upgrading a ChainNodeSet whose own labels include validator=true). ensureNodes relabels such nodes
// validator=false later in the reconcile; the cleanup must not remove them as stale validators first.
func TestEnsureValidatorKeepsInheritedValidatorLabelRegularNodes(t *testing.T) {
	regularNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-nodeset-fullnodes-0",
			Namespace: "default",
			Labels: map[string]string{
				controllers.LabelChainNodeSet:          "test-nodeset",
				controllers.LabelChainNodeSetGroup:     "fullnodes",
				controllers.LabelChainNodeSetValidator: controllers.StringValueTrue, // inherited from user labels
			},
		},
	}
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: types.UID("u")},
		Spec: appsv1.ChainNodeSetSpec{
			Genesis: &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
			Nodes:   []appsv1.NodeGroupSpec{{Name: "fullnodes", Instances: ptr.To(1)}},
		},
		Status: appsv1.ChainNodeSetStatus{ChainID: "test-chain"},
	}

	r := newValidatorTestReconciler(t, nodeSet, regularNode)
	require.NoError(t, r.ensureValidator(context.Background(), nodeSet))

	got := &appsv1.ChainNode{}
	err := r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: regularNode.Name}, got)
	require.NoError(t, err, "regular node with an inherited validator label must not be deleted by the validator cleanup")
}

// TestEnsureValidatorScaleDownRemovesStale verifies that removing a validator instance
// deletes both the stale validator ChainNode and its status entry, which is the behavior
// ensureValidator relies on when a validator group is scaled down.
func TestEnsureValidatorScaleDownRemovesStale(t *testing.T) {
	staleNode := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-nodeset-validators-1",
			Namespace: "default",
			Labels: map[string]string{
				controllers.LabelChainNodeSet:          "test-nodeset",
				controllers.LabelChainNodeSetGroup:     "validators",
				controllers.LabelChainNodeSetValidator: controllers.StringValueTrue,
			},
		},
	}

	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-nodeset",
			Namespace: "default",
			UID:       types.UID("test-uid"),
		},
		Status: appsv1.ChainNodeSetStatus{
			ChainID: "test-chain",
			Validators: []appsv1.ChainNodeSetValidatorStatus{
				{Name: "test-nodeset-validators-0", Group: "validators"},
				{Name: "test-nodeset-validators-1", Group: "validators"},
			},
			Nodes: []appsv1.ChainNodeSetNodeStatus{
				{Name: "test-nodeset-validators-0", Group: "validators"},
				{Name: "test-nodeset-validators-1", Group: "validators"},
			},
		},
	}

	r := newValidatorTestReconciler(t, nodeSet, staleNode)

	require.NoError(t, r.maybeDeleteNode(context.Background(), nodeSet, staleNode.Name))
	DeleteValidatorStatus(nodeSet, staleNode.Name)

	// The stale ChainNode is gone from the cluster.
	got := &appsv1.ChainNode{}
	err := r.Get(context.Background(), client.ObjectKey{Namespace: "default", Name: staleNode.Name}, got)
	assert.True(t, errors.IsNotFound(err), "stale validator ChainNode should be deleted")

	// Both the node status and validator status entries for the removed validator are gone.
	require.Len(t, nodeSet.Status.Validators, 1)
	assert.Equal(t, "test-nodeset-validators-0", nodeSet.Status.Validators[0].Name)
	require.Len(t, nodeSet.Status.Nodes, 1)
	assert.Equal(t, "test-nodeset-validators-0", nodeSet.Status.Nodes[0].Name)
}

// TestEnsureValidatorSnapshotsOnlyOnSnapshotNodeIndex verifies that, for a multi-instance validator
// group with snapshots configured, only the instance at snapshotNodeIndex keeps the snapshots
// config while all others have it cleared — matching regular group nodes. The user's config is not
// mutated.
func TestEnsureValidatorSnapshotsOnlyOnSnapshotNodeIndex(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: types.UID("u")},
		Spec: appsv1.ChainNodeSetSpec{
			Genesis: &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
			Nodes: []appsv1.NodeGroupSpec{{
				Name:              "validators",
				Instances:         ptr.To(3),
				SnapshotNodeIndex: ptr.To(1),
				Validator: &appsv1.NodeSetValidatorConfig{
					Persistence: &appsv1.Persistence{Snapshots: &appsv1.VolumeSnapshotsConfig{Frequency: "1h"}},
				},
			}},
		},
		Status: appsv1.ChainNodeSetStatus{ChainID: "test-chain"},
	}
	r := newValidatorTestReconciler(t, nodeSet)

	require.NoError(t, r.ensureValidator(context.Background(), nodeSet))

	// Only instance 1 (snapshotNodeIndex) keeps snapshots; instances 0 and 2 have them cleared.
	for i, name := range []string{"test-nodeset-validators-0", "test-nodeset-validators-1", "test-nodeset-validators-2"} {
		created := &appsv1.ChainNode{}
		require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, created))
		require.NotNil(t, created.Spec.Persistence, "validator %s must keep persistence", name)
		if i == 1 {
			require.NotNil(t, created.Spec.Persistence.Snapshots, "snapshotNodeIndex validator must keep snapshots")
			assert.Equal(t, "1h", created.Spec.Persistence.Snapshots.Frequency)
		} else {
			assert.Nil(t, created.Spec.Persistence.Snapshots, "validator %s must not have snapshots", name)
		}
	}

	// The user's validator persistence config must not be mutated by clearing snapshots on copies.
	require.NotNil(t, nodeSet.Spec.Nodes[0].Validator.Persistence.Snapshots, "original snapshots config must be preserved")
}

// TestEnsureValidatorCopiesPerNodeRoutes verifies that validator-group ChainNodes get the same
// per-index individual ingress/gateway route config regular group nodes get, including the index
// hostname prefix. The user's route config is not mutated.
func TestEnsureValidatorCopiesPerNodeRoutes(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: types.UID("u")},
		Spec: appsv1.ChainNodeSetSpec{
			Genesis: &appsv1.GenesisConfig{Url: ptr.To("https://example.com/genesis.json")},
			Nodes: []appsv1.NodeGroupSpec{{
				Name:                    "validators",
				Instances:               ptr.To(2),
				IndividualIngresses:     &appsv1.IngressConfig{Host: "val.example.com"},
				IndividualGatewayRoutes: &appsv1.GatewayConfig{Host: "gw.example.com"},
				Validator:               &appsv1.NodeSetValidatorConfig{},
			}},
		},
		Status: appsv1.ChainNodeSetStatus{ChainID: "test-chain"},
	}
	r := newValidatorTestReconciler(t, nodeSet)

	require.NoError(t, r.ensureValidator(context.Background(), nodeSet))

	for i, name := range []string{"test-nodeset-validators-0", "test-nodeset-validators-1"} {
		created := &appsv1.ChainNode{}
		require.NoError(t, r.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: name}, created))
		require.NotNil(t, created.Spec.Ingress, "validator %s must get an ingress", name)
		require.NotNil(t, created.Spec.Gateway, "validator %s must get a gateway route", name)
		if i == 0 {
			assert.Equal(t, "0.val.example.com", created.Spec.Ingress.Host)
			assert.Equal(t, "0.gw.example.com", created.Spec.Gateway.Host)
		} else {
			assert.Equal(t, "1.val.example.com", created.Spec.Ingress.Host)
			assert.Equal(t, "1.gw.example.com", created.Spec.Gateway.Host)
		}
	}

	// The user's route config host must not be mutated by the per-index prefixing.
	assert.Equal(t, "val.example.com", nodeSet.Spec.Nodes[0].IndividualIngresses.Host)
	assert.Equal(t, "gw.example.com", nodeSet.Spec.Nodes[0].IndividualGatewayRoutes.Host)
}

// TestUpdateValidatorStatusPublicAddress verifies that an exposed validator's node status entry
// records its public endpoint (Public, PublicAddress, PublicPort) by parsing Status.PublicAddress,
// matching regular node status, and that a validator with no public address is recorded as not
// public. This is what lets Cosmoseed advertise exposed validators as public peers.
func TestUpdateValidatorStatusPublicAddress(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset"}}

	exposed := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset-validators-0"},
		Status: appsv1.ChainNodeStatus{
			NodeID:        "nodeid",
			PublicAddress: "nodeid@1.2.3.4:26656",
		},
	}
	updateValidatorStatus(nodeSet, exposed, nil, "validators", false, false, nil, false, false)

	require.Len(t, nodeSet.Status.Nodes, 1)
	got := nodeSet.Status.Nodes[0]
	assert.True(t, got.Public)
	assert.Equal(t, "1.2.3.4", got.PublicAddress)
	assert.Equal(t, 26656, got.PublicPort)

	// A validator with no public address is recorded as not public.
	internal := &appsv1.ChainNode{ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset-validators-1"}}
	updateValidatorStatus(nodeSet, internal, nil, "validators", false, false, nil, false, false)

	require.Len(t, nodeSet.Status.Nodes, 2)
	got2 := nodeSet.Status.Nodes[1]
	assert.False(t, got2.Public)
	assert.Empty(t, got2.PublicAddress)
	assert.Zero(t, got2.PublicPort)
}

// TestDeriveGroupValidatorConfigPreservesExistingGenesisValidators verifies that user-specified
// Init.GenesisValidators are preserved (not replaced) when the controller appends the generated
// in-group validators for a multi-instance genesis-initializing group.
func TestDeriveGroupValidatorConfigPreservesExistingGenesisValidators(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default"}}
	existing := appsv1.GenesisValidator{
		PrivKeySecret:         "external-priv-key",
		AccountMnemonicSecret: "external-account",
		Moniker:               "external",
		Assets:                []string{"1000000stake"},
		StakeAmount:           "900000stake",
	}
	cfg := &appsv1.NodeSetValidatorConfig{
		Init: &appsv1.GenesisInitConfig{
			ChainID:           "test-chain",
			Assets:            []string{"1000000stake"},
			StakeAmount:       "900000stake",
			GenesisValidators: []appsv1.GenesisValidator{existing},
		},
	}

	// Instance 0 keeps Init: the user's genesis validator is preserved and the generated in-group
	// validators are appended after it.
	v0 := deriveGroupValidatorConfig(nodeSet, "validators", 0, 3, cfg)
	require.NotNil(t, v0.Init)
	require.Len(t, v0.Init.GenesisValidators, 3) // 1 user-specified + 2 generated
	assert.Equal(t, existing, v0.Init.GenesisValidators[0])
	assert.Equal(t, "test-nodeset-validators-1", v0.Init.GenesisValidators[1].Moniker)
	assert.Equal(t, "test-nodeset-validators-2", v0.Init.GenesisValidators[2].Moniker)

	// The user's original config must not be mutated.
	require.Len(t, cfg.Init.GenesisValidators, 1)
	assert.Equal(t, existing, cfg.Init.GenesisValidators[0])
}

// TestEnsureGenesisValidatorSecretsPopulatesMissingKeys verifies that an existing genesis-validator
// secret missing its required key (mnemonic or priv_validator_key.json) is populated, while existing
// valid data on that secret is preserved.
func TestEnsureGenesisValidatorSecretsPopulatesMissingKeys(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: types.UID("u")},
	}
	cfg := &appsv1.NodeSetValidatorConfig{
		Init: &appsv1.GenesisInitConfig{ChainID: "test-chain", Assets: []string{"1stake"}, StakeAmount: "1stake"},
	}
	gvs := groupGenesisValidators(nodeSet, "validators", 2, cfg)
	require.Len(t, gvs, 1)
	gv := gvs[0]

	// The account secret exists but lacks the mnemonic key (only an unrelated key); the priv-key
	// secret exists but is empty. Both are controlled by this ChainNodeSet (as the controller creates
	// them), so both required keys must be filled and the unrelated key preserved.
	ownedByNodeSet := []metav1.OwnerReference{{
		APIVersion: appsv1.GroupVersion.String(),
		Kind:       "ChainNodeSet",
		Name:       nodeSet.Name,
		UID:        nodeSet.UID,
		Controller: ptr.To(true),
	}}
	accSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: gv.AccountMnemonicSecret, OwnerReferences: ownedByNodeSet},
		Data:       map[string][]byte{"unrelated": []byte("keep-me")},
	}
	pkSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: gv.PrivKeySecret, OwnerReferences: ownedByNodeSet},
	}

	r := newValidatorTestReconciler(t, nodeSet, accSecret, pkSecret)
	ctx := context.Background()

	require.NoError(t, r.ensureGenesisValidatorSecrets(ctx, nodeSet, cfg, gvs))

	acc := &corev1.Secret{}
	require.NoError(t, r.Get(ctx, types.NamespacedName{Namespace: "default", Name: gv.AccountMnemonicSecret}, acc))
	assert.NotEmpty(t, acc.Data[mnemonicKey], "missing mnemonic key must be populated")
	assert.Equal(t, []byte("keep-me"), acc.Data["unrelated"], "existing data must be preserved")

	pk := &corev1.Secret{}
	require.NoError(t, r.Get(ctx, types.NamespacedName{Namespace: "default", Name: gv.PrivKeySecret}, pk))
	assert.NotEmpty(t, pk.Data[privKeyFilename], "missing priv-key key must be populated")
}

// TestEnsureGenesisValidatorSecretsRejectsUnownedSecret verifies that an existing secret missing its
// required key is NOT mutated when it is not controlled by this ChainNodeSet. A secret with a
// colliding name that we do not own may be user-managed, so ensureSecret returns an error instead of
// silently overwriting its data.
func TestEnsureGenesisValidatorSecretsRejectsUnownedSecret(t *testing.T) {
	nodeSet := &appsv1.ChainNodeSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-nodeset", Namespace: "default", UID: types.UID("u")},
	}
	cfg := &appsv1.NodeSetValidatorConfig{
		Init: &appsv1.GenesisInitConfig{ChainID: "test-chain", Assets: []string{"1stake"}, StakeAmount: "1stake"},
	}
	gvs := groupGenesisValidators(nodeSet, "validators", 2, cfg)
	require.Len(t, gvs, 1)
	gv := gvs[0]

	// A pre-existing secret with the colliding name, missing the mnemonic key, controlled by a
	// different owner (not this ChainNodeSet).
	foreign := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default",
			Name:      gv.AccountMnemonicSecret,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "v1",
				Kind:       "Secret",
				Name:       "someone-else",
				UID:        types.UID("other"),
				Controller: ptr.To(true),
			}},
		},
		Data: map[string][]byte{"unrelated": []byte("keep-me")},
	}

	r := newValidatorTestReconciler(t, nodeSet, foreign)
	ctx := context.Background()

	err := r.ensureGenesisValidatorSecrets(ctx, nodeSet, cfg, gvs)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not controlled by this ChainNodeSet")

	// The foreign secret must be left untouched: its mnemonic was not populated and its data preserved.
	got := &corev1.Secret{}
	require.NoError(t, r.Get(ctx, types.NamespacedName{Namespace: "default", Name: gv.AccountMnemonicSecret}, got))
	assert.NotContains(t, got.Data, mnemonicKey, "must not populate keys on an unowned secret")
	assert.Equal(t, []byte("keep-me"), got.Data["unrelated"], "existing data must be preserved")
}
