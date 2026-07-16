package chainnodeset

import (
	"context"
	"fmt"
	"reflect"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	appsv1 "github.com/voluzi/cosmopilot/v2/api/v1"
	"github.com/voluzi/cosmopilot/v2/internal/chainutils"
	"github.com/voluzi/cosmopilot/v2/internal/cometbft"
	"github.com/voluzi/cosmopilot/v2/internal/controllers"
)

func (r *Reconciler) ensureValidator(ctx context.Context, nodeSet *appsv1.ChainNodeSet) error {
	logger := log.FromContext(ctx)

	desiredValidators := map[string]struct{}{}
	nodeSetCopy := nodeSet.DeepCopy()
	nodeSet.Status.Validators = nil
	nodeSet.Status.ValidatorAddress = ""
	nodeSet.Status.ValidatorStatus = ""
	nodeSet.Status.PubKey = ""

	// The legacy singleton status fields alias the first validator in spec order (legacy
	// .spec.validator first, then group validators), regardless of whether that validator
	// has reported any status yet.
	legacyAliasSet := false

	if nodeSet.Spec.Validator != nil {
		name := fmt.Sprintf("%s-validator", nodeSet.GetName())
		desiredValidators[name] = struct{}{}

		validator, err := r.getValidatorSpec(nodeSet, validatorGroupName, 0, nodeSet.Spec.Validator)
		if err != nil {
			return fmt.Errorf("failed to get validator spec for %s: %w", nodeSet.GetName(), err)
		}

		if err := r.ensureNode(ctx, nodeSet, validator, validatorWaitMode(nodeSet, nodeSet.Spec.Validator, 1, validatorGroupName)); err != nil {
			return fmt.Errorf("failed to ensure validator node for %s: %w", nodeSet.GetName(), err)
		}
		updateValidatorStatus(nodeSet, validator, nodeSet.Spec.Validator, validatorGroupName, nodeSet.Spec.Validator.Init != nil, !legacyAliasSet)
		legacyAliasSet = true
	}

	for _, group := range nodeSet.Spec.Nodes {
		if group.Validator == nil {
			continue
		}
		instances := group.GetInstances()

		// When a group validator initializes genesis with more than one instance, only
		// instance 0 initializes genesis; the others are part of that same genesis. Their
		// account and signing-key secrets are pre-created here (deterministically, matching the
		// default secret names of the generated ChainNodes) so instance 0 can derive their
		// accounts and consensus keys when building genesis, without depending on the other
		// ChainNode controllers having created those secrets first. User-listed
		// init.genesisValidators are intentionally not auto-created: those names refer to
		// operator-provided key material and must fail until the intended secrets exist.
		//
		// Only do this before genesis exists (ChainID unset). The consensus keys are baked into the
		// immutable genesis validator set, so regenerating an accidentally-deleted secret after
		// genesis would give the validator a fresh key absent from genesis; leave post-genesis
		// recovery to the operator instead of silently minting new key material.
		if nodeSet.Status.ChainID == "" {
			genesisValidators := groupGenesisValidators(nodeSet, group.Name, instances, group.Validator)
			if len(genesisValidators) > 0 {
				if err := r.ensureGenesisValidatorSecrets(ctx, nodeSet, group.Validator, genesisValidators); err != nil {
					return fmt.Errorf("failed to ensure genesis validator secrets for %s group %s: %w", nodeSet.GetName(), group.Name, err)
				}
			}
		}

		for i := 0; i < instances; i++ {
			name := validatorNodeName(nodeSet, group.Name, i)
			desiredValidators[name] = struct{}{}

			cfg := deriveGroupValidatorConfig(nodeSet, group.Name, i, instances, group.Validator)
			validator, err := r.getValidatorSpec(nodeSet, group.Name, i, cfg)
			if err != nil {
				return fmt.Errorf("failed to get validator spec for %s group %s index %d: %w", nodeSet.GetName(), group.Name, i, err)
			}
			// Carry group-level persistent peers onto the validator ChainNodes, the same way regular
			// group nodes get them, so validator groups joining an external network can connect.
			validator.Spec.Peers = group.Peers
			// Propagate per-instance P2P exposure (.spec.nodes[].expose), the same way regular group
			// nodes get it, so validator-group ChainNodes can advertise themselves to external peers.
			// The legacy singleton .spec.validator has no expose field, so its behavior is unchanged.
			validator.Spec.Expose = exposeForInstance(group.Expose, i)

			// Copy the same per-index ingress/gateway route config regular group nodes get
			// (.spec.nodes[].individualIngresses / .individualGatewayRoutes), including the index
			// hostname prefix, so each validator ChainNode is reachable on its own route.
			if group.IndividualIngresses != nil {
				validator.Spec.Ingress = group.IndividualIngresses.DeepCopy()
				validator.Spec.Ingress.Host = fmt.Sprintf("%d.%s", i, group.IndividualIngresses.Host)
			}
			if group.IndividualGatewayRoutes != nil {
				validator.Spec.Gateway = group.IndividualGatewayRoutes.DeepCopy()
				validator.Spec.Gateway.Host = fmt.Sprintf("%d.%s", i, group.IndividualGatewayRoutes.Host)
			}

			// Match regular group nodes: snapshots run on a single instance only — the one at
			// snapshotNodeIndex. Clear snapshots on every other instance, deep-copying first so the
			// shared validator persistence config is never mutated.
			if validator.Spec.Persistence != nil && i != group.GetSnapshotNodeIndex() {
				validator.Spec.Persistence = validator.Spec.Persistence.DeepCopy()
				validator.Spec.Persistence.Snapshots = nil
			}

			if err := r.ensureNode(ctx, nodeSet, validator, validatorWaitMode(nodeSet, cfg, instances, group.Name)); err != nil {
				return fmt.Errorf("failed to ensure validator node for %s group %s index %d: %w", nodeSet.GetName(), group.Name, i, err)
			}
			// Update status immediately, in spec order. For a genesis-initializing multi-validator
			// group this propagates validator-0's chainID before validator-1's spec is derived,
			// avoiding a transient "-genesis" ConfigMap reference on first reconcile. Every instance of
			// an init group is part of the immutable genesis validator set, so mark them all init —
			// except in a cosmosigner-targeted group, where only instance 0 is a genesis validator (the
			// other instances are redundant signing endpoints of the same identity) and the derived
			// per-instance config reflects that (Init cleared for index > 0).
			isInit := group.Validator.Init != nil
			if nodeSet.IsCosmosignerTargetGroup(group.Name) {
				isInit = cfg.Init != nil
			}
			updateValidatorStatus(nodeSet, validator, cfg, group.Name, isInit, !legacyAliasSet)
			legacyAliasSet = true
		}
	}

	// Regular (non-validator) group nodes that inherited a user-set "validator=true" label (e.g. on a
	// ChainNodeSet upgraded from an older controller) match the validator selector below even though they
	// are not validators. ensureNodes relabels them validator=false, but that runs after this cleanup, so
	// collect the desired regular node names here and never delete them as stale validators.
	desiredRegular := map[string]struct{}{}
	for _, group := range nodeSet.Spec.Nodes {
		if group.Validator != nil {
			continue
		}
		for i := 0; i < group.GetInstances(); i++ {
			desiredRegular[fmt.Sprintf("%s-%s-%d", nodeSet.GetName(), group.Name, i)] = struct{}{}
		}
	}

	chainNodes, err := r.listNodeSetNodes(ctx, nodeSet, controllers.LabelChainNodeSetValidator, strconv.FormatBool(true))
	if err != nil {
		return err
	}
	for _, node := range chainNodes.Items {
		if _, ok := desiredValidators[node.Name]; ok {
			continue
		}
		if _, ok := desiredRegular[node.Name]; ok {
			continue
		}
		logger.Info("removing validator chainnode", "chainnode", node.Name)
		if err := r.maybeDeleteNode(ctx, nodeSet, node.Name); err != nil {
			return err
		}
		DeleteValidatorStatus(nodeSet, node.Name)
	}

	if !reflect.DeepEqual(nodeSet.Status, nodeSetCopy.Status) {
		logger.Info("updating validator status fields",
			"chainID", nodeSet.Status.ChainID,
			"validatorAddress", nodeSet.Status.ValidatorAddress,
			"validatorStatus", nodeSet.Status.ValidatorStatus,
			"pubKey", nodeSet.Status.PubKey,
		)
		return r.Status().Update(ctx, nodeSet)
	}
	return nil
}

