package v1

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func SetupChainNodeSetValidationWebhook(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &ChainNodeSet{}).WithValidator(&ChainNodeSet{}).Complete()
}

var _ admission.Validator[*ChainNodeSet] = &ChainNodeSet{}
var chainNodeSetLogger = log.Log.WithName("chainnodeset-webhook")

func (nodeSet *ChainNodeSet) ValidateCreate(_ context.Context, obj *ChainNodeSet) (warnings admission.Warnings, err error) {
	chainNodeSetLogger.V(1).Info("validating resource creation",
		"kind", "ChainNodeSet",
		"resource", obj.GetNamespacedName(),
	)
	return obj.Validate(nil)
}

func (nodeSet *ChainNodeSet) ValidateUpdate(_ context.Context, oldObj, newObj *ChainNodeSet) (warnings admission.Warnings, err error) {
	chainNodeSetLogger.V(1).Info("validating resource update",
		"kind", "ChainNodeSet",
		"resource", newObj.GetNamespacedName(),
	)
	return newObj.Validate(oldObj)
}

func (nodeSet *ChainNodeSet) ValidateDelete(_ context.Context, obj *ChainNodeSet) (warnings admission.Warnings, err error) {
	chainNodeSetLogger.V(1).Info("validating resource deletion (not implemented)",
		"kind", "ChainNodeSet",
		"resource", obj.GetNamespacedName(),
	)
	return nil, nil
}

