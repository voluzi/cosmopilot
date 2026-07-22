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
	// A ChainNodeSet's own name is never a resource it derives (every derived name carries a
	// "-<group>"/"-signer"/"-seed"/ordinal segment), so there is no set-name suffix to reserve; the
	// concrete collisions a nodeset can cause are validated inside Validate.
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

func (nodeSet *ChainNodeSet) tmKMSDeprecationWarnings() admission.Warnings {
	warnings := admission.Warnings{}
	if nodeSet.Spec.Validator != nil && nodeSet.Spec.Validator.TmKMS != nil {
		warnings = append(warnings, ".spec.validator.tmKMS is deprecated and will be removed in a future version; migrate to .spec.cosmosigner")
		if hashicorp := nodeSet.Spec.Validator.TmKMS.Provider.Hashicorp; hashicorp != nil && hashicorp.AutoRenewToken {
			warnings = append(warnings, ".spec.validator.tmKMS.provider.hashicorp.autoRenewToken uses the deprecated vault-token-renewer sidecar; migrate to .spec.cosmosigner, which renews Vault tokens internally")
		}
	}
	for i, group := range nodeSet.Spec.Nodes {
		if group.Validator != nil && group.Validator.TmKMS != nil {
			warnings = append(warnings, fmt.Sprintf(".spec.nodes[%d].validator.tmKMS is deprecated and will be removed in a future version; migrate to .spec.nodes[%d].cosmosigner", i, i))
			if hashicorp := group.Validator.TmKMS.Provider.Hashicorp; hashicorp != nil && hashicorp.AutoRenewToken {
				warnings = append(warnings, fmt.Sprintf(".spec.nodes[%d].validator.tmKMS.provider.hashicorp.autoRenewToken uses the deprecated vault-token-renewer sidecar; migrate to .spec.nodes[%d].cosmosigner, which renews Vault tokens internally", i, i))
			}
		}
	}
	return warnings
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

	// CosmoGuard is not supported on the legacy singleton .spec.validator. With the in-pod sidecar
	// removed, a guard is only reconciled for .spec.nodes groups (and standalone ChainNodes), so
	// enabling it here would leave the validator's API traffic silently unguarded. Reject it (rather
	// than silently dropping protection) and steer users to the supported form: a .spec.nodes
	// validator group with cosmoGuard enabled, which the per-group guard fronts.
	if nodeSet.Spec.Validator != nil && nodeSet.Spec.Validator.Config.CosmoGuardEnabled() {
		return nil, fmt.Errorf(".spec.validator.config.cosmoGuard is not supported on the legacy singleton validator; define the validator as a .spec.nodes group with cosmoGuard enabled instead")
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
		// An empty group name would derive broken child names (<nodeset>--<index>) and doubles as the
		// internal "no validator group" sentinel in signer resolution, silently disabling
		// validator-targeted safeguards.
		if group.Name == "" {
			return nil, fmt.Errorf(".spec.nodes[%d].name must not be empty", i)
		}

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

		// The CosmoGuard dashboard port must not collide with a port the guard Service already exposes.
		if err := group.GetServiceConfig().ValidateCosmoGuardDashboard(); err != nil {
			return nil, fmt.Errorf(".spec.nodes[%d] %w", i, err)
		}

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
			// A multi-instance validator group WITHOUT a cosmosigner runs one validator per instance,
			// each of which must sign with its own consensus key. A shared privateKeySecret or a shared
			// tmKMS key would make every instance sign with the same key (double-signing), so both are
			// rejected regardless of genesis mode; the controller generates a distinct key per instance.
			// A cosmosigner-targeted group instead holds ONE consensus identity (the signer's) and its
			// nodes mount no local key, so an explicit privateKeySecret there names the signer's
			// identity/import source and is allowed. tmKMS stays rejected either way: it is a per-pod
			// sidecar, and cosmosigner+tmKMS are mutually exclusive anyway.
			signerTargeted := nodeSet.groupCosmosigner(group.Name) != nil
			if group.GetInstances() > 1 && group.Validator.PrivateKeySecret != nil && !signerTargeted {
				return nil, fmt.Errorf(".spec.nodes[%d].validator.privateKeySecret cannot be set when the validator group has multiple instances", i)
			}
			if group.GetInstances() > 1 && group.Validator.TmKMS != nil {
				return nil, fmt.Errorf(".spec.nodes[%d].validator.tmKMS cannot be set when the validator group has multiple instances (every instance would sign with the same key)", i)
			}
			// For a genesis-initializing multi-instance group (no cosmosigner) the controller also
			// manages the per-instance account mnemonic secrets, so a shared one cannot be provided.
			// A cosmosigner-targeted group has a single validator (instance 0's flow), so its one
			// account may be named explicitly.
			if group.Validator.Init != nil && group.GetInstances() > 1 && group.Validator.Init.AccountMnemonicSecret != nil && !signerTargeted {
				return nil, fmt.Errorf(".spec.nodes[%d].validator.init.accountMnemonicSecret cannot be set when validator.init is used with multiple instances", i)
			}
			// A multi-instance createValidator group (no cosmosigner) derives a distinct per-instance
			// account for each generated validator. A single shared accountMnemonicSecret would make
			// every instance submit a create-validator tx for the same operator account, so it is
			// rejected (mirroring the init guard above). A cosmosigner-targeted group runs only
			// instance 0's create-validator flow, so its explicit account is allowed.
			if group.Validator.CreateValidator != nil && group.GetInstances() > 1 && group.Validator.CreateValidator.AccountMnemonicSecret != nil && !signerTargeted {
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
	if err := nodeSet.validateCosmosigner(old); err != nil {
		return nil, err
	}
	if err := nodeSet.validateCosmosignerUpdate(old); err != nil {
		return nil, err
	}

	// Reject any node-group or signer name whose derived resource names would exceed the 63-character
	// DNS label limit (and then fail every reconcile). Runs on create and update.
	if err := nodeSet.validateGeneratedNameLengths(); err != nil {
		return nil, err
	}

	// Reject a group whose derived Service name collides with another group's/route's derived Service
	// (e.g. guarded group "g" vs a group named "g-cg", or group "g" vs a group named "g-internal").
	if err := nodeSet.validateServiceNameCollisions(); err != nil {
		return nil, err
	}

	// Reject a node group whose generated child ChainNode names collide with reserved StatefulSet-child
	// patterns (…-cg-<n> / …-signer-<n>). Runs on create and update (via obj.Validate(nil) on create),
	// gated on live instances and grandfathering groups already active in old, so an existing
	// ChainNodeSet is never locked out of unrelated updates while a newly added/activated group is
	// still caught before its children are stranded.
	if err := nodeSet.validateGroupChildReservedNames(old); err != nil {
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
			// A managed signer migration may intentionally change signing keys. Keep the non-signing
			// genesis configuration immutable while leaving the on-chain key choice to the user.
			changed := genesisSigningMaterialChanged(old.Spec.Validator, nodeSet.Spec.Validator, defaultPrivKeySecret)
			if old.groupCosmosigner(ReservedValidatorGroupName) != nil || nodeSet.groupCosmosigner(ReservedValidatorGroupName) != nil {
				changed = genesisConfigurationFingerprint(old.Spec.Validator.Init, old.Spec.Validator.Info, old.Spec.Validator.GetAccountPrefix(), old.Spec.Validator.GetValPrefix(), old.Spec.Validator.GetAccountHDPath()) !=
					genesisConfigurationFingerprint(nodeSet.Spec.Validator.Init, nodeSet.Spec.Validator.Info, nodeSet.Spec.Validator.GetAccountPrefix(), nodeSet.Spec.Validator.GetValPrefix(), nodeSet.Spec.Validator.GetAccountHDPath())
			}
			if changed {
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
			// A multi-instance init group's semantics depend on whether a cosmosigner targets it: with
			// a signer it is ONE genesis validator with redundant signing endpoints, without one it is
			// N genesis validators. Toggling that after genesis would flip instances 1..n-1 between
			// "redundant endpoint" and "genesis validator" — either stranding recorded genesis
			// validators without their init flow or desiring init validators absent from the immutable
			// genesis — so the signer-targeted-ness of such a group is frozen.
			oldSignerTargeted := old.groupCosmosigner(group.Name) != nil
			newSignerTargeted := nodeSet.groupCosmosigner(group.Name) != nil
			if og.GetInstances() > 1 && oldSignerTargeted != newSignerTargeted {
				return nil, fmt.Errorf(".spec.nodes[%d]: a cosmosigner cannot be added to or removed from multi-instance genesis-initializing validator group %q after genesis has been created: it changes which instances are genesis validators", i, group.Name)
			}
			// Scaling changes the genesis validator set for a plain init group (one validator per
			// instance). A cosmosigner-targeted group holds ONE identity — its instances are redundant
			// signing endpoints, only instance 0 is in genesis — so scaling it (while it stays
			// signer-targeted on both sides, keeping instance 0 intact) does not touch the genesis set.
			signerTargetedBothSides := oldSignerTargeted && newSignerTargeted
			if group.GetInstances() > og.GetInstances() && !signerTargetedBothSides {
				return nil, fmt.Errorf(".spec.nodes[%d] genesis-initializing validator group %q cannot be scaled up after creation", i, group.Name)
			}
			// Shrinking is rejected too: the removed validators' voting power stays in the immutable
			// genesis validator set, so dropping them can halt the chain (it may never reach the
			// 2/3 voting power required to produce blocks). There is no API field to opt into this
			// unsafe operation, so it is rejected outright; decommissioning must be done on-chain.
			// A signer-targeted group may shrink to no fewer than one instance (instance 0 must stay).
			if group.GetInstances() < og.GetInstances() && !(signerTargetedBothSides && group.GetInstances() >= 1) {
				return nil, fmt.Errorf(".spec.nodes[%d] genesis-initializing validator group %q cannot be scaled down after creation: its validators are part of the immutable genesis validator set", i, group.Name)
			}
			// The group's validators are in the immutable genesis with fixed consensus keys and gentx
			// parameters. Reject changing their signing material (privateKeySecret or tmKMS key) or any
			// genesis parameter in .init (assets, stake, accounts, genesisValidators, ...) — a recreated
			// genesis would otherwise differ. Non-signer multi-instance groups cannot set
			// privateKeySecret/tmKMS (rejected above) and their per-instance keys derive from stable
			// names, so this only flags real changes.
			defaultPrivKeySecret := fmt.Sprintf("%s-%s-0-priv-key", nodeSet.GetName(), group.Name)
			changed := genesisSigningMaterialChanged(og.Validator, group.Validator, defaultPrivKeySecret)
			if old.groupCosmosigner(group.Name) != nil || nodeSet.groupCosmosigner(group.Name) != nil {
				changed = genesisConfigurationFingerprint(og.Validator.Init, og.Validator.Info, og.Validator.GetAccountPrefix(), og.Validator.GetValPrefix(), og.Validator.GetAccountHDPath()) !=
					genesisConfigurationFingerprint(group.Validator.Init, group.Validator.Info, group.Validator.GetAccountPrefix(), group.Validator.GetValPrefix(), group.Validator.GetAccountHDPath())
			}
			if changed {
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

	return nodeSet.tmKMSDeprecationWarnings(), nil
}

// validateCosmosigner validates every managed cosmosigner a ChainNodeSet runs: the top-level
// .spec.cosmosigner (which selects node groups) and each per-group .spec.nodes[].cosmosigner (whose
// target is fixed to its enclosing group). Each signer signs for a single consensus identity shared
// across the nodes it connects to — a multi-instance validator group with a cosmosigner is ONE
// validator with N redundant signing endpoints (multiple validators require multiple groups, each
// with its own signer and key).
//
// old is the previous revision on the update path (nil on create); it enables lifecycle-diff and
// migration-readiness checks against the controller-recorded applied public key.
func (nodeSet *ChainNodeSet) validateCosmosigner(old *ChainNodeSet) error {
	legacyStandaloneSignerServices := map[string]struct{}{}
	rememberServiceNames := func(source *ChainNodeSet) {
		for i := range source.Spec.Nodes {
			legacyStandaloneSignerServices[source.Spec.Nodes[i].GetServiceName(source)] = struct{}{}
		}
		for i := range source.Spec.Ingresses {
			legacyStandaloneSignerServices[source.Spec.Ingresses[i].GetName(source)] = struct{}{}
		}
		for i := range source.Spec.GatewayRoutes {
			name := fmt.Sprintf("%s-global-%s", source.GetName(), source.Spec.GatewayRoutes[i].Name)
			legacyStandaloneSignerServices[name] = struct{}{}
		}
	}
	if old != nil {
		rememberServiceNames(old)
	} else if nodeSet.Status.LegacySignerServiceNamesInitialized {
		for _, name := range nodeSet.Status.LegacySignerServiceNames {
			legacyStandaloneSignerServices[name] = struct{}{}
		}
	}

	// A Service named "<node>-signer" or "<node>-signer-privval" collides with the raft/discovery
	// Service of a standalone ChainNode named <node>, even when this ChainNodeSet has no signer.
	checkStandaloneSignerService := func(path, serviceName string) error {
		for _, suffix := range []string{"-signer", "-signer-privval"} {
			if strings.HasSuffix(serviceName, suffix) {
				if _, legacy := legacyStandaloneSignerServices[serviceName]; legacy {
					return nil
				}
				return fmt.Errorf("%s derives Service name %q, which collides with a standalone ChainNode cosmosigner Service: choose a name without a reserved signer suffix", path, serviceName)
			}
		}
		return nil
	}
	checkStandaloneSignerServices := func() error {
		for i, g := range nodeSet.Spec.Nodes {
			if err := checkStandaloneSignerService(fmt.Sprintf(".spec.nodes[%d].name", i), g.GetServiceName(nodeSet)); err != nil {
				return err
			}
		}
		for i := range nodeSet.Spec.Ingresses {
			if err := checkStandaloneSignerService(fmt.Sprintf(".spec.ingresses[%d].name", i), nodeSet.Spec.Ingresses[i].GetName(nodeSet)); err != nil {
				return err
			}
		}
		for i := range nodeSet.Spec.GatewayRoutes {
			serviceName := fmt.Sprintf("%s-global-%s", nodeSet.GetName(), nodeSet.Spec.GatewayRoutes[i].Name)
			if err := checkStandaloneSignerService(fmt.Sprintf(".spec.gatewayRoutes[%d].name", i), serviceName); err != nil {
				return err
			}
		}
		return nil
	}

	anySigner := nodeSet.Spec.Cosmosigner != nil
	for i := range nodeSet.Spec.Nodes {
		if nodeSet.Spec.Nodes[i].Cosmosigner != nil {
			anySigner = true
		}
	}
	if !anySigner {
		return checkStandaloneSignerServices()
	}

	// Every derived signer resource name must be unique and must not collide with a node group's own
	// Services: a group named "<g>-signer" has a group Service colliding with group <g>'s signer
	// StatefulSet/raft Service. Either collision would have two reconcilers overwrite one object.
	signerNames := map[string]string{}
	for _, s := range nodeSet.ResolveCosmosigners() {
		path := nodeSet.signerFieldPath(s)
		if prev, dup := signerNames[s.Name]; dup {
			return fmt.Errorf("%s derives signer resource name %q, which %s also derives: rename one of the groups so every signer name is unique", path, s.Name, prev)
		}
		signerNames[s.Name] = path
	}
	for i, g := range nodeSet.Spec.Nodes {
		groupService := fmt.Sprintf("%s-%s", nodeSet.GetName(), g.Name)
		if _, collides := signerNames[groupService]; collides {
			return fmt.Errorf(".spec.nodes[%d].name %q collides with a cosmosigner's derived resource name %q: rename the group", i, g.Name, groupService)
		}
		if trimmed, ok := strings.CutSuffix(groupService, "-privval"); ok {
			if _, collides := signerNames[trimmed]; collides {
				return fmt.Errorf(".spec.nodes[%d].name %q collides with a cosmosigner's derived discovery Service name %q: rename the group", i, g.Name, groupService)
			}
		}
	}

	checkRouteService := func(path, serviceName string) error {
		if signerPath, collides := signerNames[serviceName]; collides {
			return fmt.Errorf("%s derives global route Service name %q, which collides with the raft Service from %s", path, serviceName, signerPath)
		}
		if signerName, ok := strings.CutSuffix(serviceName, "-privval"); ok {
			if signerPath, collides := signerNames[signerName]; collides {
				return fmt.Errorf("%s derives global route Service name %q, which collides with the discovery Service from %s", path, serviceName, signerPath)
			}
		}
		return nil
	}
	for i := range nodeSet.Spec.Ingresses {
		ing := &nodeSet.Spec.Ingresses[i]
		path := fmt.Sprintf(".spec.ingresses[%d]", i)
		if err := checkRouteService(path, ing.GetName(nodeSet)); err != nil {
			return err
		}
		if ing.UseInternal() {
			if err := checkRouteService(path, fmt.Sprintf("%s-internal", ing.GetName(nodeSet))); err != nil {
				return err
			}
		}
	}
	for i := range nodeSet.Spec.GatewayRoutes {
		gw := &nodeSet.Spec.GatewayRoutes[i]
		path := fmt.Sprintf(".spec.gatewayRoutes[%d]", i)
		serviceName := fmt.Sprintf("%s-global-%s", nodeSet.GetName(), gw.Name)
		if err := checkRouteService(path, serviceName); err != nil {
			return err
		}
		if gw.UseInternal() {
			if err := checkRouteService(path, serviceName+"-internal"); err != nil {
				return err
			}
		}
	}
	if err := checkStandaloneSignerServices(); err != nil {
		return err
	}

	// Shape rules for the top-level signer (target selection over node groups) and each per-group
	// signer (target fixed to its enclosing group).
	if c := nodeSet.Spec.Cosmosigner; c != nil {
		if err := nodeSet.validateTopLevelCosmosigner(c); err != nil {
			return err
		}
	}
	for i := range nodeSet.Spec.Nodes {
		g := &nodeSet.Spec.Nodes[i]
		if g.Cosmosigner == nil {
			continue
		}
		if err := nodeSet.validateGroupCosmosigner(i, g); err != nil {
			return err
		}
	}

	// Per-resolved-signer backend/target consistency (software/uploadGenerated/registers rules), then
	// cross-signer invariants (target uniqueness, derived name lengths).
	for _, s := range nodeSet.ResolveCosmosigners() {
		if err := nodeSet.validateResolvedSigner(old, s); err != nil {
			return err
		}
	}
	if err := nodeSet.validateCosmosignerTargetUniqueness(); err != nil {
		return err
	}
	return nil
}

// validateTopLevelCosmosigner validates the shape of .spec.cosmosigner: exactly-one-backend (via
// Cosmosigner.Validate) and how its nodeGroups select targets. A single top-level signer holds one
// consensus identity, so it may target at most one validator. A multi-instance validator group is a
// valid target: its instances are redundant signing endpoints of that one identity, not N
// validators (multiple validators require multiple groups, each with its own signer).
func (nodeSet *ChainNodeSet) validateTopLevelCosmosigner(c *Cosmosigner) error {
	if err := c.Validate(".spec.cosmosigner", true); err != nil {
		return err
	}

	groups := make(map[string]NodeGroupSpec, len(nodeSet.Spec.Nodes))
	for _, g := range nodeSet.Spec.Nodes {
		groups[g.Name] = g
	}

	validatorTargets := 0
	if len(c.NodeGroups) == 0 {
		if nodeSet.Spec.Validator == nil {
			return fmt.Errorf(".spec.cosmosigner.nodeGroups is required when .spec.validator is not set")
		}
		if nodeSet.Spec.Validator.TmKMS != nil {
			return fmt.Errorf(".spec.cosmosigner and .spec.validator.tmKMS are mutually exclusive")
		}
		validatorTargets = 1
	} else {
		seen := map[string]struct{}{}
		for i, name := range c.NodeGroups {
			if _, dup := seen[name]; dup {
				return fmt.Errorf(".spec.cosmosigner.nodeGroups[%d] %q is listed more than once", i, name)
			}
			seen[name] = struct{}{}

			// The reserved "validator" name targets the legacy .spec.validator singleton, not a
			// group in .spec.nodes: handle it here so a single signer can dial both the legacy
			// validator AND a sentry/fullnode group together. The per-target rules below (multi-instance,
			// tmKMS) only apply to a real .spec.nodes[] group.
			if name == ReservedValidatorGroupName {
				if nodeSet.Spec.Validator == nil {
					return fmt.Errorf(".spec.cosmosigner.nodeGroups[%d] %q targets the legacy .spec.validator, which is not set", i, name)
				}
				if nodeSet.Spec.Validator.TmKMS != nil {
					return fmt.Errorf(".spec.cosmosigner cannot target the legacy .spec.validator, which uses tmKMS: cosmosigner and tmKMS are mutually exclusive")
				}
				validatorTargets++
				continue
			}

			group, ok := groups[name]
			if !ok {
				return fmt.Errorf(".spec.cosmosigner.nodeGroups[%d] %q does not match any group in .spec.nodes", i, name)
			}
			if group.GetInstances() == 0 {
				return fmt.Errorf(".spec.cosmosigner cannot target group %q with zero instances", name)
			}
			if group.Validator != nil {
				if group.Validator.TmKMS != nil {
					return fmt.Errorf(".spec.cosmosigner cannot target group %q which uses tmKMS: cosmosigner and tmKMS are mutually exclusive", name)
				}
				validatorTargets++
			}
		}
	}
	if validatorTargets > 1 {
		return fmt.Errorf(".spec.cosmosigner cannot target more than one validator: a signer holds a single consensus identity")
	}
	return nil
}

// validateGroupCosmosigner validates a per-group .spec.nodes[i].cosmosigner: exactly-one-backend and
// no nodeGroups (its target is the enclosing group), the group must have at least one instance, and
// the group's validator must not also use tmKMS. A multi-instance group is fine — validator or
// sentry — the signer holds ONE consensus identity and dials every instance pod (for a validator
// group the instances are redundant signing endpoints of the same validator, never N validators).
func (nodeSet *ChainNodeSet) validateGroupCosmosigner(i int, g *NodeGroupSpec) error {
	path := fmt.Sprintf(".spec.nodes[%d].cosmosigner", i)
	if err := g.Cosmosigner.Validate(path, false); err != nil {
		return err
	}
	if g.GetInstances() == 0 {
		return fmt.Errorf("%s cannot be set on group %q with zero instances", path, g.Name)
	}
	if g.Validator != nil && g.Validator.TmKMS != nil {
		return fmt.Errorf("%s and .spec.nodes[%d].validator.tmKMS are mutually exclusive", path, i)
	}
	return nil
}

// validateResolvedSigner applies the backend/target consistency rules to one resolved signer,
// regardless of whether it came from the top-level or a per-group block. A sentry signer (no
// validator target) must carry its own key; a validator-targeted signer must reuse that validator's
// selected consensus key.
func (nodeSet *ChainNodeSet) validateResolvedSigner(old *ChainNodeSet, s ResolvedSigner) error {
	c := s.Spec
	path := nodeSet.signerFieldPath(s)

	if s.ValidatorGroup == "" {
		// Sentry signer: no controller-registered validator key to reuse, so key material must be
		// supplied explicitly.
		if c.UsesSoftwareBackend() && (c.Backend.Software.PrivateKeySecret == nil || *c.Backend.Software.PrivateKeySecret == "") {
			return fmt.Errorf("%s.backend.software.privateKeySecret is required when no validator is targeted", path)
		}
		if c.UsesVaultBackend() && c.Backend.Vault.UploadGenerated {
			return fmt.Errorf("%s.backend.vault.uploadGenerated requires targeting a validator whose generated key can be imported", path)
		}
		return nil
	}

	targetValidator := nodeSet.validatorConfigForGroup(s.ValidatorGroup)
	if c.UsesVaultBackend() && c.VaultUploadsGenerated(targetValidator.Init != nil) && c.Backend.Vault.GetKeyVersion() != 1 {
		return fmt.Errorf("%s.backend.vault.uploadGenerated requires keyVersion 1 because Vault imports create the initial key version", path)
	}

	// With a validator target the signer uses that validator's own key, so an explicit software secret
	// (which could point elsewhere) is not allowed.
	if c.UsesSoftwareBackend() && c.Backend.Software.PrivateKeySecret != nil && *c.Backend.Software.PrivateKeySecret != "" {
		return fmt.Errorf("%s.backend.software.privateKeySecret cannot be set when targeting a validator: the validator's own key is used", path)
	}

	// When the targeted validator registers a freshly-generated consensus key on-chain (genesis init
	// or create-validator), Cosmopilot registers the validator's local key. The signer must therefore
	// use that same key: only the software backend (which references it) or Vault with uploadGenerated
	// (which imports it — implicitly auto-defaulted for genesis-init targets). A pre-provisioned
	// Vault/GCP key may differ from the locally registered pubkey. On an established chain this is an
	// explicit operator migration: Cosmopilot handles break-before-make and state reset, while the user
	// remains responsible for the on-chain validator key. Creating a new validator is not a migration:
	// its locally registered key must still match the signer.
	registers := targetValidator.Init != nil || targetValidator.CreateValidator != nil
	registrationRecorded := func(candidate *ChainNodeSet) bool {
		for _, validator := range candidate.Status.Validators {
			if validator.Group == s.ValidatorGroup && validator.Address != "" && validator.PubKey != "" && validator.Status != "" {
				return true
			}
		}
		return false
	}
	migrationWaiver := false
	if old != nil && old.Status.ChainID != "" {
		migrationWaiver = registrationRecorded(old)
	} else if old == nil && nodeSet.Status.ChainID != "" {
		// No-webhook path: the previous spec is unavailable, so a non-empty recorded signing digest that
		// MATCHES this resolved signer proves the exact signer identity rolled out and served. Registration
		// alone (a validator address) does not prove a NEWLY configured pre-provisioned Vault/GCP backend
		// holds the validator's consensus key, so a missing or mismatched digest keeps the signer subject
		// to the software/uploadGenerated rule.
		st := nodeSet.GetCosmosignerStatus(s.Name)
		migrationWaiver = registrationRecorded(nodeSet) && st != nil && st.SigningDigest != "" && st.SigningDigest == s.Digest()
	}
	if registers && !migrationWaiver {
		matches := c.UsesSoftwareBackend() || c.VaultUploadsGenerated(targetValidator.Init != nil)
		if !matches {
			return fmt.Errorf("%s targeting a validator that initializes genesis or uses createValidator requires the software backend or vault.uploadGenerated so the registered consensus key matches the signer", path)
		}
	}

	hasExplicitKey := targetValidator.PrivateKeySecret != nil && *targetValidator.PrivateKeySecret != ""

	// uploadGenerated imports the targeted validator's key into Vault, so that key must exist: the
	// validator must generate one (init/createValidator) or supply an explicit privateKeySecret.
	if c.UsesVaultBackend() && c.Backend.Vault.UploadGenerated {
		if !registers && !hasExplicitKey {
			return fmt.Errorf("%s.backend.vault.uploadGenerated requires the targeted validator to initialize genesis, use createValidator, or set an explicit privateKeySecret to import", path)
		}
	}

	// The software backend mounts the targeted validator's key secret. A plain external-genesis
	// validator never creates its default key, so an explicit privateKeySecret is required.
	if c.UsesSoftwareBackend() && !registers && !hasExplicitKey {
		return fmt.Errorf("%s software backend targeting a validator that consumes an external genesis requires the validator to set privateKeySecret: its consensus key is not generated", path)
	}

	return nil
}

// validateCosmosignerTargetUniqueness rejects a node group targeted by both the top-level
// .spec.cosmosigner and its own .spec.nodes[].cosmosigner: two signers would then dial the same nodes
// and fight over the single privval connection each node accepts.
func (nodeSet *ChainNodeSet) validateCosmosignerTargetUniqueness() error {
	topTargets := map[string]struct{}{}
	if c := nodeSet.Spec.Cosmosigner; c != nil {
		if len(c.NodeGroups) == 0 {
			if nodeSet.Spec.Validator != nil {
				topTargets[ReservedValidatorGroupName] = struct{}{}
			}
		} else {
			for _, name := range c.NodeGroups {
				topTargets[name] = struct{}{}
			}
		}
	}
	for i := range nodeSet.Spec.Nodes {
		g := &nodeSet.Spec.Nodes[i]
		if g.Cosmosigner == nil {
			continue
		}
		if _, dup := topTargets[g.Name]; dup {
			return fmt.Errorf(".spec.nodes[%d].cosmosigner conflicts with .spec.cosmosigner, which already targets group %q; a group can be signed by only one signer", i, g.Name)
		}
	}
	return nil
}

// validateGeneratedNameLengths rejects any derived resource name that would exceed the 63-character
// DNS label limit. It checks, per node group, the group base (its always-on -internal Service and,
// when CosmoGuard is enabled, the -cg-upstream Service) and the group's longest child pod name; the
// legacy singleton .spec.validator's own ChainNode base; each global ingress/gateway route's
// always-created -internal Service; cosmoseed's headless and highest-ordinal instance Services (when
// enabled); and, per resolved signer, the discovery Service "<signer>-privval".
func (nodeSet *ChainNodeSet) validateGeneratedNameLengths() error {
	const subject = "ChainNodeSet or node-group name"
	for i := range nodeSet.Spec.Nodes {
		g := &nodeSet.Spec.Nodes[i]
		base := g.GetServiceName(nodeSet)
		if err := validateDerivedNameLengths(base, subject, nameFeatures{
			cosmoguardGroup: g.GetServiceConfig().CosmoGuardEnabled(),
		}); err != nil {
			return err
		}
		// The group's highest-ordinal child ChainNode carries the longest always-on 63-bound name.
		// A scaled-to-zero group has no child ChainNodes, so skip the nonexistent "<base>--1" child.
		if g.GetInstances() > 0 {
			child := fmt.Sprintf("%s-%d", base, g.GetInstances()-1)
			if err := validateDerivedNameLengths(child, subject, nameFeatures{}); err != nil {
				return err
			}
		}
	}
	// The legacy singleton .spec.validator is a ChainNode named "<nodeSet>-validator" that is not in
	// .spec.nodes; validate its always-on -internal Service (its longest always-on 63-bound name).
	if nodeSet.Spec.Validator != nil {
		base := fmt.Sprintf("%s-%s", nodeSet.GetName(), ReservedValidatorGroupName)
		if err := validateDerivedNameLengths(base, subject, nameFeatures{}); err != nil {
			return err
		}
	}
	// Each global ingress/gateway route always creates a "<nodeSet>-global-<route>-internal" Service
	// (getGlobalInternalServiceSpec runs unconditionally, independent of useInternal), and that
	// -internal name is the longest 63-bound name the route derives. A long route name pushes it past
	// the label limit even when every node-group name fits.
	for i := range nodeSet.Spec.Ingresses {
		route := &nodeSet.Spec.Ingresses[i]
		if svc := fmt.Sprintf("%s-internal", route.GetName(nodeSet)); len(svc) > 63 {
			return fmt.Errorf("the global ingress Service name %q (%d chars) exceeds the 63-character limit: shorten the ChainNodeSet name or the ingress route name %q", svc, len(svc), route.Name)
		}
	}
	for i := range nodeSet.Spec.GatewayRoutes {
		route := &nodeSet.Spec.GatewayRoutes[i]
		if svc := fmt.Sprintf("%s-global-%s-internal", nodeSet.GetName(), route.Name); len(svc) > 63 {
			return fmt.Errorf("the global gateway Service name %q (%d chars) exceeds the 63-character limit: shorten the ChainNodeSet name or the gateway route name %q", svc, len(svc), route.Name)
		}
	}
	// Cosmoseed derives a bare headless Service "<nodeSet>-seed-headless" and, per instance, a
	// StatefulSet whose pods and per-instance "<nodeSet>-seed-<i>-internal" Service are 63-bound. The
	// highest-ordinal instance carries the longest such name. Only checked when cosmoseed is enabled.
	if nodeSet.Spec.Cosmoseed.IsEnabled() {
		if svc := fmt.Sprintf("%s-seed-headless", nodeSet.GetName()); len(svc) > 63 {
			return fmt.Errorf("the cosmoseed headless Service name %q (%d chars) exceeds the 63-character limit: shorten the ChainNodeSet name", svc, len(svc))
		}
		if inst := nodeSet.Spec.Cosmoseed.GetInstances(); inst > 0 {
			if svc := fmt.Sprintf("%s-seed-%d-internal", nodeSet.GetName(), inst-1); len(svc) > 63 {
				return fmt.Errorf("the cosmoseed instance Service name %q (%d chars) exceeds the 63-character limit: shorten the ChainNodeSet name or reduce cosmoseed instances", svc, len(svc))
			}
		}
	}
	// The discovery Service name already contains the "-signer" segment, so keep a dedicated check
	// rather than routing through validateDerivedNameLengths (which would re-append it).
	for _, s := range nodeSet.ResolveCosmosigners() {
		if svc := s.Name + "-privval"; len(svc) > 63 {
			return fmt.Errorf("the cosmosigner discovery Service name %q (%d chars) exceeds the 63-character limit: shorten the ChainNodeSet or node-group name", svc, len(svc))
		}
	}
	return nil
}

// validateServiceNameCollisions rejects two distinct owners (node groups, global ingress/gateway
// routes) that derive the same Service name and would fight over it — both share the ChainNodeSet
// owner, so the ownership guard does not stop the reconcile paths from overwriting each other. Every
// group derives a main Service "<nodeSet>-<g>", an always-on "<nodeSet>-<g>-internal" Service, and,
// when CosmoGuard is enabled, a guard Service "<nodeSet>-<g>-cg"; every route always creates a
// "<nodeSet>-global-<route>-internal" Service plus its public backing Service. Collisions this
// catches include a guarded group "g" vs a group named "g-cg" (guard vs main) and a group "g" vs a
// group named "g-internal" (internal Service vs main). Mirrors the "<g>-signer" group/signer
// collision check in validateCosmosignerTargetUniqueness.
func (nodeSet *ChainNodeSet) validateServiceNameCollisions() error {
	owners := map[string]string{}
	add := func(name, owner string) error {
		if prev, taken := owners[name]; taken && prev != owner {
			return fmt.Errorf("Service name %q is derived by both %s and %s: rename one of them", name, prev, owner)
		}
		owners[name] = owner
		return nil
	}
	for i := range nodeSet.Spec.Nodes {
		g := &nodeSet.Spec.Nodes[i]
		owner := fmt.Sprintf("node group %q", g.Name)
		base := g.GetServiceName(nodeSet)
		names := []string{base, base + "-internal"}
		if g.GetServiceConfig().CosmoGuardEnabled() {
			// A guarded group derives a CosmoGuard client Service (base+"-cg"), an upstream Service
			// (base+"-cg-upstream") and a peer Service (base+"-cg-peer") — all in the shared name space.
			names = append(names, base+"-cg", base+"-cg-upstream", base+"-cg-peer")
		}
		for _, name := range names {
			if err := add(name, owner); err != nil {
				return err
			}
		}
	}
	// The legacy singleton .spec.validator materializes a ChainNode "<nodeSet>-validator" outside
	// .spec.nodes; its main Service and always-created -internal variant share the name space.
	if nodeSet.Spec.Validator != nil {
		owner := "legacy validator"
		base := fmt.Sprintf("%s-validator", nodeSet.GetName())
		for _, name := range []string{base, base + "-internal"} {
			if err := add(name, owner); err != nil {
				return err
			}
		}
	}
	// Cosmoseed derives a client Service "<nodeSet>-seed" and a headless Service
	// "<nodeSet>-seed-headless"; both share the operator-wide Service name space.
	if nodeSet.Spec.Cosmoseed.IsEnabled() {
		owner := "cosmoseed"
		base := fmt.Sprintf("%s-seed", nodeSet.GetName())
		for _, name := range []string{base, base + "-headless"} {
			if err := add(name, owner); err != nil {
				return err
			}
		}
	}
	for i := range nodeSet.Spec.Ingresses {
		ing := &nodeSet.Spec.Ingresses[i]
		owner := fmt.Sprintf("global ingress route %q", ing.Name)
		// ensureServices always creates BOTH the public "<nodeSet>-global-<route>" Service and its
		// "-internal" variant regardless of UseInternal, so register the public name directly rather
		// than GetServiceName (which flips to the internal name when UseInternal is set).
		base := ing.GetName(nodeSet)
		for _, name := range []string{base, base + "-internal"} {
			if err := add(name, owner); err != nil {
				return err
			}
		}
	}
	for i := range nodeSet.Spec.GatewayRoutes {
		gw := &nodeSet.Spec.GatewayRoutes[i]
		owner := fmt.Sprintf("global gateway route %q", gw.Name)
		// GatewayConfig.GetName is the Gateway object name ("<nodeSet>-<route>-gw"); the backing Service
		// is "<nodeSet>-global-<route>". ensureServices always creates both it and its -internal variant
		// regardless of UseInternal, so register the public name directly rather than GetServiceName.
		base := fmt.Sprintf("%s-global-%s", nodeSet.GetName(), gw.Name)
		for _, name := range []string{base, base + "-internal"} {
			if err := add(name, owner); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateGroupChildReservedNames rejects a node group whose generated child ChainNode names collide
// with the reserved StatefulSet-child patterns (…-cg-<n> / …-signer-<n>). The controller creates child
// ChainNodes "<base>-<ordinal>"; if <base> ends in "-cg" or "-signer" (e.g. a group named "foo-cg"),
// the child name "<nodeSet>-foo-cg-0" is itself rejected by ValidateReservedStatefulChildName at its
// own admission create, leaving the ChainNodeSet admitted but permanently unable to reconcile its
// children. Surface that up front on the parent with a clear error.
//
// The check runs on create and update (via obj.Validate(nil) on create), but only for groups that
// actually materialize children now — a scaled-to-zero group creates no child, so it is skipped until
// it is scaled up. Groups whose Service name was already active are grandfathered so an unrelated
// update to a ChainNodeSet that predates this validation is never blocked; the grandfathered set is
// built the same way validateCosmosigner builds its legacy whitelist — from the old spec's active
// group Service names on the update path, or from the controller-recorded LegacySignerServiceNames on
// the no-webhook path (old == nil). A group that is newly added, renamed into a reserved shape, or
// scaled up from zero is not in that set and is caught before its children are stranded.
func (nodeSet *ChainNodeSet) validateGroupChildReservedNames(old *ChainNodeSet) error {
	grandfathered := map[string]bool{}
	if old != nil {
		for i := range old.Spec.Nodes {
			g := &old.Spec.Nodes[i]
			if g.GetInstances() > 0 {
				grandfathered[g.GetServiceName(old)] = true
			}
		}
	} else if nodeSet.Status.LegacySignerServiceNamesInitialized {
		for _, name := range nodeSet.Status.LegacySignerServiceNames {
			grandfathered[name] = true
		}
	}
	for i := range nodeSet.Spec.Nodes {
		g := &nodeSet.Spec.Nodes[i]
		base := g.GetServiceName(nodeSet)
		if g.GetInstances() == 0 || grandfathered[base] {
			continue
		}
		child := fmt.Sprintf("%s-0", base)
		if err := ValidateReservedStatefulChildName(child, true); err != nil {
			return fmt.Errorf("node group %q derives child ChainNode names like %q that collide with reserved CosmoGuard/cosmosigner StatefulSet child names: rename the group", g.Name, child)
		}
	}
	return nil
}

// validateCosmosignerUpdate enforces the update-path invariants of every managed signer, pairing the
// old and new resolved signers by name: per-signer replica immutability (raft membership is not
// migrated), the teardown-in-flight replica lock for a re-added signer, and — once the chain is
// established — per-signer retargeting immutability and per-validator signing-key immutability.
func (nodeSet *ChainNodeSet) validateCosmosignerUpdate(old *ChainNodeSet) error {
	if old == nil {
		return nil
	}

	oldSigners := make(map[string]ResolvedSigner)
	for _, s := range old.ResolveCosmosigners() {
		oldSigners[s.Name] = s
	}
	oldGroups := make(map[string]NodeGroupSpec, len(old.Spec.Nodes))
	for _, group := range old.Spec.Nodes {
		oldGroups[group.Name] = group
	}

	for _, ns := range nodeSet.ResolveCosmosigners() {
		path := nodeSet.signerFieldPath(ns)
		if os, ok := oldSigners[ns.Name]; ok {
			// Present in both revisions: the replica count is immutable — the membership recorded in the
			// existing per-pod raft state is not updated by rendering a new bootstrap list.
			if os.Spec.GetReplicas() != ns.Spec.GetReplicas() {
				return fmt.Errorf("%s.replicas is immutable after creation: changing it does not migrate the raft membership in the signer's state and can break quorum", path)
			}
			// The raft-state PVC template is immutable too: StatefulSet volumeClaimTemplates cannot be
			// updated, so an accepted change would be silently ignored by the reconciler.
			if err := validateCosmosignerStateStorageImmutable(path, os.Spec, ns.Spec); err != nil {
				return err
			}
			if old.Status.ChainID != "" && os.Digest() != ns.Digest() {
				st := old.GetCosmosignerStatus(os.Name)
				if st == nil || st.AppliedDigest == "" || st.PublicKey == "" {
					return fmt.Errorf("%s cannot be migrated until the controller records its applied public key; restore the previous configuration and wait for one reconcile", path)
				}
			}
		} else if st := old.GetCosmosignerStatus(ns.Name); st != nil {
			// Re-added while a previous incarnation's teardown is still in flight: its raft PVCs may
			// still exist, so the replica count AND the PVC template must match until teardown clears
			// the recorded values — surviving claims would be re-bound with the old membership and at
			// their old size/class.
			if st.Replicas != nil && *st.Replicas != ns.Spec.GetReplicas() {
				return fmt.Errorf("%s.replicas must stay %d until the previous signer's teardown completes: its raft state PVCs may still exist and their membership does not match", path, *st.Replicas)
			}
			if st.StateStorageSize != "" &&
				!CosmosignerStateStorageEqual(st.StateStorageSize, st.StateStorageClassName, ns.Spec.GetStateStorageSize(), ns.Spec.StorageClassName) {
				return fmt.Errorf("%s.stateStorageSize/.storageClassName must stay unchanged until the previous signer's teardown completes: its raft state PVCs may still exist at the old size/class", path)
			}
		}
	}

	if old.Status.ChainID == "" {
		return nil
	}

	desiredSigners := nodeSet.ResolveCosmosigners()
	for _, os := range old.ResolveCosmosigners() {
		st := old.GetCosmosignerStatus(os.Name)
		if st == nil || st.AtEstablishment == nil || *st.AtEstablishment == "" ||
			old.GenesisSentryEstablishmentIdentity(os) != *st.AtEstablishment {
			continue
		}
		preserved := false
		for _, ns := range desiredSigners {
			if nodeSet.GenesisSentryEstablishmentIdentity(ns) == *st.AtEstablishment {
				preserved = true
				break
			}
		}
		if !preserved {
			return fmt.Errorf("cosmosigner %q protects an immutable genesis validator key and cannot be removed or changed unless an equivalent signer keeps serving the same key", os.Name)
		}
	}

	desiredNames := make(map[string]struct{}, len(desiredSigners))
	for _, s := range desiredSigners {
		desiredNames[s.Name] = struct{}{}
	}
	addedSigner := false
	for _, s := range desiredSigners {
		if _, existed := oldSigners[s.Name]; !existed {
			addedSigner = true
		}
	}
	for _, s := range old.ResolveCosmosigners() {
		if _, kept := desiredNames[s.Name]; kept {
			continue
		}
		st := old.GetCosmosignerStatus(s.Name)
		replacement, replaced := nodeSet.DesiredReplacementSigner(desiredSigners, st)
		if st != nil && (st.ServingIdentity != "" || st.SigningDigest != "") && !replaced {
			return fmt.Errorf("cosmosigner %q cannot be removed without a replacement managed signer for validator group %q: no supported handoff can transfer its slash-protection state to an independent local/tmKMS path", s.Name, st.ServingGroup)
		}
		if addedSigner {
			if st == nil || st.AppliedDigest == "" || st.PublicKey == "" {
				return fmt.Errorf("cosmosigner %q cannot be moved to a new manifest placement until the controller records its applied public key; restore the previous configuration and wait for one reconcile", s.Name)
			}
			if replaced {
				path := nodeSet.signerFieldPath(replacement)
				if st.Replicas != nil && *st.Replicas != replacement.Spec.GetReplicas() {
					return fmt.Errorf("%s.replicas must stay %d during a manifest placement move: changing it does not migrate the existing raft membership", path, *st.Replicas)
				}
				if st.StateStorageSize != "" &&
					!CosmosignerStateStorageEqual(st.StateStorageSize, st.StateStorageClassName, replacement.Spec.GetStateStorageSize(), replacement.Spec.StorageClassName) {
					return fmt.Errorf("%s.stateStorageSize/.storageClassName must stay unchanged during a manifest placement move: the replacement reuses the existing raft-state PVCs", path)
				}
			}
		}
	}

	// A multi-instance validator group changes meaning when signer-targeted: without a signer every
	// instance is an independent validator, while with one signer the instances are redundant
	// endpoints for a single identity. That classification cannot change after the chain exists.
	for i, group := range nodeSet.Spec.Nodes {
		if group.Validator == nil {
			continue
		}
		og, ok := oldGroups[group.Name]
		if !ok || og.Validator == nil || (og.GetInstances() <= 1 && group.GetInstances() <= 1) {
			continue
		}
		oldTargeted := old.groupCosmosigner(group.Name) != nil
		newTargeted := nodeSet.groupCosmosigner(group.Name) != nil
		if oldTargeted == newTargeted {
			continue
		}
		return fmt.Errorf(".spec.nodes[%d]: a cosmosigner cannot be added to or removed from established multi-instance validator group %q: it would change the group between multiple on-chain validators and one signing identity", i, group.Name)
	}

	return nil
}

// genesisValidatorPrivKeySecrets returns the set of priv-key secret names preserved on-chain via
// init.genesisValidators across the legacy singleton and every ACTIVE (non-zero-instance) validator
// group. Those keys are part of the immutable genesis validator set; a zero-instance group runs no
// validators and contributes nothing to genesis, so its entries are excluded.
func (nodeSet *ChainNodeSet) genesisValidatorPrivKeySecrets() map[string]struct{} {
	out := map[string]struct{}{}
	add := func(init *GenesisInitConfig) {
		if init == nil {
			return
		}
		for _, gv := range init.GenesisValidators {
			out[gv.PrivKeySecret] = struct{}{}
		}
	}
	if nodeSet.Spec.Validator != nil {
		add(nodeSet.Spec.Validator.Init)
	}
	for i := range nodeSet.Spec.Nodes {
		g := &nodeSet.Spec.Nodes[i]
		if g.Validator != nil && g.GetInstances() > 0 {
			add(g.Validator.Init)
		}
	}
	return out
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
	// vaultKeys tracks Vault Transit keys in a backend-agnostic form so the same key referenced
	// through tmKMS and through cosmosigner is detected as a collision.
	vaultKeys := map[string]string{}
	// genesisValidatorSecrets are priv-key secrets registered in genesis via init.genesisValidators. A
	// sentry-mode software signer may legitimately share such a secret (that entry is how its key gets
	// on-chain), so this set is used to allow that specific overlap.
	genesisValidatorSecrets := map[string]struct{}{}

	registerSecret := func(path, secret string) error {
		if prev, ok := privateKeySecrets[secret]; ok {
			return fmt.Errorf("%s.privateKeySecret %q is already used by %s; each validator must sign with a distinct key", path, secret, prev)
		}
		privateKeySecrets[secret] = path
		return nil
	}

	registerVault := func(path, id string) error {
		if prev, ok := vaultKeys[id]; ok {
			return fmt.Errorf("%s references the same Vault signing key as %s; each validator must sign with a distinct key", path, prev)
		}
		vaultKeys[id] = path
		return nil
	}

	// registerGcp registers a GCP KMS key version. Its identity namespace ("gcpkms\x00...") never
	// collides with Vault/local/tmKMS identities, so sharing vaultKeys only ever detects a
	// GCP-vs-GCP collision between two signers referencing the same key version.
	registerGcp := func(path, keyVersion string) error {
		id := "gcpkms\x00" + keyVersion
		if prev, ok := vaultKeys[id]; ok {
			return fmt.Errorf("%s references the same GCP KMS signing key as %s; each validator must sign with a distinct key", path, prev)
		}
		vaultKeys[id] = path
		return nil
	}

	registerTmKMS := func(path string, v *NodeSetValidatorConfig) error {
		if id, ok := tmKMSSigningKeyIdentity(v.TmKMS); ok {
			if prev, ok := tmKMSKeys[id]; ok {
				return fmt.Errorf("%s.tmKMS references the same signing key as %s; each validator must sign with a distinct key", path, prev)
			}
			tmKMSKeys[id] = path
		}
		if id, ok := tmkmsNormalizedVaultKey(v.TmKMS); ok {
			if err := registerVault(path+".tmKMS", id); err != nil {
				return err
			}
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
			genesisValidatorSecrets[gv.PrivKeySecret] = struct{}{}
		}
		return nil
	}

	// cosmosignerLeavesLocalKeyUnused reports whether the group's validator is targeted by a
	// pre-provisioned external cosmosigner backend (Vault without uploadGenerated, or GCP) and its
	// local secret provably never held the live consensus key. Before establishment there is no live
	// key yet; afterward, only the write-once at-establishment signer record proves the external
	// backend served from the start. A signer added later must keep the former local secret reserved.
	// Software, vault.uploadGenerated, Init, and CreateValidator targets always consume the local key.
	cosmosignerLeavesLocalKeyUnused := func(group string, v *NodeSetValidatorConfig) bool {
		c := nodeSet.groupCosmosigner(group)
		if c == nil {
			return false
		}
		if v != nil && (v.Init != nil || v.CreateValidator != nil) {
			return false
		}
		if c.UsesSoftwareBackend() {
			return false
		}
		if c.UsesVaultBackend() && c.Backend.Vault.UploadGenerated {
			return false
		}
		if nodeSet.Status.ChainID == "" {
			return true
		}
		for _, signer := range nodeSet.ResolveCosmosigners() {
			if signer.ValidatorGroup != group {
				continue
			}
			matchesHistory := func(status *CosmosignerStatus) bool {
				return status != nil && status.ServingGroup == group && status.AtEstablishment != nil &&
					*status.AtEstablishment == signer.ValidatorTargetedIdentity()
			}
			if current := nodeSet.GetCosmosignerStatus(signer.Name); current != nil {
				return matchesHistory(current) && current.LocalKeyEverServed != nil && !*current.LocalKeyEverServed
			}
			foundHistory := false
			for i := range nodeSet.Status.Cosmosigners {
				status := &nodeSet.Status.Cosmosigners[i]
				if !matchesHistory(status) {
					continue
				}
				foundHistory = true
				if status.LocalKeyEverServed == nil || *status.LocalKeyEverServed {
					return false
				}
			}
			return foundHistory
		}
		return false
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
		// An explicit privateKeySecret on a pure TmKMS validator is unused and must not be reserved. The
		// same applies when a pre-provisioned external cosmosigner is the signer.
		if (v.TmKMS == nil || v.Init != nil || tmkmsUploadsGeneratedPrivKey(v)) &&
			!cosmosignerLeavesLocalKeyUnused(ReservedValidatorGroupName, v) {
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
		// privateKeySecret names one key (single-instance group, or the single identity of a
		// cosmosigner-targeted group); otherwise every instance resolves to its own default — except in a
		// cosmosigner-targeted group, which holds one identity (instance 0's key) and has no per-instance
		// keys to reserve.
		usesLocalKey := (group.Validator.TmKMS == nil || group.Validator.Init != nil || tmkmsUploadsGeneratedPrivKey(group.Validator)) &&
			!cosmosignerLeavesLocalKeyUnused(group.Name, group.Validator)
		if !usesLocalKey {
			// Nothing to reserve.
		} else if group.Validator.PrivateKeySecret != nil {
			if err := registerSecret(path, *group.Validator.PrivateKeySecret); err != nil {
				return err
			}
		} else {
			keyInstances := group.GetInstances()
			if nodeSet.groupCosmosigner(group.Name) != nil {
				keyInstances = 1
			}
			for idx := 0; idx < keyInstances; idx++ {
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

	// Every managed cosmosigner signs with a single consensus identity. When a signer targets a
	// validator, that validator's own key is already registered above (the signer reuses it), so only
	// the external backend identity and the sentry-mode software secret need registering here to catch
	// collisions with other validators — and with each other, now that a ChainNodeSet can run several
	// signers.
	//
	// sentrySoftwareSecrets tracks the software key of every sentry signer so two sentry signers can
	// never share one, even when that key is a genesis-validator secret (which the general
	// uniqueness map skips — see below).
	sentrySoftwareSecrets := map[string]string{}
	for _, s := range nodeSet.ResolveCosmosigners() {
		c := s.Spec
		path := nodeSet.signerFieldPath(s)
		switch {
		case c.UsesVaultBackend():
			v := c.Backend.Vault
			if v.Address != "" && v.KeyName != "" {
				ns := ""
				if v.Namespace != nil {
					ns = *v.Namespace
				}
				if err := registerVault(path, normalizedVaultIdentity(v.Address, ns, v.GetVaultMount(), v.KeyName, v.GetKeyVersion())); err != nil {
					return err
				}
			}
		case c.UsesGcpKmsBackend():
			if kv := c.Backend.GcpKMS.KeyVersion; kv != "" {
				if err := registerGcp(path, kv); err != nil {
					return err
				}
			}
		case c.UsesSoftwareBackend():
			// A validator-targeted software signer reuses that validator's already-registered key, so
			// nothing extra is registered.
			if s.TargetsValidator() || c.Backend.Software.PrivateKeySecret == nil {
				break
			}
			secret := *c.Backend.Software.PrivateKeySecret
			// Two sentry signers holding the same key would sign the same identity from two independent
			// raft clusters (double-signing), so reject that regardless of any genesis overlap.
			if prev, ok := sentrySoftwareSecrets[secret]; ok {
				return fmt.Errorf("%s.backend.software.privateKeySecret %q is already used by %s; each signer must sign with a distinct key", path, secret, prev)
			}
			sentrySoftwareSecrets[secret] = path
			// A sentry-mode software key must still be unique versus other live validators — except when
			// it is the priv-key secret of a genesis validator entry, which is the documented way to
			// register the sentry signer's key on-chain (that single overlap is allowed; a second sentry
			// on the same secret was already rejected above).
			if _, sharedWithGenesis := genesisValidatorSecrets[secret]; !sharedWithGenesis {
				if err := registerSecret(path+".backend.software", secret); err != nil {
					return err
				}
			}
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
		// A cosmosigner-targeted group has one identity (instance 0's account); no per-instance
		// accounts exist for the redundant signing endpoints.
		if nodeSet.groupCosmosigner(group.Name) == nil {
			for idx := 1; idx < group.GetInstances(); idx++ {
				accountSecrets[fmt.Sprintf("%s-%s-%d-account", nodeSet.GetName(), group.Name, idx)] = path
			}
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
		// An explicit accountMnemonicSecret names one account (single-instance group, or the single
		// create-validator flow of a cosmosigner-targeted group). Otherwise every instance resolves
		// to its own default <nodeset>-<group>-<index>-account — except in a cosmosigner-targeted
		// group, where only instance 0 runs create-validator.
		if group.Validator.CreateValidator.AccountMnemonicSecret != nil {
			if err := register(path, *group.Validator.CreateValidator.AccountMnemonicSecret); err != nil {
				return err
			}
		} else {
			accountInstances := group.GetInstances()
			if nodeSet.groupCosmosigner(group.Name) != nil {
				accountInstances = 1
			}
			for idx := 0; idx < accountInstances; idx++ {
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
		defaultAccount := fmt.Sprintf("%s-%s-0-account", nodeSet.GetName(), group.Name)
		if err := registerInit(path, defaultAccount, group.Validator.Init); err != nil {
			return err
		}
		// Instances 1..n-1 are recorded as generated genesis validators with deterministic accounts
		// <nodeset>-<group>-<index>-account (see groupGenesisValidators) — unless a cosmosigner
		// targets the group, in which case there is only one genesis validator (instance 0) and no
		// per-instance accounts.
		if nodeSet.groupCosmosigner(group.Name) == nil {
			for idx := 1; idx < group.GetInstances(); idx++ {
				secret := fmt.Sprintf("%s-%s-%d-account", nodeSet.GetName(), group.Name, idx)
				if err := register(fmt.Sprintf("%s (instance %d)", path, idx), secret); err != nil {
					return err
				}
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
	tmKMSID, _ := tmKMSSigningKeyIdentity(tmKMS)
	return strings.Join([]string{secret, tmKMSID, genesisConfigurationFingerprint(init, info, accountPrefix, valPrefix, accountHDPath)}, "\x00")
}

func genesisConfigurationFingerprint(init *GenesisInitConfig, info *ValidatorInfo, accountPrefix, valPrefix, accountHDPath string) string {
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
	return strings.Join([]string{accountPrefix, valPrefix, accountHDPath, string(infoJSON), string(initJSON)}, "\x00")
}

// GenesisSigningDigestAllowsRefresh proves that a recorded raw genesis fingerprint differs from the
// current config only because an admitted signing-path migration changed signing material.
func (nodeSet *ChainNodeSet) GenesisSigningDigestAllowsRefresh(group string, cfg *NodeSetValidatorConfig, recorded string) bool {
	if cfg == nil || recorded == "" || recorded == GenesisDigestExternal {
		return false
	}
	managed := nodeSet.groupCosmosigner(group) != nil
	if !managed {
		for i := range nodeSet.Status.Cosmosigners {
			st := &nodeSet.Status.Cosmosigners[i]
			if st.ServingGroup == group && st.AppliedDigest != "" && st.PublicKey != "" {
				managed = true
				break
			}
		}
	}
	if !managed {
		return false
	}
	configuration := genesisConfigurationFingerprint(cfg.Init, cfg.Info, cfg.GetAccountPrefix(), cfg.GetValPrefix(), cfg.GetAccountHDPath())
	return strings.HasSuffix(recorded, "\x00"+configuration)
}

// normalizedVaultIdentity identifies one pinned Vault Transit key version across managed backends.
// The namespace and null-byte separators prevent distinct Vault keys from being conflated.
func normalizedVaultIdentity(address, namespace, mount, key string, version int) string {
	return fmt.Sprintf("vault\x00%s\x00%s\x00%s\x00%s\x00%d", address, namespace, mount, key, version)
}

// tmkmsNormalizedVaultKey returns the backend-agnostic Vault identity a tmKMS config points at, and
// whether one is configured.
func tmkmsNormalizedVaultKey(t *TmKMS) (string, bool) {
	if t == nil || t.Provider.Hashicorp == nil {
		return "", false
	}
	h := t.Provider.Hashicorp
	if h.Address == "" || h.Key == "" {
		return "", false
	}
	return normalizedVaultIdentity(h.Address, "", DefaultCosmosignerVaultMount, h.Key, 1), true
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