func updateValidatorStatus(nodeSet *appsv1.ChainNodeSet, validator *appsv1.ChainNode, cfg *appsv1.NodeSetValidatorConfig, group string, isGenesisInit, setLegacyAlias bool) {
	if nodeSet.Status.ChainID == "" {
		nodeSet.SetEstablishedChainID(validator.Status.ChainID)
	}
	nodeStatus := appsv1.ChainNodeSetNodeStatus{
		Name:    validator.Name,
		ID:      validator.Status.NodeID,
		Address: validator.Status.IP,
		Port:    chainutils.P2pPort,
		Seed:    validator.Status.SeedMode,
		Group:   group,
	}
	// Publish the validator's public endpoint when it is exposed, matching regular node status, so
	// Cosmoseed advertises exposed validators as public peers.
	if host, port, ok := parsePublicAddress(validator.Status.PublicAddress); ok {
		nodeStatus.Public = true
		nodeStatus.PublicAddress = host
		nodeStatus.PublicPort = port
	}
	AddOrUpdateNodeStatus(nodeSet, nodeStatus)
	// Genesis-initializing validators belong to the immutable genesis validator set. Record that
	// fact, plus a fingerprint of their signing material, so the no-webhook reconcile path can detect
	// disallowed post-genesis changes (removal, conversion, or signing-material changes) without a
	// previous spec to diff against. The default priv-key secret matches the generated ChainNode
	// default (<chainnode>-priv-key).
	signingKeyDigest := ""
	if isGenesisInit {
		signingKeyDigest = cfg.GenesisSigningFingerprint(fmt.Sprintf("%s-priv-key", validator.Name))
	}
	AddOrUpdateValidatorStatus(nodeSet, appsv1.ChainNodeSetValidatorStatus{
		Name:             validator.Name,
		Group:            group,
		Address:          validator.Status.ValidatorAddress,
		Status:           validator.Status.ValidatorStatus,
		PubKey:           validator.Status.PubKey,
		Init:             isGenesisInit,
		SigningKeyDigest: signingKeyDigest,
	})

	// Preserve legacy singleton status fields as an alias for the first validator in spec
	// order. Pinning to the first validator (even before it reports status) keeps the alias
	// unambiguous instead of latching onto whichever validator first reports a non-empty value.
	if setLegacyAlias {
		nodeSet.Status.ValidatorAddress = validator.Status.ValidatorAddress
		nodeSet.Status.ValidatorStatus = validator.Status.ValidatorStatus
		nodeSet.Status.PubKey = validator.Status.PubKey
	}
}