func (nodeSet *ChainNodeSet) Validate(old *ChainNodeSet) (admission.Warnings, error) {
	// Count validators and how many of them initialize a new genesis.
	initValidators := 0
	nonInitValidators := 0
	if nodeSet.Spec.Validator != nil {
		if nodeSet.Spec.Validator.Init != nil {
			initValidators++
		} else {
			nonInitValidators++
		}
	}
	for _, group := range nodeSet.Spec.Nodes {
		// A group with zero instances runs no validators, so it must not count toward the genesis
		// requirements (otherwise it would trigger false "genesis required" rejections).
		if group.Validator == nil || group.GetInstances() == 0 {
			continue
		}
		if group.Validator.Init != nil {
			initValidators++
		} else {
			nonInitValidators++
		}
	}

	// Ensure a genesis is specified unless a validator initializes a new genesis. When a validator
	// does initialize genesis, every other validator must initialize one too, otherwise the non-init
	// validators have no genesis source to consume.
	//
	// This requirement is lifted once the chain is already running and the genesis exists as the
	// generated <chainID>-genesis ConfigMap, which every node — including non-init validators added
	// later (e.g. joining a running chain via createValidator) — can consume. Two conditions must
	// hold for that to be safe:
	//
	//   - genesisAlreadyCreated: the chain's genesis exists (its chainID is recorded in status). The
	//     status is read from old on the webhook update path (the incoming object may not carry it)
	//     and from the object itself on the no-webhook reconcile path (where old is nil but the
	//     persisted status is present).
	//   - genesisInitGenerated: the genesis was produced by a genesis-initializing validator
	//     (.validator.init), which is what generates the <chainID>-genesis ConfigMap. When the chain
	//     instead uses an explicit genesis source (genesis.configMap, useDataVolume, a custom
	//     ConfigMap name, ...) no <chainID>-genesis is generated, so .spec.genesis must be retained
	//     and cannot be dropped just because the chainID is set.
	//
	// Adding a new genesis-initializing validator after creation is still rejected below.
	genesisAlreadyCreated := nodeSet.Status.ChainID != "" || (old != nil && old.Status.ChainID != "")
	genesisInitGenerated := nodeSet.ShouldInitGenesis() || (old != nil && old.ShouldInitGenesis())
	if nodeSet.Spec.Genesis == nil && !(genesisAlreadyCreated && genesisInitGenerated) {
		switch {
		case initValidators == 0:
			return nil, fmt.Errorf(".spec.genesis is required except when initializing new genesis with .spec.validator.init")
		case nonInitValidators > 0:
			return nil, fmt.Errorf(".spec.genesis is required when a validator does not initialize a new genesis with .spec.validator.init")
		}
	}

	// Do not accept both genesis and validator init
	if nodeSet.Spec.Validator != nil && nodeSet.Spec.Validator.Init != nil && nodeSet.Spec.Genesis != nil {
		return nil, fmt.Errorf(".spec.genesis and .spec.validator.init are mutually exclusive")
	}

	// A validator that both initializes genesis and runs create-validator is contradictory: an
	// init validator is already part of the generated genesis validator set, so a create-validator
	// tx for it would be redundant (and fail on-chain). Reject the combination instead of relying on
	// downstream code to silently drop one of them.
	if nodeSet.Spec.Validator != nil && nodeSet.Spec.Validator.Init != nil && nodeSet.Spec.Validator.CreateValidator != nil {
		return nil, fmt.Errorf(".spec.validator.init and .spec.validator.createValidator are mutually exclusive")
	}

	// Mirror the per-group create-validator/TmKMS guard below for the legacy singleton .spec.validator:
	// the pod signs through the KMS sidecar and never mounts the local priv-key secret, so the
	// locally-registered create-validator pubkey only matches the signing key when the generated key
	// is uploaded to the KMS.
	if nodeSet.Spec.Validator != nil && nodeSet.Spec.Validator.CreateValidator != nil && nodeSet.Spec.Validator.TmKMS != nil && !tmkmsUploadsGeneratedPrivKey(nodeSet.Spec.Validator) {
		return nil, fmt.Errorf(".spec.validator.tmKMS with createValidator requires hashicorp.uploadGenerated=true so the locally-generated key is uploaded to the KMS and the registered create-validator pubkey matches the signing key")
	}

	// Validate validator snapshots config
	if nodeSet.Spec.Validator != nil && nodeSet.Spec.Validator.Persistence != nil && nodeSet.Spec.Validator.Persistence.Snapshots != nil {
		if err := validateSnapshotsConfig(nodeSet.Spec.Validator.Persistence.Snapshots, ".spec.validator.persistence.snapshots"); err != nil {
			return nil, err
		}
	}

	// Validate validator persistence size with the same logic used for regular group persistence,
	// so an invalid quantity is rejected here instead of failing later on the generated ChainNode.
	if nodeSet.Spec.Validator != nil && nodeSet.Spec.Validator.Persistence != nil && nodeSet.Spec.Validator.Persistence.Size != nil {
		if _, err := resource.ParseQuantity(*nodeSet.Spec.Validator.Persistence.Size); err != nil {
			return nil, fmt.Errorf("bad format for .spec.validator.persistence.size: %v", err)
		}
	}

	// Index the previous groups (on update) so we can detect disallowed changes such as scaling
	// up a genesis-initializing validator group after genesis has been created.
	oldGroups := map[string]NodeGroupSpec{}
	if old != nil {
		for _, g := range old.Spec.Nodes {
			oldGroups[g.Name] = g
		}
	}

	// Validate each node group
	seenGroupNames := make(map[string]int, len(nodeSet.Spec.Nodes))
	for i, group := range nodeSet.Spec.Nodes {
		// The validator group name is reserved for the legacy singleton .spec.validator.
		if group.Name == ReservedValidatorGroupName {
			return nil, fmt.Errorf(".spec.nodes[%d].name %q is reserved", i, ReservedValidatorGroupName)
		}

		// Group names must be unique: ChainNode names are derived from <nodeset>-<group>-<index>,
		// so duplicate group names would produce colliding child ChainNode names.
		if prev, ok := seenGroupNames[group.Name]; ok {
			return nil, fmt.Errorf(".spec.nodes[%d].name %q duplicates .spec.nodes[%d].name", i, group.Name, prev)
		}
		seenGroupNames[group.Name] = i

		// Validate persistence size
		if group.Persistence != nil && group.Persistence.Size != nil {
			_, err := resource.ParseQuantity(*group.Persistence.Size)
			if err != nil {
				return nil, fmt.Errorf("bad format for .spec.nodes[%d].persistence.size: %v", i, err)
			}
		}

		// Validate snapshots config
		if group.Persistence != nil && group.Persistence.Snapshots != nil {
			if err := validateSnapshotsConfig(group.Persistence.Snapshots, fmt.Sprintf(".spec.nodes[%d].persistence.snapshots", i)); err != nil {
				return nil, err
			}
		}

		// Validate group validator config
		if group.Validator != nil {
			if group.Validator.Init != nil && nodeSet.Spec.Genesis != nil {
				return nil, fmt.Errorf(".spec.genesis and .spec.nodes[%d].validator.init are mutually exclusive", i)
			}
			// A validator group cannot both initialize genesis and run create-validator. Only
			// instance 0 initializes genesis, but it records every group instance (including the
			// non-init ones) in the generated genesis validator set. The non-init instances are
			// derived by clearing only Init, so they would still carry CreateValidator and submit a
			// create-validator tx for a validator already present in genesis. Reject the combination
			// rather than silently clearing CreateValidator on the derived instances.
			if group.Validator.Init != nil && group.Validator.CreateValidator != nil {
				return nil, fmt.Errorf(".spec.nodes[%d].validator.init and .spec.nodes[%d].validator.createValidator are mutually exclusive", i, i)
			}
			// A multi-instance validator group runs one validator per instance, each of which must
			// sign with its own consensus key. A shared privateKeySecret or a shared tmKMS key would
			// make every instance sign with the same key (double-signing), so both are rejected
			// regardless of genesis mode; the controller generates a distinct key per instance.
			if group.GetInstances() > 1 && group.Validator.PrivateKeySecret != nil {
				return nil, fmt.Errorf(".spec.nodes[%d].validator.privateKeySecret cannot be set when the validator group has multiple instances", i)
			}
			if group.GetInstances() > 1 && group.Validator.TmKMS != nil {
				return nil, fmt.Errorf(".spec.nodes[%d].validator.tmKMS cannot be set when the validator group has multiple instances (every instance would sign with the same key)", i)
			}
			// For a genesis-initializing multi-instance group the controller also manages the
			// per-instance account mnemonic secrets, so a shared one cannot be provided.
			if group.Validator.Init != nil && group.GetInstances() > 1 && group.Validator.Init.AccountMnemonicSecret != nil {
				return nil, fmt.Errorf(".spec.nodes[%d].validator.init.accountMnemonicSecret cannot be set when validator.init is used with multiple instances", i)
			}
			// A multi-instance createValidator group derives a distinct per-instance account for each
			// generated validator. A single shared accountMnemonicSecret would make every instance
			// submit a create-validator tx for the same operator account, so it is rejected (mirroring
			// the init guard above).
			if group.Validator.CreateValidator != nil && group.GetInstances() > 1 && group.Validator.CreateValidator.AccountMnemonicSecret != nil {
				return nil, fmt.Errorf(".spec.nodes[%d].validator.createValidator.accountMnemonicSecret cannot be set when the validator group has multiple instances", i)
			}
			// A create-validator ChainNode registers Status.PubKey derived from its local priv-key
			// secret, but a TmKMS validator signs through the KMS sidecar and never mounts that secret.
			// Unless the local key is uploaded to the KMS (hashicorp.uploadGenerated=true), the
			// registered pubkey would not match the key the pod actually signs with. An explicit
			// privateKeySecret does not change this — it only selects which local key is registered, not
			// what the KMS signs with — so the upload is required regardless until KMS pubkey
			// registration is supported.
			if group.Validator.CreateValidator != nil && group.Validator.TmKMS != nil && !tmkmsUploadsGeneratedPrivKey(group.Validator) {
				return nil, fmt.Errorf(".spec.nodes[%d].validator.tmKMS with createValidator requires hashicorp.uploadGenerated=true so the locally-generated key is uploaded to the KMS and the registered create-validator pubkey matches the signing key", i)
			}
			// Validate validator persistence size with the same logic regular group persistence uses,
			// so an invalid quantity is rejected here instead of failing later on the generated ChainNode.
			if group.Validator.Persistence != nil && group.Validator.Persistence.Size != nil {
				if _, err := resource.ParseQuantity(*group.Validator.Persistence.Size); err != nil {
					return nil, fmt.Errorf("bad format for .spec.nodes[%d].validator.persistence.size: %v", i, err)
				}
			}
			if group.Validator.Persistence != nil && group.Validator.Persistence.Snapshots != nil {
				if err := validateSnapshotsConfig(group.Validator.Persistence.Snapshots, fmt.Sprintf(".spec.nodes[%d].validator.persistence.snapshots", i)); err != nil {
					return nil, err
				}
			}
		}

		if group.GetSnapshotNodeIndex() < 0 || group.GetSnapshotNodeIndex() >= group.GetInstances() {
			return nil, fmt.Errorf(".spec.nodes[%d].snapshotNodeIndex is out of range", i)
		}

		// Catch duplicate subdomain prefixes on individual ingresses/gateways here
		// instead of letting the ChainNode webhook reject the child during reconcile.
		if ing := group.IndividualIngresses; ing != nil {
			if err := ValidateSubdomainPrefixes(fmt.Sprintf(".spec.nodes[%d].individualIngresses", i), ing.Subdomains,
				ing.EnableRPC, ing.EnableGRPC, ing.EnableLCD, ing.EnableEvmRPC, ing.EnableEvmRpcWs); err != nil {
				return nil, err
			}
		}
		if gw := group.IndividualGatewayRoutes; gw != nil {
			if err := ValidateSubdomainPrefixes(fmt.Sprintf(".spec.nodes[%d].individualGatewayRoutes", i), gw.Subdomains,
				gw.EnableRPC, gw.EnableGRPC, gw.EnableLCD, gw.EnableEvmRPC, gw.EnableEvmRpcWs); err != nil {
				return nil, err
			}
		}
	}

	// Validator groups get a PodDisruptionBudget named "<nodeset>-<group>-validator", while regular
	// groups own resources named "<nodeset>-<group>". A validator group named "foo" therefore produces a
	// "<nodeset>-foo-validator" PDB that conflicts with a regular group named "foo-validator": that
	// regular group owns the same "<nodeset>-foo-validator" name and reconciles it every pass — creating
	// its own PDB when enabled, or deleting that name when disabled, which would tear down the validator
	// group's PDB. So the conflict exists whenever the validator group's PDB is enabled and a regular
	// group with the suffixed name is present, regardless of the regular group's own PDB setting.
	//
	// It is gated on the validator PDB being enabled because PDBs default to disabled: without that gate
	// the common topology ("foo" + "foo-validator", neither enabling a PDB) would be rejected for a
	// conflict that never occurs. If "foo-validator" is also a validator group, its validator PDB is
	// "<nodeset>-foo-validator-validator" instead, so there is no conflict.
	for i, group := range nodeSet.Spec.Nodes {
		if group.Validator == nil || !group.Validator.HasPdbEnabled() {
			continue
		}
		if suffixedIndex, ok := seenGroupNames[group.Name+"-validator"]; ok &&
			nodeSet.Spec.Nodes[suffixedIndex].Validator == nil {
			return nil, fmt.Errorf(".spec.nodes[%d].name %q collides with regular group %q: its validator PDB %q would clash with that group's PDB",
				i, group.Name, group.Name+"-validator", fmt.Sprintf("%s-validator", group.Name))
		}
	}

	if initValidators > 1 {
		return nil, fmt.Errorf("only one ChainNodeSet validator can initialize genesis")
	}

	// Validate the managed cosmosigner deployment and its target resolution.
	if err := nodeSet.validateCosmosigner(); err != nil {
		return nil, err
	}

	// Two validators that explicitly reference the same signing material would sign with the same
	// consensus key (double-signing). Reject duplicates across every validator that actually runs.
	if err := nodeSet.validateUniqueSigningKeys(); err != nil {
		return nil, err
	}

	// Two create-validator validators that resolve to the same account-mnemonic secret would derive
	// the same operator/valoper account. Reject duplicates across every create-validator that runs.
	if err := nodeSet.validateUniqueCreateValidatorAccounts(); err != nil {
		return nil, err
	}

	// Two genesis validators that resolve to the same account-mnemonic secret would derive the same
	// operator/valoper account for distinct gentxs, producing an invalid genesis. Reject duplicates
	// across every account that ends up in the generated genesis.
	if err := nodeSet.validateUniqueGenesisValidatorAccounts(); err != nil {
		return nil, err
	}

	// Once genesis has been created (the chainID is known), the initial validator set is fixed: the
	// genesis is never regenerated. Reject any update that adds genesis-initializing validators —
	// whether by enabling .spec.validator.init, introducing or renaming an init validator group, or
	// scaling an existing init group up — since those validators would not be part of the genesis
	// validator set. Validators can still be added to a running chain via a group with
	// createValidator.
	if old != nil && old.Status.ChainID != "" {
		if nodeSet.Spec.Validator != nil && nodeSet.Spec.Validator.Init != nil &&
			(old.Spec.Validator == nil || old.Spec.Validator.Init == nil) {
			return nil, fmt.Errorf(".spec.validator.init cannot be added after genesis has been created")
		}
		// Conversely, the legacy singleton genesis-initializing validator cannot be removed once
		// genesis exists: dropping .spec.validator (or clearing .spec.validator.init) would make
		// ensureValidator delete the underlying ChainNode whose voting power remains in the immutable
		// genesis validator set, potentially halting the chain. This mirrors the group guard below.
		if old.Spec.Validator != nil && old.Spec.Validator.Init != nil &&
			(nodeSet.Spec.Validator == nil || nodeSet.Spec.Validator.Init == nil) {
			return nil, fmt.Errorf(".spec.validator.init cannot be removed after genesis has been created: its validator is part of the immutable genesis validator set")
		}
		// The legacy genesis-initializing validator is fixed in the immutable genesis. Reject changing
		// its signing material (private-key secret or tmKMS key) or any genesis parameter in .init
		// (assets, stake, accounts, ...) even when the validator is otherwise kept: a recreated genesis
		// would otherwise differ. The add/remove guards above guarantee both old and new carry .init here.
		if old.Spec.Validator != nil && old.Spec.Validator.Init != nil &&
			nodeSet.Spec.Validator != nil && nodeSet.Spec.Validator.Init != nil {
			defaultPrivKeySecret := fmt.Sprintf("%s-validator-priv-key", nodeSet.GetName())
			if genesisSigningMaterialChanged(old.Spec.Validator, nodeSet.Spec.Validator, defaultPrivKeySecret) {
				return nil, fmt.Errorf(".spec.validator signing material or genesis parameters cannot be changed after genesis has been created: they are part of the immutable genesis validator set")
			}
		}
		for i, group := range nodeSet.Spec.Nodes {
			if group.Validator == nil || group.Validator.Init == nil {
				continue
			}
			og, ok := oldGroups[group.Name]
			if !ok || og.Validator == nil || og.Validator.Init == nil {
				return nil, fmt.Errorf(".spec.nodes[%d]: a genesis-initializing validator group cannot be added after genesis has been created", i)
			}
			if group.GetInstances() > og.GetInstances() {
				return nil, fmt.Errorf(".spec.nodes[%d] genesis-initializing validator group %q cannot be scaled up after creation", i, group.Name)
			}
			// Shrinking is rejected too: the removed validators' voting power stays in the immutable
			// genesis validator set, so dropping them can halt the chain (it may never reach the
			// 2/3 voting power required to produce blocks). There is no API field to opt into this
			// unsafe operation, so it is rejected outright; decommissioning must be done on-chain.
			if group.GetInstances() < og.GetInstances() {
				return nil, fmt.Errorf(".spec.nodes[%d] genesis-initializing validator group %q cannot be scaled down after creation: its validators are part of the immutable genesis validator set", i, group.Name)
			}
			// The group's validators are in the immutable genesis with fixed consensus keys and gentx
			// parameters. Reject changing their signing material (single-instance privateKeySecret or
			// tmKMS key) or any genesis parameter in .init (assets, stake, accounts, genesisValidators,
			// ...) — a recreated genesis would otherwise differ. Multi-instance groups cannot set
			// privateKeySecret/tmKMS (rejected above) and their per-instance keys derive from stable
			// names, so this only flags real changes.
			defaultPrivKeySecret := fmt.Sprintf("%s-%s-0-priv-key", nodeSet.GetName(), group.Name)
			if genesisSigningMaterialChanged(og.Validator, group.Validator, defaultPrivKeySecret) {
				return nil, fmt.Errorf(".spec.nodes[%d] genesis-initializing validator group %q signing material or genesis parameters cannot be changed after creation: they are part of the immutable genesis validator set", i, group.Name)
			}
		}

		// Conversely, every genesis-initializing validator group that existed when genesis was
		// created must still be present as a genesis-initializing validator group. Deleting such a
		// group, or converting it to a non-init or non-validator group, would make ensureValidator
		// delete the underlying ChainNodes whose voting power remains in the immutable genesis
		// validator set — potentially halting the chain. The loop above already rejects an instance
		// count change for groups that keep init, so only existence/conversion is checked here.
		newGroups := make(map[string]NodeGroupSpec, len(nodeSet.Spec.Nodes))
		for _, g := range nodeSet.Spec.Nodes {
			newGroups[g.Name] = g
		}
		for _, og := range old.Spec.Nodes {
			if og.Validator == nil || og.Validator.Init == nil || og.GetInstances() == 0 {
				continue
			}
			ng, ok := newGroups[og.Name]
			if !ok || ng.Validator == nil || ng.Validator.Init == nil {
				return nil, fmt.Errorf("genesis-initializing validator group %q cannot be removed or converted after genesis has been created: its validators are part of the immutable genesis validator set", og.Name)
			}
		}
	}

	// Names in .spec.ingresses and .spec.gatewayRoutes must be unique across both lists,
	// because both produce identically-named global Services (<name>-global-<name>).
	seenRouteNames := make(map[string]string, len(nodeSet.Spec.Ingresses)+len(nodeSet.Spec.GatewayRoutes))
	for i, ing := range nodeSet.Spec.Ingresses {
		if existing, ok := seenRouteNames[ing.Name]; ok {
			return nil, fmt.Errorf(".spec.ingresses[%d].name %q duplicates %s", i, ing.Name, existing)
		}
		seenRouteNames[ing.Name] = fmt.Sprintf(".spec.ingresses[%d]", i)
	}
	for i, gw := range nodeSet.Spec.GatewayRoutes {
		if existing, ok := seenRouteNames[gw.Name]; ok {
			return nil, fmt.Errorf(".spec.gatewayRoutes[%d].name %q duplicates %s", i, gw.Name, existing)
		}
		seenRouteNames[gw.Name] = fmt.Sprintf(".spec.gatewayRoutes[%d]", i)
	}

	// Reject duplicate subdomain prefixes across enabled endpoints in each ingress / gateway entry
	for i, ing := range nodeSet.Spec.Ingresses {
		if err := ValidateSubdomainPrefixes(fmt.Sprintf(".spec.ingresses[%d]", i), ing.Subdomains,
			ing.EnableRPC, ing.EnableGRPC, ing.EnableLCD, ing.EnableEvmRPC, ing.EnableEvmRpcWs); err != nil {
			return nil, err
		}
	}
	for i, gw := range nodeSet.Spec.GatewayRoutes {
		if err := ValidateSubdomainPrefixes(fmt.Sprintf(".spec.gatewayRoutes[%d]", i), gw.Subdomains,
			gw.EnableRPC, gw.EnableGRPC, gw.EnableLCD, gw.EnableEvmRPC, gw.EnableEvmRpcWs); err != nil {
			return nil, err
		}
	}

	return nil, nil
}

