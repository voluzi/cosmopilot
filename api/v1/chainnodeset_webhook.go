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
	if err := ValidateCosmosignerReservedName(obj.GetName(), true); err != nil {
		return nil, err
	}
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
	if err := nodeSet.validateCosmosigner(old); err != nil {
		return nil, err
	}
	if old != nil {
		if err := validateCosmosignerReplicasImmutable(old.Spec.Cosmosigner, nodeSet.Spec.Cosmosigner); err != nil {
			return nil, err
		}
		// Once the chain is established, the targeted validator's consensus pubkey is fixed on-chain.
		// Reject changes to its effective signing key — including adding, removing or switching the
		// cosmosigner backend — while allowing same-key migrations (equivalent keys compare equal).
		if old.Status.ChainID != "" && (old.Spec.Cosmosigner != nil || nodeSet.Spec.Cosmosigner != nil) {
			// Retargeting the signer to different groups after establishment would leave the previously
			// targeted validator signing locally (or concurrently) with a different key, even if the
			// signer's own key is unchanged. The target set is therefore immutable — but only when a
			// validator is (or was) targeted: a sentry-only signer over regular groups protects no
			// in-cluster validator identity, so moving it between fullnode groups stays allowed.
			oldTargetsValidator := old.cosmosignerTargetSigningIdentity(old.CosmosignerTargetGroups()) != ""
			newTargetsValidator := nodeSet.cosmosignerTargetSigningIdentity(nodeSet.CosmosignerTargetGroups()) != ""
			if old.Spec.Cosmosigner != nil && nodeSet.Spec.Cosmosigner != nil &&
				(oldTargetsValidator || newTargetsValidator) &&
				!equalStringSet(old.CosmosignerTargetGroups(), nodeSet.CosmosignerTargetGroups()) {
				return nil, fmt.Errorf(".spec.cosmosigner.nodeGroups is immutable after the chain is established: retargeting the signer would change which validator signs")
			}
			// The identity check only applies when the signer serves a validator whose key is
			// registered on-chain by this controller. A sentry-mode signer over regular groups has
			// its key registered out-of-band (e.g. init.genesisValidators), so adding or changing it
			// resolves no in-cluster validator identity — there is nothing on the old side to
			// compare, and rejecting would block the documented add-sentry-signer-later flow. When
			// the old side DID resolve a validator identity, an update that empties it (dropping the
			// signer and the validator's own signing path together) is rejected too: the on-chain
			// validator would be left with no signing path.
			targets := nodeSet.CosmosignerTargetGroups()
			if len(targets) == 0 {
				targets = old.CosmosignerTargetGroups()
			}
			oldIdentity := old.cosmosignerTargetSigningIdentity(targets)
			newIdentity := nodeSet.cosmosignerTargetSigningIdentity(targets)
			if oldIdentity != "" && (newIdentity == "" || oldIdentity != newIdentity) {
				return nil, fmt.Errorf("the consensus signing key of the cosmosigner-targeted validator is immutable after the chain is established: changing or removing the cosmosigner/signing configuration would leave it signing with a key not in the on-chain validator set (or not signing at all)")
			}
		}
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
			// When cosmosigner is involved on either side, compare through the effective signing
			// identity (same-key signer migrations compare equal); otherwise keep the raw fingerprint.
			changed := genesisSigningMaterialChanged(old.Spec.Validator, nodeSet.Spec.Validator, defaultPrivKeySecret)
			if old.Spec.Cosmosigner != nil || nodeSet.Spec.Cosmosigner != nil {
				changed = old.nodeSetEffectiveGenesisFingerprint(ReservedValidatorGroupName, old.Spec.Validator) !=
					nodeSet.nodeSetEffectiveGenesisFingerprint(ReservedValidatorGroupName, nodeSet.Spec.Validator)
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
			changed := genesisSigningMaterialChanged(og.Validator, group.Validator, defaultPrivKeySecret)
			if old.Spec.Cosmosigner != nil || nodeSet.Spec.Cosmosigner != nil {
				// Identity-normalized comparison so a same-key signer migration passes.
				changed = old.nodeSetEffectiveGenesisFingerprint(group.Name, og.Validator) !=
					nodeSet.nodeSetEffectiveGenesisFingerprint(group.Name, group.Validator)
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
//
// old is the previous revision on the update path (nil on create); it enables the same-key
// migration waiver mirroring the ChainNode webhook.
func (nodeSet *ChainNodeSet) validateCosmosigner(old *ChainNodeSet) error {
	c := nodeSet.Spec.Cosmosigner
	if c == nil {
		return nil
	}

	if err := c.Validate(".spec.cosmosigner", true); err != nil {
		return err
	}

	// The signer owns resources named "<nodeset>-signer" and "<nodeset>-signer-privval"; a node
	// group with either name would produce a colliding Service.
	for i, g := range nodeSet.Spec.Nodes {
		if g.Name == "signer" || g.Name == "signer-privval" {
			return fmt.Errorf(".spec.nodes[%d].name %q is reserved when .spec.cosmosigner is configured", i, g.Name)
		}
	}

	// Index groups by name for target resolution.
	groups := make(map[string]NodeGroupSpec, len(nodeSet.Spec.Nodes))
	for _, g := range nodeSet.Spec.Nodes {
		groups[g.Name] = g
	}

	validatorTargets := 0
	var targetValidator *NodeSetValidatorConfig

	if len(c.NodeGroups) == 0 {
		// Default target is the legacy singleton validator.
		if nodeSet.Spec.Validator == nil {
			return fmt.Errorf(".spec.cosmosigner.nodeGroups is required when .spec.validator is not set")
		}
		if nodeSet.Spec.Validator.TmKMS != nil {
			return fmt.Errorf(".spec.cosmosigner and .spec.validator.tmKMS are mutually exclusive")
		}
		validatorTargets = 1
		targetValidator = nodeSet.Spec.Validator
	} else {
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
			// A group scaled to zero has no pods, so it can neither be a signing endpoint nor host a
			// validator whose key the signer would use.
			if group.GetInstances() == 0 {
				return fmt.Errorf(".spec.cosmosigner cannot target group %q with zero instances", name)
			}
			if group.Validator != nil {
				if group.GetInstances() > 1 {
					return fmt.Errorf(".spec.cosmosigner cannot target validator group %q with multiple instances: each instance is a distinct validator with its own key", name)
				}
				if group.Validator.TmKMS != nil {
					return fmt.Errorf(".spec.cosmosigner cannot target group %q which uses tmKMS: cosmosigner and tmKMS are mutually exclusive", name)
				}
				validatorTargets++
				targetValidator = group.Validator
			}
		}
	}

	// A single signer holds one consensus identity. Targeting more than one validator would make
	// distinct validators share the same signing key.
	if validatorTargets > 1 {
		return fmt.Errorf(".spec.cosmosigner cannot target more than one validator: a signer holds a single consensus identity")
	}

	if validatorTargets == 0 {
		// Without a validator target there is no controller-registered consensus key to reuse, so the
		// signer's key material must be supplied explicitly.
		if c.UsesSoftwareBackend() && (c.Backend.Software.PrivateKeySecret == nil || *c.Backend.Software.PrivateKeySecret == "") {
			return fmt.Errorf(".spec.cosmosigner.backend.software.privateKeySecret is required when no validator is targeted")
		}
		if c.UsesVaultBackend() && c.Backend.Vault.UploadGenerated {
			return fmt.Errorf(".spec.cosmosigner.backend.vault.uploadGenerated requires targeting a validator whose generated key can be imported")
		}
		return nil
	}

	// With a validator target the signer must use that validator's own key, so an explicit software
	// secret (which could point elsewhere) is not allowed.
	if c.UsesSoftwareBackend() && c.Backend.Software.PrivateKeySecret != nil && *c.Backend.Software.PrivateKeySecret != "" {
		return fmt.Errorf(".spec.cosmosigner.backend.software.privateKeySecret cannot be set when targeting a validator: the validator's own key is used")
	}

	// When the targeted validator registers a freshly-generated consensus key on-chain (genesis
	// init or create-validator), Cosmopilot registers the validator's local key. The signer must
	// therefore use that same key: only the software backend (which references it) or Vault with
	// uploadGenerated (which imports it — implicitly auto-defaulted for genesis-init targets, per
	// the documented tmKMS-parity behavior) match. A pre-provisioned Vault/GCP key would register a
	// different pubkey than the signer holds, until external pubkey registration is wired. Waived
	// for a same-key migration on an established chain (mirrors the ChainNode webhook): the previous
	// signing path already put this exact key on-chain, e.g. tmKMS→cosmosigner on the same Vault
	// key. On the no-webhook path (old == nil) the waiver requires the status-recorded signing
	// digest to MATCH the current spec: the digest is only recorded after this exact signer
	// identity was rolled out and serving, so a matching digest proves the pre-provisioned key is
	// the one in effect — while a newly added signer (no digest, or a different identity) stays
	// subject to the rule. (A first-time no-webhook migration can set uploadGenerated=true: the
	// import verifies the stored pubkey matches the source key, so it is a safe no-op on same-key.)
	registers := targetValidator.Init != nil || targetValidator.CreateValidator != nil
	sameKeyWaiver := nodeSetSameKeyMigration(old, nodeSet) ||
		(old == nil && nodeSet.Status.CosmosignerSigningDigest != "" &&
			nodeSet.CosmosignerSigningDigest() == nodeSet.Status.CosmosignerSigningDigest)
	if registers && !sameKeyWaiver {
		matches := c.UsesSoftwareBackend() || c.VaultUploadsGenerated(targetValidator.Init != nil)
		if !matches {
			return fmt.Errorf(".spec.cosmosigner targeting a validator that initializes genesis or uses createValidator requires the software backend or vault.uploadGenerated so the registered consensus key matches the signer")
		}
	}

	hasExplicitKey := targetValidator.PrivateKeySecret != nil && *targetValidator.PrivateKeySecret != ""

	// uploadGenerated imports the targeted validator's key into Vault, so that key must exist: the
	// validator must generate one (init/createValidator) or supply an explicit privateKeySecret. A
	// plain external-genesis validator with only the default key never creates it, leaving nothing
	// to import.
	if c.UsesVaultBackend() && c.Backend.Vault.UploadGenerated {
		if !registers && !hasExplicitKey {
			return fmt.Errorf(".spec.cosmosigner.backend.vault.uploadGenerated requires the targeted validator to initialize genesis, use createValidator, or set an explicit privateKeySecret to import")
		}
	}

	// The software backend mounts the targeted validator's key secret. A plain external-genesis
	// validator never creates its default key, so an explicit privateKeySecret is required.
	if c.UsesSoftwareBackend() && !registers && !hasExplicitKey {
		return fmt.Errorf(".spec.cosmosigner software backend targeting a validator that consumes an external genesis requires the validator to set privateKeySecret: its consensus key is not generated")
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
	// pre-provisioned external cosmosigner backend (Vault without uploadGenerated, or GCP) AND its
	// local secret provably never held the live consensus key. Only then may the secret be reused
	// by another validator. Software and vault.uploadGenerated backends consume the local key, so
	// it stays reserved for them — and so does any Init/CreateValidator target: its generated
	// secret registered (or was imported as) the on-chain consensus key, e.g. after a same-key
	// tmKMS→cosmosigner migration where the previously-uploaded key still lives in that secret;
	// freeing it would let a second validator sign with the same identity.
	cosmosignerLeavesLocalKeyUnused := func(group string, v *NodeSetValidatorConfig) bool {
		c := nodeSet.Spec.Cosmosigner
		if c == nil || !nodeSet.IsCosmosignerTargetGroup(group) {
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
		return true
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
		// privateKeySecret is only valid on a single-instance group (multi-instance groups with
		// privateKeySecret are rejected earlier); otherwise every instance resolves to its own default.
		usesLocalKey := (group.Validator.TmKMS == nil || group.Validator.Init != nil || tmkmsUploadsGeneratedPrivKey(group.Validator)) &&
			!cosmosignerLeavesLocalKeyUnused(group.Name, group.Validator)
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

	// The managed cosmosigner deployment signs with a single consensus identity. When it targets a
	// validator, that validator's own key is already registered above (the signer reuses it), so
	// only the external backend identity and the sentry-mode software secret need registering here
	// to catch collisions with other validators.
	if c := nodeSet.Spec.Cosmosigner; c != nil {
		switch {
		case c.UsesVaultBackend():
			v := c.Backend.Vault
			if v.Address != "" && v.KeyName != "" {
				ns := ""
				if v.Namespace != nil {
					ns = *v.Namespace
				}
				if err := registerVault(".spec.cosmosigner", normalizedVaultIdentity(v.Address, ns, v.GetVaultMount(), v.KeyName)); err != nil {
					return err
				}
			}
		// A GCP KMS key is not registered: a ChainNodeSet has at most one cosmosigner and no other
		// signing path (tmKMS, local key) can reference a GCP key, so there is no possible in-set
		// collision to detect.
		case c.UsesSoftwareBackend():
			// A validator-targeted software signer reuses that validator's already-registered key, so
			// nothing extra is registered. A sentry-mode software key must still be unique versus other
			// live validators — except when it is the priv-key secret of a genesis validator entry, which
			// is the documented way to register the sentry signer's key on-chain (that overlap is allowed).
			if _, hasValidatorTarget := nodeSet.CosmosignerValidatorTargetSecret(); !hasValidatorTarget &&
				c.Backend.Software.PrivateKeySecret != nil {
				secret := *c.Backend.Software.PrivateKeySecret
				if _, sharedWithGenesis := genesisValidatorSecrets[secret]; !sharedWithGenesis {
					if err := registerSecret(".spec.cosmosigner.backend.software", secret); err != nil {
						return err
					}
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

// genesisSigningFingerprintWithIdentity is like genesisSigningFingerprint but takes a precomputed
// effective signing identity in place of the raw private-key-secret + tmKMS material, so equivalent
// keys (e.g. the same Vault key via tmKMS or cosmosigner) yield the same fingerprint. The init,
// account-derivation and info fields are compared identically, so genuine genesis changes are still
// detected.
func genesisSigningFingerprintWithIdentity(signingIdentity string, init *GenesisInitConfig, info *ValidatorInfo, accountPrefix, valPrefix, accountHDPath string) string {
	var initJSON []byte
	if init != nil {
		initCopy := init.DeepCopy()
		initCopy.AccountPrefix = nil
		initCopy.ValPrefix = nil
		initCopy.AccountHDPath = nil
		initJSON, _ = json.Marshal(initCopy)
	}
	var infoJSON []byte
	if info != nil {
		infoJSON, _ = json.Marshal(info)
	}
	return strings.Join([]string{signingIdentity, accountPrefix, valPrefix, accountHDPath, string(infoJSON), string(initJSON)}, "\x00")
}

// nodeSetValidatorEffectiveIdentity returns the normalized signing identity of a nodeset validator
// (legacy singleton via ReservedValidatorGroupName, or a validator group): the cosmosigner identity
// when that group is the signer target on this revision, otherwise its own local/tmKMS identity.
func (nodeSet *ChainNodeSet) nodeSetValidatorEffectiveIdentity(group string, cfg *NodeSetValidatorConfig) string {
	if nodeSet.Spec.Cosmosigner != nil && nodeSet.IsCosmosignerTargetGroup(group) {
		return nodeSet.CosmosignerSigningIdentity()
	}
	return nodeSet.validatorGroupSigningIdentity(group, cfg)
}

// nodeSetEffectiveGenesisFingerprint mirrors the ChainNode effectiveGenesisFingerprint for a nodeset
// genesis-initializing validator: the genesis identity compared through the normalized signing
// identity, so a same-key signer migration (e.g. tmKMS→cosmosigner on the same Vault key) is not
// misread as a genesis change while genuine init/account changes still are.
func (nodeSet *ChainNodeSet) nodeSetEffectiveGenesisFingerprint(group string, cfg *NodeSetValidatorConfig) string {
	if cfg == nil {
		return genesisSigningFingerprintWithIdentity("", nil, nil, "", "", "")
	}
	return genesisSigningFingerprintWithIdentity(
		nodeSet.nodeSetValidatorEffectiveIdentity(group, cfg),
		cfg.Init, cfg.Info, cfg.GetAccountPrefix(), cfg.GetValPrefix(), cfg.GetAccountHDPath())
}

// nodeSetSameKeyMigration reports whether an update keeps the effective consensus signing key of the
// cosmosigner-targeted validator unchanged on an established chain (e.g. tmKMS→cosmosigner on the
// same Vault key). Equivalent keys compare equal via the normalized signing identity.
func nodeSetSameKeyMigration(old, nodeSet *ChainNodeSet) bool {
	if old == nil || old.Status.ChainID == "" {
		return false
	}
	targets := nodeSet.CosmosignerTargetGroups()
	if len(targets) == 0 {
		targets = old.CosmosignerTargetGroups()
	}
	oldIdentity := old.cosmosignerTargetSigningIdentity(targets)
	return oldIdentity != "" && oldIdentity == nodeSet.cosmosignerTargetSigningIdentity(targets)
}

// normalizedVaultIdentity returns a backend-agnostic identifier for a Vault Transit key, so the
// same key referenced through tmKMS and through cosmosigner compares equal. tmKMS uses the default
// "transit" mount and the root namespace implicitly. The Vault namespace is included so keys in
// distinct Vault Enterprise namespaces are not conflated. The null-byte separators keep the fields
// unambiguous.
func normalizedVaultIdentity(address, namespace, mount, key string) string {
	return fmt.Sprintf("vault\x00%s\x00%s\x00%s\x00%s", address, namespace, mount, key)
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
	return normalizedVaultIdentity(h.Address, "", DefaultCosmosignerVaultMount, h.Key), true
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