func validatorNodeName(nodeSet *appsv1.ChainNodeSet, group string, index int) string {
	// validatorGroupName is reserved for the legacy singleton .spec.validator (rejected as a
	// real group name by the webhook), so it is the only caller that reaches this branch.
	if group == validatorGroupName {
		return fmt.Sprintf("%s-validator", nodeSet.GetName())
	}
	return fmt.Sprintf("%s-%s-%d", nodeSet.GetName(), group, index)
}

func (r *Reconciler) getValidatorSpec(nodeSet *appsv1.ChainNodeSet, group string, index int, cfg *appsv1.NodeSetValidatorConfig) (*appsv1.ChainNode, error) {
	var genesisConfig *appsv1.GenesisConfig
	switch {
	case cfg.Init != nil:
		genesisConfig = nil

	case nodeSet.Spec.Genesis.ShouldDownloadUsingContainer() || nodeSet.Spec.Genesis.HasConfigMapSource():
		genesisConfig = nodeSet.Spec.Genesis

	default:
		genesisConfig = &appsv1.GenesisConfig{
			ConfigMap: ptr.To(nodeSet.Spec.Genesis.GetConfigMapName(nodeSet.Status.ChainID)),
		}
	}

	labels := WithChainNodeSetLabels(nodeSet, map[string]string{
		controllers.LabelChainNodeSet:          nodeSet.GetName(),
		controllers.LabelChainNodeSetGroup:     group,
		controllers.LabelChainNodeSetValidator: strconv.FormatBool(true),
	})

	// Stamp the specific signer's discovery-service label when a managed cosmosigner targets this
	// validator instance, so exactly that signer selects and dials this node's pod.
	if signerName, ok := signerNameForNode(nodeSet, group); ok {
		// A targeted ChainNodeSet child must not inherit a user-set chain-node label: a same-named
		// standalone signer's discovery Service selects chain-node + cosmosigner-target, and the
		// target label below may equal that standalone signer's name. Preserve the user label on
		// non-target resources, but drop it from target children before adding the signer selector.
		delete(labels, controllers.LabelChainNode)
		labels[controllers.LabelCosmosignerTarget] = signerName
	}

	// Carry the same global ingress/gateway membership labels regular group nodes get, so global
	// Services that target this group via .groups select the validator ChainNodes as endpoints.
	for _, ingress := range nodeSet.Spec.Ingresses {
		if ingress.HasGroup(group) {
			labels[ingress.GetName(nodeSet)] = strconv.FormatBool(true)
		}
	}
	for _, gw := range nodeSet.Spec.GatewayRoutes {
		if gw.HasGroup(group) {
			labels[gw.GetName(nodeSet)] = strconv.FormatBool(true)
		}
	}

	validator := &appsv1.ChainNode{
		ObjectMeta: metav1.ObjectMeta{
			Name:      validatorNodeName(nodeSet, group, index),
			Namespace: nodeSet.GetNamespace(),
			Labels:    labels,
		},
		Spec: appsv1.ChainNodeSpec{
			Genesis:     genesisConfig,
			App:         nodeSet.GetAppSpecWithUpgrades(),
			Config:      cfg.Config,
			Persistence: cfg.Persistence,
			Validator: &appsv1.ValidatorConfig{
				PrivateKeySecret: cfg.PrivateKeySecret,
				Info:             cfg.Info,
				Init:             cfg.Init,
				TmKMS:            cfg.TmKMS,
				CreateValidator:  cfg.CreateValidator,
				AccountHDPath:    cfg.AccountHDPath,
				AccountPrefix:    cfg.AccountPrefix,
				ValPrefix:        cfg.ValPrefix,
			},
			Resources:          cfg.Resources,
			Affinity:           cfg.Affinity,
			NodeSelector:       cfg.NodeSelector,
			StateSyncRestore:   cfg.StateSyncRestore,
			StateSyncResources: cfg.StateSyncResources,
			VPA:                cfg.VPA,
			OverrideVersion:    cfg.OverrideVersion,
		},
	}

	// A validator targeted by a managed cosmosigner deployment signs through the external signer:
	// it listens for it and mounts no local key.
	if _, ok := signerNameForNode(nodeSet, group); ok {
		validator.Spec.RemoteSignerTarget = true
	}

	return validator, controllerutil.SetControllerReference(nodeSet, validator, r.Scheme)
}