// validateCosmosigner validates the managed cosmosigner deployment and how it targets node groups.
//
// A cosmosigner signs for a single consensus identity shared across every node it connects to.
// Valid targets are therefore:
//   - a regular (non-validator) node group of any size — "sentry mode", where the group nodes are
//     the signing endpoints of the one identity;
//   - the legacy singleton .spec.validator (the default when nodeGroups is empty);
//   - a single-instance validator group — a drop-in remote signer for that one validator.
//
// A multi-instance validator group is rejected: each of its instances is a distinct validator with
// its own consensus key, which cannot be collapsed onto a single signer identity. A targeted
// validator must not also use TmKMS.
func (nodeSet *ChainNodeSet) validateCosmosigner() error {
	c := nodeSet.Spec.Cosmosigner
	if c == nil {
		return nil
	}

	if err := c.Validate(".spec.cosmosigner", true); err != nil {
		return err
	}

	// Index groups by name for target resolution.
	groups := make(map[string]NodeGroupSpec, len(nodeSet.Spec.Nodes))
	for _, g := range nodeSet.Spec.Nodes {
		groups[g.Name] = g
	}

	if len(c.NodeGroups) == 0 {
		// Default target is the legacy singleton validator.
		if nodeSet.Spec.Validator == nil {
			return fmt.Errorf(".spec.cosmosigner.nodeGroups is required when .spec.validator is not set")
		}
		if nodeSet.Spec.Validator.TmKMS != nil {
			return fmt.Errorf(".spec.cosmosigner and .spec.validator.tmKMS are mutually exclusive")
		}
		return nil
	}

	seen := map[string]struct{}{}
	for i, name := range c.NodeGroups {
		if _, dup := seen[name]; dup {
			return fmt.Errorf(".spec.cosmosigner.nodeGroups[%d] %q is listed more than once", i, name)
		}
		seen[name] = struct{}{}

		group, ok := groups[name]
		if !ok {
			return fmt.Errorf(".spec.cosmosigner.nodeGroups[%d] %q does not match any group in .spec.nodes", i, name)
		}
		if group.Validator != nil {
			if group.GetInstances() > 1 {
				return fmt.Errorf(".spec.cosmosigner cannot target validator group %q with multiple instances: each instance is a distinct validator with its own key", name)
			}
			if group.Validator.TmKMS != nil {
				return fmt.Errorf(".spec.cosmosigner cannot target group %q which uses tmKMS: cosmosigner and tmKMS are mutually exclusive", name)
			}
		}
	}
	return nil
}

// validateUniqueSigningKeys rejects two running validators that would sign with the same
// consensus key (double-signing). Two validators collide when they resolve to the same private
// key secret — whether that name is set explicitly via privateKeySecret or left to the generated
// ChainNode default — or when they reference the same tmKMS signing key.
//
// The resolved default secret name is included for every running validator so an explicit
// privateKeySecret on one validator cannot silently alias another validator's default secret
// (e.g. a single-instance group setting "<nodeset>-<group>-0-priv-key", the default of a
// multi-instance group instance). The names match the generated ChainNode defaults precisely:
// <nodeset>-validator-priv-key for the legacy singleton and <nodeset>-<group>-<index>-priv-key
// for group validators.
//
// "Running" validators are the legacy singleton .spec.validator and every .spec.nodes[].validator
// group with at least one instance.
func (nodeSet *ChainNodeSet) validateUniqueSigningKeys() error {
	privateKeySecrets := map[string]string{}
	tmKMSKeys := map[string]string{}

	registerSecret := func(path, secret string) error {
		if prev, ok := privateKeySecrets[secret]; ok {
			return fmt.Errorf("%s.privateKeySecret %q is already used by %s; each validator must sign with a distinct key", path, secret, prev)
		}
		privateKeySecrets[secret] = path
		return nil
	}

	registerTmKMS := func(path string, v *NodeSetValidatorConfig) error {
		if id, ok := tmKMSSigningKeyIdentity(v.TmKMS); ok {
			if prev, ok := tmKMSKeys[id]; ok {
				return fmt.Errorf("%s.tmKMS references the same signing key as %s; each validator must sign with a distinct key", path, prev)
			}
			tmKMSKeys[id] = path
		}
		return nil
	}

	// User-preserved genesis validators (validator.init.genesisValidators) reference existing
	// priv-key secrets that must be distinct from every other validator's signing key — the init
	// validator's own key, the generated in-group instance defaults, and any other validator's
	// explicit or default key. Register them in the same map so a collision is rejected here,
	// before the controller appends its generated genesis-validator entries.
	registerGenesisValidators := func(path string, init *GenesisInitConfig) error {
		if init == nil {
			return nil
		}
		for j, gv := range init.GenesisValidators {
			gvPath := fmt.Sprintf("%s.init.genesisValidators[%d].privKeySecret", path, j)
			if prev, ok := privateKeySecrets[gv.PrivKeySecret]; ok {
				return fmt.Errorf("%s %q is already used by %s; each validator must sign with a distinct key", gvPath, gv.PrivKeySecret, prev)
			}
			privateKeySecrets[gv.PrivKeySecret] = gvPath
		}
		return nil
	}

	// Legacy singleton validator: its ChainNode is named <nodeset>-validator, so without an
	// explicit privateKeySecret it resolves to <nodeset>-validator-priv-key.
	if v := nodeSet.Spec.Validator; v != nil {
		// A TmKMS validator signs through the external KMS sidecar and never mounts a local priv-key
		// secret, so reserving its secret — default OR explicit — would wrongly reject another validator
		// that uses that name. Only reserve when the validator actually uses a local key: it does not use
		// TmKMS, it initializes genesis, or it runs create-validator while uploading the generated key to
		// the KMS. In the last two cases the controller still creates/uploads the local priv-key via
		// RequiresPrivKey, so its resolved name is the validator's real consensus key and must be reserved.
		// An explicit privateKeySecret on a pure TmKMS validator is unused and must not be reserved.
		if v.TmKMS == nil || v.Init != nil || tmkmsUploadsGeneratedPrivKey(v) {
			secret := fmt.Sprintf("%s-validator-priv-key", nodeSet.GetName())
			if v.PrivateKeySecret != nil {
				secret = *v.PrivateKeySecret
			}
			if err := registerSecret(".spec.validator", secret); err != nil {
				return err
			}
		}
		if err := registerTmKMS(".spec.validator", v); err != nil {
			return err
		}
		if err := registerGenesisValidators(".spec.validator", v.Init); err != nil {
			return err
		}
	}

	for i, group := range nodeSet.Spec.Nodes {
		if group.Validator == nil || group.GetInstances() == 0 {
			continue
		}
		path := fmt.Sprintf(".spec.nodes[%d].validator", i)
		// A TmKMS validator group signs through the external KMS sidecar and never mounts a local priv-key
		// secret, so neither its explicit privateKeySecret nor its per-instance defaults must be reserved
		// here — another validator may use that name. Only reserve when the group actually uses a local
		// key: it does not use TmKMS, it initializes genesis, or it runs create-validator with Hashicorp
		// uploadGenerated (the last two create/upload the local priv-key via RequiresPrivKey). An explicit
		// privateKeySecret is only valid on a single-instance group (multi-instance groups with
		// privateKeySecret are rejected earlier); otherwise every instance resolves to its own default.
		usesLocalKey := group.Validator.TmKMS == nil || group.Validator.Init != nil || tmkmsUploadsGeneratedPrivKey(group.Validator)
		if !usesLocalKey {
			// Nothing to reserve.
		} else if group.Validator.PrivateKeySecret != nil {
			if err := registerSecret(path, *group.Validator.PrivateKeySecret); err != nil {
				return err
			}
		} else {
			for idx := 0; idx < group.GetInstances(); idx++ {
				secret := fmt.Sprintf("%s-%s-%d-priv-key", nodeSet.GetName(), group.Name, idx)
				if err := registerSecret(path, secret); err != nil {
					return err
				}
			}
		}
		if err := registerTmKMS(path, group.Validator); err != nil {
			return err
		}
		if err := registerGenesisValidators(path, group.Validator.Init); err != nil {
			return err
		}
	}
	return nil
}