// deriveGroupValidatorConfig returns the per-instance validator config to use for a
// validator group. For a group that initializes genesis (Init != nil) with more than
// one instance:
//   - instance 0 keeps Init and — unless a cosmosigner targets the group — gets the other
//     instances recorded in Init.GenesisValidators so they are added to the generated genesis
//     as actual genesis validators (account + gentx), not merely funded accounts.
//   - instances > 0 get a copy with Init cleared so they consume the generated genesis
//     instead of initializing their own.
//
// A cosmosigner-targeted group holds ONE consensus identity (the signer's): its instances are
// redundant signing endpoints of the same validator, never N validators. So no sibling genesis
// validators are recorded, and CreateValidator is cleared for index > 0 (only instance 0 runs the
// registration flow — N flows would race to register the same pubkey).
//
// The user-provided config is never mutated in place: a DeepCopy is returned whenever a
// change is needed, otherwise the original config is returned unchanged.
func deriveGroupValidatorConfig(nodeSet *appsv1.ChainNodeSet, group string, index, instances int, cfg *appsv1.NodeSetValidatorConfig) *appsv1.NodeSetValidatorConfig {
	// Nothing special to do for single-instance groups: the only instance uses the config as-is.
	if instances <= 1 {
		return cfg
	}
	signerTargeted := nodeSet.IsCosmosignerTargetGroup(group)
	// Groups that neither initialize genesis nor need the one-identity CreateValidator derivation
	// can use the config as-is on every instance.
	if cfg.Init == nil && !(signerTargeted && cfg.CreateValidator != nil) {
		return cfg
	}

	if index > 0 {
		derived := cfg.DeepCopy()
		if cfg.Init != nil {
			// The init validator derives every group validator's genesis account using this group's
			// resolved account settings (which may be configured under .init). Pin those resolved
			// values onto the derived config before clearing .init, so the generated ChainNode derives
			// the exact same account/valoper addresses and its identity matches the entry recorded in
			// genesis.
			if derived.AccountPrefix == nil {
				derived.AccountPrefix = ptr.To(cfg.GetAccountPrefix())
			}
			if derived.ValPrefix == nil {
				derived.ValPrefix = ptr.To(cfg.GetValPrefix())
			}
			if derived.AccountHDPath == nil {
				derived.AccountHDPath = ptr.To(cfg.GetAccountHDPath())
			}
			derived.Init = nil
		}
		if signerTargeted {
			// One identity: only instance 0 registers the validator; the other instances are
			// redundant signing endpoints.
			derived.CreateValidator = nil
		}
		return derived
	}

	// Instance 0 of a cosmosigner-targeted group is the group's single validator: no sibling
	// genesis validators to record.
	if signerTargeted || cfg.Init == nil {
		return cfg
	}

	// Instance 0: record the other validators in the group as genesis validators so
	// initGenesis includes their accounts and gentxs in the generated genesis. Append to any
	// user-specified genesis validators on the copied config instead of replacing them, so
	// explicitly-configured genesis validators are preserved.
	derived := cfg.DeepCopy()
	derived.Init.GenesisValidators = append(derived.Init.GenesisValidators, groupGenesisValidators(nodeSet, group, instances, cfg)...)
	return derived
}

// groupGenesisValidators returns the GenesisValidator entries for the non-init instances
// (index 1..instances-1) of a genesis-initializing validator group. Each entry references
// the deterministic account-mnemonic and priv-key secret names of the corresponding
// generated ChainNode, and carries the group's default moniker, assets and stake amount.
// It returns nil for groups that do not initialize genesis, have a single instance, or are
// targeted by a cosmosigner (one identity — the extra instances are not genesis validators).
func groupGenesisValidators(nodeSet *appsv1.ChainNodeSet, group string, instances int, cfg *appsv1.NodeSetValidatorConfig) []appsv1.GenesisValidator {
	if cfg.Init == nil || instances <= 1 || nodeSet.IsCosmosignerTargetGroup(group) {
		return nil
	}
	validators := make([]appsv1.GenesisValidator, 0, instances-1)
	for i := 1; i < instances; i++ {
		name := validatorNodeName(nodeSet, group, i)
		validators = append(validators, appsv1.GenesisValidator{
			PrivKeySecret:         fmt.Sprintf("%s-priv-key", name),
			AccountMnemonicSecret: fmt.Sprintf("%s-account", name),
			Moniker:               name,
			Assets:                cfg.Init.Assets,
			StakeAmount:           cfg.Init.StakeAmount,
		})
	}
	return validators
}

// ensureGenesisValidatorSecrets makes sure the account-mnemonic and priv-key secrets for each
// extra genesis validator exist, creating them deterministically when missing. The secret names
// match the default names of the corresponding generated ChainNodes, so the ChainNode
// controllers reuse the same secrets instead of generating fresh (and conflicting) ones.
func (r *Reconciler) ensureGenesisValidatorSecrets(ctx context.Context, nodeSet *appsv1.ChainNodeSet, cfg *appsv1.NodeSetValidatorConfig, validators []appsv1.GenesisValidator) error {
	for _, gv := range validators {
		if err := r.ensureSecret(ctx, nodeSet, gv.AccountMnemonicSecret, []string{mnemonicKey}, func() (map[string][]byte, error) {
			account, err := chainutils.CreateAccount(cfg.GetAccountPrefix(), cfg.GetValPrefix(), cfg.GetAccountHDPath())
			if err != nil {
				return nil, err
			}
			return map[string][]byte{mnemonicKey: []byte(account.Mnemonic)}, nil
		}); err != nil {
			return fmt.Errorf("failed to ensure account secret %s: %w", gv.AccountMnemonicSecret, err)
		}

		if err := r.ensureSecret(ctx, nodeSet, gv.PrivKeySecret, []string{privKeyFilename}, func() (map[string][]byte, error) {
			key, err := cometbft.GeneratePrivKey()
			if err != nil {
				return nil, err
			}
			return map[string][]byte{privKeyFilename: key}, nil
		}); err != nil {
			return fmt.Errorf("failed to ensure priv-key secret %s: %w", gv.PrivKeySecret, err)
		}
	}
	return nil
}