// tmkmsUploadsGeneratedPrivKey reports whether a TmKMS validator still has the controller generate a
// local consensus priv-key and upload it to the KMS. Outside of genesis-initializing validators
// (handled separately via .init), this happens when the validator runs create-validator and the
// Hashicorp provider sets uploadGenerated: RequiresPrivKey then creates the default <...>-priv-key
// secret and uploads it to Vault. That generated secret holds the validator's real consensus key, so
// it must be reserved for uniqueness like a local key even though block signing goes through the KMS
// sidecar.
func tmkmsUploadsGeneratedPrivKey(v *NodeSetValidatorConfig) bool {
	if v == nil || v.TmKMS == nil || v.CreateValidator == nil {
		return false
	}
	h := v.TmKMS.Provider.Hashicorp
	return h != nil && h.UploadGenerated
}

// validateUniqueCreateValidatorAccounts rejects two running create-validator validators that would
// submit their create-validator tx from the same operator/valoper account. Two validators collide
// when they resolve to the same account-mnemonic secret — whether that name is set explicitly via
// createValidator.accountMnemonicSecret or left to the generated ChainNode default.
//
// The resolved default name is included for every running create-validator validator so an explicit
// accountMnemonicSecret on one validator cannot silently alias another validator's default account
// secret. Default names match the generated ChainNode account defaults precisely:
// <nodeset>-validator-account for the legacy singleton and <nodeset>-<group>-<index>-account for
// group validators.
//
// "Running" create-validator validators are the legacy singleton .spec.validator and every
// .spec.nodes[].validator group with at least one instance, in each case only when createValidator
// is set.
func (nodeSet *ChainNodeSet) validateUniqueCreateValidatorAccounts() error {
	accountSecrets := map[string]string{}

	register := func(path, secret string) error {
		if prev, ok := accountSecrets[secret]; ok {
			return fmt.Errorf("%s.createValidator resolves to account secret %q already used by %s; each create-validator validator must use a distinct account", path, secret, prev)
		}
		accountSecrets[secret] = path
		return nil
	}

	// Pre-seed the map with every account already claimed by genesis validators (init validator and
	// generated/preserved genesis validator entries). A create-validator that resolves to the same
	// account as a genesis validator would submit a create-validator tx for an operator already in
	// the immutable genesis validator set, causing that tx to fail on-chain.
	if v := nodeSet.Spec.Validator; v != nil && v.Init != nil {
		secret := fmt.Sprintf("%s-validator-account", nodeSet.GetName())
		if v.Init.AccountMnemonicSecret != nil {
			secret = *v.Init.AccountMnemonicSecret
		}
		accountSecrets[secret] = ".spec.validator (genesis)"
		for _, gv := range v.Init.GenesisValidators {
			accountSecrets[gv.AccountMnemonicSecret] = ".spec.validator.init.genesisValidators (genesis)"
		}
	}
	for i, group := range nodeSet.Spec.Nodes {
		if group.Validator == nil || group.Validator.Init == nil || group.GetInstances() == 0 {
			continue
		}
		path := fmt.Sprintf(".spec.nodes[%d].validator (genesis)", i)
		secret := fmt.Sprintf("%s-%s-0-account", nodeSet.GetName(), group.Name)
		if group.Validator.Init.AccountMnemonicSecret != nil {
			secret = *group.Validator.Init.AccountMnemonicSecret
		}
		accountSecrets[secret] = path
		for _, gv := range group.Validator.Init.GenesisValidators {
			accountSecrets[gv.AccountMnemonicSecret] = path + ".init.genesisValidators"
		}
		for idx := 1; idx < group.GetInstances(); idx++ {
			accountSecrets[fmt.Sprintf("%s-%s-%d-account", nodeSet.GetName(), group.Name, idx)] = path
		}
	}

	// Legacy singleton validator: its ChainNode is named <nodeset>-validator, so without an explicit
	// accountMnemonicSecret its account resolves to <nodeset>-validator-account.
	if v := nodeSet.Spec.Validator; v != nil && v.CreateValidator != nil {
		secret := fmt.Sprintf("%s-validator-account", nodeSet.GetName())
		if v.CreateValidator.AccountMnemonicSecret != nil {
			secret = *v.CreateValidator.AccountMnemonicSecret
		}
		if err := register(".spec.validator", secret); err != nil {
			return err
		}
	}

	for i, group := range nodeSet.Spec.Nodes {
		if group.Validator == nil || group.GetInstances() == 0 || group.Validator.CreateValidator == nil {
			continue
		}
		path := fmt.Sprintf(".spec.nodes[%d].validator", i)
		// An explicit accountMnemonicSecret is only valid on a single-instance group (multi-instance
		// create-validator groups with a shared secret are rejected earlier). Otherwise every instance
		// resolves to its own default <nodeset>-<group>-<index>-account.
		if group.Validator.CreateValidator.AccountMnemonicSecret != nil {
			if err := register(path, *group.Validator.CreateValidator.AccountMnemonicSecret); err != nil {
				return err
			}
		} else {
			for idx := 0; idx < group.GetInstances(); idx++ {
				secret := fmt.Sprintf("%s-%s-%d-account", nodeSet.GetName(), group.Name, idx)
				if err := register(path, secret); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// validateUniqueGenesisValidatorAccounts rejects two genesis validators that resolve to the same
// account-mnemonic secret. initGenesis derives the operator/valoper account for each genesis
// validator's gentx from its account mnemonic, so two genesis validators sharing a mnemonic secret
// would produce gentxs for the same operator account — an invalid genesis. Every account that ends
// up in the generated genesis is tracked:
//
//   - the genesis-initializing validator's own account (explicit init.accountMnemonicSecret or the
//     generated ChainNode default),
//   - the generated per-instance accounts of a multi-instance init group (index 1..n-1), which the
//     controller records as genesis validators via groupGenesisValidators, and
//   - every user-provided init.genesisValidators[].accountMnemonicSecret (preserved genesis
//     validators), mirroring how validateUniqueSigningKeys tracks their priv-key secrets.
//
// "Genesis validators" are the genesis-initializing validators (.spec.validator.init and
// .spec.nodes[].validator.init with at least one instance) together with the validators they record
// in genesis.
func (nodeSet *ChainNodeSet) validateUniqueGenesisValidatorAccounts() error {
	accountSecrets := map[string]string{}

	register := func(path, secret string) error {
		if prev, ok := accountSecrets[secret]; ok {
			return fmt.Errorf("%s resolves to genesis account secret %q already used by %s; each genesis validator must use a distinct account", path, secret, prev)
		}
		accountSecrets[secret] = path
		return nil
	}

	// registerInit records a genesis-initializing validator's own account (explicit or the generated
	// ChainNode default defaultAccountSecret) plus every preserved genesis validator it references.
	registerInit := func(path, defaultAccountSecret string, init *GenesisInitConfig) error {
		secret := defaultAccountSecret
		if init.AccountMnemonicSecret != nil {
			secret = *init.AccountMnemonicSecret
		}
		if err := register(path, secret); err != nil {
			return err
		}
		for j, gv := range init.GenesisValidators {
			gvPath := fmt.Sprintf("%s.init.genesisValidators[%d].accountMnemonicSecret", path, j)
			if err := register(gvPath, gv.AccountMnemonicSecret); err != nil {
				return err
			}
		}
		return nil
	}

	// Legacy singleton genesis-initializing validator: its ChainNode is <nodeset>-validator, so its
	// account defaults to <nodeset>-validator-account.
	if v := nodeSet.Spec.Validator; v != nil && v.Init != nil {
		if err := registerInit(".spec.validator", fmt.Sprintf("%s-validator-account", nodeSet.GetName()), v.Init); err != nil {
			return err
		}
	}

	for i, group := range nodeSet.Spec.Nodes {
		if group.Validator == nil || group.Validator.Init == nil || group.GetInstances() == 0 {
			continue
		}
		path := fmt.Sprintf(".spec.nodes[%d].validator", i)
		// Instance 0 is the genesis initializer; its account defaults to <nodeset>-<group>-0-account.
		if err := registerInit(path, fmt.Sprintf("%s-%s-0-account", nodeSet.GetName(), group.Name), group.Validator.Init); err != nil {
			return err
		}
		// Instances 1..n-1 are recorded as generated genesis validators with deterministic accounts
		// <nodeset>-<group>-<index>-account (see groupGenesisValidators).
		for idx := 1; idx < group.GetInstances(); idx++ {
			secret := fmt.Sprintf("%s-%s-%d-account", nodeSet.GetName(), group.Name, idx)
			if err := register(fmt.Sprintf("%s (instance %d)", path, idx), secret); err != nil {
				return err
			}
		}
	}
	return nil
}

// genesisSigningMaterialChanged reports whether the consensus signing material or genesis identity
// of a genesis-initializing validator differs between its old and new config. It compares the
// resolved genesis-signing fingerprint of both sides, so any change to the signing material
// (private-key secret or tmKMS identity), the init chain ID, or the preserved genesis validator list
// is detected, while no-op edits keep the same fingerprint and are not rejected.
func genesisSigningMaterialChanged(oldVal, newVal *NodeSetValidatorConfig, defaultPrivKeySecret string) bool {
	return oldVal.GenesisSigningFingerprint(defaultPrivKeySecret) != newVal.GenesisSigningFingerprint(defaultPrivKeySecret)
}

// GenesisSigningFingerprint returns a stable, opaque fingerprint of the signing material and genesis
// identity that bind a genesis-initializing validator to the immutable genesis validator set:
//
//   - the resolved private-key secret (an explicit privateKeySecret, otherwise defaultPrivKeySecret,
//     the generated ChainNode default);
//   - the concrete tmKMS signing-key identity;
//   - the resolved account derivation settings (accountPrefix, valPrefix, accountHDPath), which live on
//     the validator config and determine the operator/account addresses initGenesis derives; and
//   - the entire .validator.init config. The whole block feeds initGenesis (chainID, assets, stake
//     amount, commission, accounts, additional init commands, the preserved genesis validator list,
//     ...), so it is all immutable after genesis: a recreated init ChainNode would otherwise rebuild a
//     different genesis under the same chain ID. It is serialized rather than cherry-picked so newly
//     added genesis-affecting fields are covered automatically.
//
// Two configs with the same fingerprint produce the same genesis (membership, consensus keys and
// gentx parameters), so a changed fingerprint means a genesis-affecting change. Field separators are
// non-printable bytes so distinct fields cannot collide.
func (v *NodeSetValidatorConfig) GenesisSigningFingerprint(defaultPrivKeySecret string) string {
	if v == nil {
		return genesisSigningFingerprint(nil, nil, nil, nil, "", "", "", defaultPrivKeySecret)
	}
	return genesisSigningFingerprint(v.PrivateKeySecret, v.TmKMS, v.Init, v.Info, v.GetAccountPrefix(), v.GetValPrefix(), v.GetAccountHDPath(), defaultPrivKeySecret)
}

// genesisSigningFingerprint is the component-based core of GenesisSigningFingerprint, taking the
// signing-material, validator info, account and init fields directly so it can serve both
// NodeSetValidatorConfig (ChainNodeSet) and ValidatorConfig (ChainNode), which share these fields but
// are distinct types. .validator.info (moniker/details/website/identity) is included because
// initGenesis bakes it into the init validator's gentx.
func genesisSigningFingerprint(privateKeySecret *string, tmKMS *TmKMS, init *GenesisInitConfig, info *ValidatorInfo, accountPrefix, valPrefix, accountHDPath, defaultPrivKeySecret string) string {
	secret := defaultPrivKeySecret
	if privateKeySecret != nil {
		secret = *privateKeySecret
	}
	var initJSON []byte
	if init != nil {
		// The account derivation fields also have deprecated copies inside init; they are included
		// resolved (accountPrefix/valPrefix/accountHDPath) above, so null the init-level copies here to
		// avoid flagging a no-op move of the same value between the validator and init levels.
		initCopy := init.DeepCopy()
		initCopy.AccountPrefix = nil
		initCopy.ValPrefix = nil
		initCopy.AccountHDPath = nil
		// json.Marshal is deterministic for this config (struct fields in declaration order, no maps),
		// so equal init blocks always produce equal bytes.
		initJSON, _ = json.Marshal(initCopy)
	}
	var infoJSON []byte
	if info != nil {
		infoJSON, _ = json.Marshal(info)
	}
	tmKMSID, _ := tmKMSSigningKeyIdentity(tmKMS)

	return strings.Join([]string{secret, tmKMSID, accountPrefix, valPrefix, accountHDPath, string(infoJSON), string(initJSON)}, "\x00")
}

// tmKMSSigningKeyIdentity returns a stable identifier for the concrete signing key a tmKMS config
// points at, and whether one is configured. Only a fully specified provider yields an identity; an
// unconfigured provider has no concrete key to compare and is skipped (reported as not configured).
// The null-byte separator keeps the address and key fields unambiguous in the composite key.
func tmKMSSigningKeyIdentity(t *TmKMS) (string, bool) {
	if t == nil {
		return "", false
	}
	if h := t.Provider.Hashicorp; h != nil {
		if h.Address == "" || h.Key == "" {
			return "", false
		}
		return fmt.Sprintf("hashicorp\x00%s\x00%s", h.Address, h.Key), true
	}
	return "", false
}