// ensureSecret creates a secret owned by the ChainNodeSet with the data returned by genData if it
// does not already exist. When the secret already exists but is missing (or has empty) any of the
// requiredKeys, those keys are populated from genData while existing valid data is preserved, so an
// incomplete secret (e.g. one created without a mnemonic or priv_validator_key.json) is healed
// instead of leaving the genesis to be built from incomplete data. A secret that already has all
// required keys is left untouched, so its key material stays stable across reconciles.
func (r *Reconciler) ensureSecret(ctx context.Context, nodeSet *appsv1.ChainNodeSet, name string, requiredKeys []string, genData func() (map[string][]byte, error)) error {
	logger := log.FromContext(ctx)

	secret := &corev1.Secret{}
	err := r.Get(ctx, types.NamespacedName{Namespace: nodeSet.GetNamespace(), Name: name}, secret)
	if err == nil {
		missing := missingSecretKeys(secret.Data, requiredKeys)
		if len(missing) == 0 {
			return nil
		}

		// The secret exists but is incomplete. Only heal secrets that this ChainNodeSet controls: a
		// secret with a colliding name that we do not own may be user-managed, and silently overwriting
		// its data could corrupt key material we have no authority over. Refuse with a clear error
		// instead. Secrets created by this controller carry a controller owner reference to the
		// ChainNodeSet (see the create path below), so legitimately-managed secrets still heal.
		if !metav1.IsControlledBy(secret, nodeSet) {
			return fmt.Errorf("secret %s exists but is not controlled by this ChainNodeSet; refusing to modify potentially user-managed secret data", name)
		}

		data, err := genData()
		if err != nil {
			return err
		}
		if secret.Data == nil {
			secret.Data = make(map[string][]byte, len(missing))
		}
		for _, key := range missing {
			secret.Data[key] = data[key]
		}

		logger.Info("populating missing keys in secret", "secret", name, "keys", missing)
		return r.Update(ctx, secret)
	}
	if !errors.IsNotFound(err) {
		return err
	}

	data, err := genData()
	if err != nil {
		return err
	}

	secret = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: nodeSet.GetNamespace(),
			Labels:    WithChainNodeSetLabels(nodeSet),
		},
		Data: data,
	}
	if err := controllerutil.SetControllerReference(nodeSet, secret, r.Scheme); err != nil {
		return err
	}

	logger.Info("creating genesis validator secret", "secret", name)
	if err := r.Create(ctx, secret); err != nil && !errors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// missingSecretKeys returns the subset of keys that are absent from data or whose value is empty.
func missingSecretKeys(data map[string][]byte, keys []string) []string {
	var missing []string
	for _, key := range keys {
		if len(data[key]) == 0 {
			missing = append(missing, key)
		}
	}
	return missing
}

// validatorWaitMode reports what ensureValidator must block on after reconciling a validator
// ChainNode. Only the validator that initializes genesis while the ChainNodeSet still has no
// chainID needs to be waited on: that path produces the genesis every other node depends on.
//
// For a single-instance genesis validator the chain produces blocks on its own, so we wait until
// it is running/syncing. For a multi-instance genesis group the chain only produces blocks once
// every group validator is online, and the remaining validators are created only after this wait
// returns; blocking on "running" would therefore deadlock. In that case we block only until the
// genesis is ready (the chainID is populated and its ConfigMap exists), which is enough to derive
// and create the remaining validators. All other validators reconcile without blocking.
func validatorWaitMode(nodeSet *appsv1.ChainNodeSet, cfg *appsv1.NodeSetValidatorConfig, instances int, group string) chainNodeWait {
	if cfg.Init == nil || nodeSet.Status.ChainID != "" {
		return waitNone
	}
	// A cosmosigner-targeted init validator starts as a remote-signer target with no local key: it
	// blocks at startup waiting for the signer to dial in. The signer is deployed only after this
	// wait returns, so blocking on "running" would deadlock. Wait only until genesis is ready (the
	// chainID is populated), which is enough for the signer to be configured and dial in.
	if instances > 1 || nodeSet.IsCosmosignerTargetGroup(group) {
		return waitGenesisReady
	}
	return waitRunningOrSyncing
}
