package v1

import (
	"context"
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

func SetupChainNodeValidationWebhook(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &ChainNode{}).WithValidator(&ChainNode{}).Complete()
}

var _ admission.Validator[*ChainNode] = &ChainNode{}
var chainNodeLogger = log.Log.WithName("chainnode-webhook")

// GenesisDigestExternal is the sentinel recorded in ChainNodeStatus.GenesisSigningDigest for a node
// that established its chain genesis from an external source (downloaded/ConfigMap) rather than by
// initializing it. It lets the no-webhook reconcile path tell such a node apart from a genesis
// initializer (which records a real signing fingerprint), so adding .validator.init to it after genesis
// can be rejected. It contains no NUL byte, so it never collides with a real fingerprint.
const GenesisDigestExternal = "external"

func (chainNode *ChainNode) ValidateCreate(_ context.Context, obj *ChainNode) (warnings admission.Warnings, err error) {
	chainNodeLogger.V(1).Info("validating resource creation",
		"kind", "ChainNode",
		"resource", obj.GetNamespacedName(),
	)
	if err := ValidateReservedResourceName(obj.GetName(), true); err != nil {
		return nil, err
	}
	if err := ValidateReservedStatefulChildName(obj.GetName(), true); err != nil {
		return nil, err
	}
	return obj.Validate(nil)
}

func (chainNode *ChainNode) ValidateUpdate(_ context.Context, oldObj, newObj *ChainNode) (warnings admission.Warnings, err error) {
	chainNodeLogger.V(1).Info("validating resource update",
		"kind", "ChainNode",
		"resource", newObj.GetNamespacedName(),
	)
	return newObj.Validate(oldObj)
}

func (chainNode *ChainNode) ValidateDelete(_ context.Context, obj *ChainNode) (warnings admission.Warnings, err error) {
	chainNodeLogger.V(1).Info("validating resource deletion (not implemented)",
		"kind", "ChainNode",
		"resource", obj.GetNamespacedName(),
	)
	return nil, nil
}

func (chainNode *ChainNode) tmKMSDeprecationWarnings() admission.Warnings {
	if chainNode.Spec.Validator == nil || chainNode.Spec.Validator.TmKMS == nil {
		return nil
	}
	warnings := admission.Warnings{
		".spec.validator.tmKMS is deprecated and will be removed in a future version; migrate to .spec.cosmosigner",
	}
	if hashicorp := chainNode.Spec.Validator.TmKMS.Provider.Hashicorp; hashicorp != nil && hashicorp.AutoRenewToken {
		warnings = append(warnings, ".spec.validator.tmKMS.provider.hashicorp.autoRenewToken uses the deprecated vault-token-renewer sidecar; migrate to .spec.cosmosigner, which renews Vault tokens internally")
	}
	return warnings
}

func (chainNode *ChainNode) Validate(old *ChainNode) (admission.Warnings, error) {
	// Validate persistence size
	_, err := resource.ParseQuantity(chainNode.GetPersistenceSize())
	if err != nil {
		return nil, fmt.Errorf("bad format for .spec.size: %v", err)
	}

	// Reject a node name whose derived resource names would exceed the 63-character DNS label limit
	// (and then fail every reconcile). Enforced on create and update since Validate runs on both.
	if err := validateDerivedNameLengths(chainNode.GetName(), "ChainNode name", nameFeatures{
		cosmosigner:    chainNode.Spec.Cosmosigner != nil,
		cosmoguardNode: chainNode.Spec.Config.CosmoGuardEnabled(),
	}); err != nil {
		return nil, err
	}

	// Ensure a genesis is specified when .spec.validator.init is not.
	if (chainNode.Spec.Validator == nil || chainNode.Spec.Validator.Init == nil) && chainNode.Spec.Genesis == nil {
		return nil, fmt.Errorf(".spec.genesis is required except when initializing new genesis with .spec.validator.init")
	}

	// Do not accept both genesis and validator init
	if chainNode.Spec.Validator != nil && chainNode.Spec.Validator.Init != nil && chainNode.Spec.Genesis != nil {
		return nil, fmt.Errorf(".spec.genesis and .spec.validator.init are mutually exclusive")
	}

	// Validate snapshots config
	if chainNode.Spec.Persistence != nil && chainNode.Spec.Persistence.Snapshots != nil {
		if err := validateSnapshotsConfig(chainNode.Spec.Persistence.Snapshots, ".spec.persistence.snapshots"); err != nil {
			return nil, err
		}
	}

	// The CosmoGuard dashboard port must not collide with a port the guard Service already exposes.
	if err := chainNode.Spec.Config.ValidateCosmoGuardDashboard(); err != nil {
		return nil, fmt.Errorf(".spec.config.%w", err)
	}

	// Reject duplicate subdomain prefixes across enabled endpoints
	if ing := chainNode.Spec.Ingress; ing != nil {
		if err := ValidateSubdomainPrefixes(".spec.ingress", ing.Subdomains,
			ing.EnableRPC, ing.EnableGRPC, ing.EnableLCD, ing.EnableEvmRPC, ing.EnableEvmRpcWs); err != nil {
			return nil, err
		}
	}
	if gw := chainNode.Spec.Gateway; gw != nil {
		if err := ValidateSubdomainPrefixes(".spec.gateway", gw.Subdomains,
			gw.EnableRPC, gw.EnableGRPC, gw.EnableLCD, gw.EnableEvmRPC, gw.EnableEvmRpcWs); err != nil {
			return nil, err
		}
	}

	if err := chainNode.validateGenesisValidators(); err != nil {
		return nil, err
	}

	// remoteSignerTarget is a controller-managed marker set by the ChainNodeSet controller on nodes
	// of targeted groups. Setting it by hand on a ChainNode that has no cosmosigner of its own and no
	// owning ChainNodeSet would make a validator stop mounting its key and silently fail to sign.
	if chainNode.Spec.RemoteSignerTarget && chainNode.Spec.Cosmosigner == nil && !isControlledByChainNodeSet(chainNode) {
		return nil, fmt.Errorf(".spec.remoteSignerTarget is managed by the ChainNodeSet controller and cannot be set manually")
	}

	// Validate cosmosigner configuration when present.
	if c := chainNode.Spec.Cosmosigner; c != nil {
		if err := c.Validate(".spec.cosmosigner", false); err != nil {
			return nil, err
		}
		// A node cannot both sign through a TmKMS sidecar and a cosmosigner deployment.
		if chainNode.UsesTmKms() {
			return nil, fmt.Errorf(".spec.cosmosigner and .spec.validator.tmKMS are mutually exclusive")
		}

		isValidator := chainNode.Spec.Validator != nil
		// The node generates and registers its own consensus key when it initializes genesis or runs
		// create-validator.
		registers := isValidator && (chainNode.Spec.Validator.Init != nil || chainNode.Spec.Validator.CreateValidator != nil)
		initializesGenesis := isValidator && chainNode.Spec.Validator.Init != nil
		hasValidatorKey := isValidator && chainNode.Spec.Validator.PrivateKeySecret != nil && *chainNode.Spec.Validator.PrivateKeySecret != ""
		if c.UsesVaultBackend() && c.VaultUploadsGenerated(initializesGenesis) && c.Backend.Vault.GetKeyVersion() != 1 {
			return nil, fmt.Errorf(".spec.cosmosigner Vault key imports require keyVersion 1 because Vault creates imported material as the initial key version")
		}

		// The signer must use the node's own key. A validator therefore cannot point the software
		// backend at a different secret, and a non-validator (which has no controller-created key)
		// must supply one explicitly.
		if c.UsesSoftwareBackend() {
			s := c.Backend.Software.PrivateKeySecret
			switch {
			case !isValidator:
				if s == nil || *s == "" {
					return nil, fmt.Errorf(".spec.cosmosigner.backend.software.privateKeySecret is required when the node is not a validator")
				}
			case s != nil && *s != "":
				return nil, fmt.Errorf(".spec.cosmosigner.backend.software.privateKeySecret cannot be set when the node is a validator: the validator's own key is used")
			case !registers && !hasValidatorKey:
				// A plain external-genesis validator never creates its default key, so it must set one.
				return nil, fmt.Errorf(".spec.cosmosigner software backend on a validator that consumes an external genesis requires .spec.validator.privateKeySecret: its consensus key is not generated")
			}
		}

		// uploadGenerated imports the node's own key into Vault: it needs a key to import — the node
		// must be a validator that either generates one (init/create-validator) or supplies an
		// explicit privateKeySecret.
		if c.UsesVaultBackend() && c.Backend.Vault.UploadGenerated {
			switch {
			case !isValidator:
				return nil, fmt.Errorf(".spec.cosmosigner.backend.vault.uploadGenerated requires the node to be a validator whose key can be imported")
			case !registers && !hasValidatorKey:
				return nil, fmt.Errorf(".spec.cosmosigner.backend.vault.uploadGenerated requires the validator to initialize genesis, use createValidator, or set an explicit privateKeySecret to import")
			}
		}

		// A validator that registers a freshly-generated key on-chain must sign with that same key,
		// so the backend must be software (which references it) or Vault uploadGenerated (which
		// imports it — auto-defaulted for genesis-init validators, matching the documented tmKMS
		// parity) — not a pre-provisioned Vault/GCP key with a different pubkey. Waived for a
		// migration only after the controller records the operator address, consensus public key, and
		// staking status returned by the on-chain validator query. Account and local-key setup can
		// populate the address and public key before registration completes. Cosmopilot performs a
		// break-before-make signer transition, while the user remains responsible for the selected key
		// for the on-chain validator. On the no-webhook path
		// (old == nil) the waiver requires the status-recorded signing digest to MATCH the current
		// spec: the digest is only ever recorded after this exact signer identity was rolled out and
		// serving, so a matching digest proves the pre-provisioned key is the in-effect one — while
		// a NEWLY added signer (no digest, or digest from a different identity) stays subject to the
		// rule, since "registration completed" alone says nothing about the new backend's key.
		registrationRecorded := func(candidate *ChainNode) bool {
			return candidate.Status.ValidatorAddress != "" && candidate.Status.PubKey != "" && candidate.Status.ValidatorStatus != ""
		}
		recordedDigest := chainNode.Status.CosmosignerSigningDigest
		migrationWaiver := (old != nil && old.Status.ChainID != "" && old.Spec.Validator != nil && registrationRecorded(old)) ||
			(old == nil && chainNode.Status.ChainID != "" && registrationRecorded(chainNode) &&
				recordedDigest != "" && recordedDigest == chainNode.CosmosignerSigningDigest())
		if registers && !migrationWaiver {
			matches := c.UsesSoftwareBackend() || c.VaultUploadsGenerated(initializesGenesis)
			if !matches {
				return nil, fmt.Errorf(".spec.cosmosigner on a validator that initializes genesis or uses createValidator requires the software backend or vault.uploadGenerated so the registered consensus key matches the signer")
			}
		}
	}

	if old != nil {
		if err := validateCosmosignerReplicasImmutable(old.Spec.Cosmosigner, chainNode.Spec.Cosmosigner); err != nil {
			return nil, err
		}
		// The spec-diff helper above no-ops when the signer was removed in an earlier update
		// (old.Spec.Cosmosigner is nil). While the previous signer's teardown is still in flight the
		// controller has not yet cleared the recorded replica count and PVC template, and its raft PVCs
		// may still exist; a re-add with a different count could bind them with a mismatched membership,
		// and a different storage size/class would be silently ignored on the surviving claims. Enforce
		// the recorded values until teardown completes and clears them.
		if c := chainNode.Spec.Cosmosigner; c != nil {
			if replicas := old.Status.CosmosignerReplicas; replicas != nil && *replicas != c.GetReplicas() {
				return nil, fmt.Errorf(".spec.cosmosigner.replicas must stay %d until the previous signer's teardown completes: its raft state PVCs may still exist and their membership does not match", *replicas)
			}
			if recorded := old.Status.CosmosignerStateStorageSize; recorded != "" &&
				!CosmosignerStateStorageEqual(recorded, old.Status.CosmosignerStateStorageClassName, c.GetStateStorageSize(), c.StorageClassName) {
				return nil, fmt.Errorf(".spec.cosmosigner.stateStorageSize/.storageClassName must stay unchanged until the previous signer's teardown completes: its raft state PVCs may still exist at the old size/class")
			}
		}
		// A signer migration may change backends or keys, but it must not silently remove the validator
		// role itself. The controller handles the signer transition; on-chain key correctness remains
		// the user's responsibility.
		if old.Status.ChainID != "" && (old.Spec.Cosmosigner != nil || chainNode.Spec.Cosmosigner != nil) {
			if old.Spec.Validator != nil && chainNode.Spec.Validator == nil {
				return nil, fmt.Errorf(".spec.validator cannot be removed while migrating cosmosigner on an established validator")
			}
			validatorSignerServed := old.Status.CosmosignerServingIdentity != "" ||
				old.Status.CosmosignerSigningDigest != "" ||
				(old.Status.CosmosignerValidatorTargeted != nil && *old.Status.CosmosignerValidatorTargeted)
			if old.Spec.Cosmosigner != nil && chainNode.Spec.Cosmosigner == nil && validatorSignerServed {
				return nil, fmt.Errorf(".spec.cosmosigner cannot be removed from an established validator without a supported slash-protection state handoff; migrate to another managed signer before removing it")
			}
			if old.Spec.Cosmosigner != nil && chainNode.Spec.Cosmosigner != nil &&
				old.CosmosignerSigningDigest() != chainNode.CosmosignerSigningDigest() &&
				(old.Status.CosmosignerAppliedDigest == "" || old.Status.CosmosignerPublicKey == "") {
				return nil, fmt.Errorf(".spec.cosmosigner cannot be migrated until the controller records its applied public key; restore the previous configuration and wait for one reconcile")
			}
		}
	}

	// No-webhook reconcile path (old is nil): the previous spec is unavailable, so status supplies the
	// applied public key needed for managed migrations. Replica/storage changes and incomplete rollout
	// removals remain rejected.
	if old == nil {
		c := chainNode.Spec.Cosmosigner
		// Removing a signer is safe only while the validator role remains present. The controller stops
		// and verifies the signer pods are gone before publishing the fallback path.
		if c == nil && chainNode.Status.ChainID != "" {
			serving := chainNode.Status.CosmosignerServingIdentity
			if serving != "" && chainNode.Spec.Validator == nil {
				return nil, fmt.Errorf(".spec.cosmosigner cannot be removed together with .spec.validator (webhooks disabled): the on-chain validator would have no signing path")
			}
			if serving == "" && chainNode.Status.CosmosignerSigningDigest == "" &&
				(chainNode.Status.CosmosignerReplicas != nil || chainNode.Status.CosmosignerStateStorageSize != "") {
				switch targeted := chainNode.Status.CosmosignerValidatorTargeted; {
				case targeted == nil:
					return nil, fmt.Errorf(".spec.cosmosigner cannot be removed (webhooks disabled): its pre-rollout target kind was not recorded, so the controller cannot prove whether an on-chain validator would lose its signing path — restore the signer so the controller can record it, or remove it with webhooks enabled")
				case *targeted:
					return nil, fmt.Errorf(".spec.cosmosigner cannot be removed (webhooks disabled): its validator rollout identity has not been recorded yet, so the controller cannot prove the local fallback key is safe — restore the signer until rollout completes, or remove it with webhooks enabled")
				}
			}
			// A digest with no serving identity is a legacy record (pre-field) whose identity can no
			// longer be reconstructed once the spec is gone: unjudgeable, so reject conservatively.
			if serving == "" && chainNode.Status.CosmosignerSigningDigest != "" {
				return nil, fmt.Errorf(".spec.cosmosigner cannot be removed (webhooks disabled): a signer served this chain but its recorded identity predates this version and cannot be verified — restore the signer so the controller can record it, or remove it with webhooks enabled")
			}
		}
		if c != nil && chainNode.Status.ChainID != "" {
			if replicas := chainNode.Status.CosmosignerReplicas; replicas != nil && *replicas != c.GetReplicas() {
				return nil, fmt.Errorf(".spec.cosmosigner.replicas is immutable after the signer is deployed (webhooks disabled): changing it does not migrate the raft membership and can break quorum")
			}
			// The PVC template is immutable too: StatefulSet volumeClaimTemplates cannot be updated and
			// existing claims stay at their old size/class, so a change would be silently ignored.
			if recorded := chainNode.Status.CosmosignerStateStorageSize; recorded != "" &&
				!CosmosignerStateStorageEqual(recorded, chainNode.Status.CosmosignerStateStorageClassName, c.GetStateStorageSize(), c.StorageClassName) {
				return nil, fmt.Errorf(".spec.cosmosigner.stateStorageSize/.storageClassName are immutable after the signer is deployed (webhooks disabled): its raft state PVCs cannot be resized or moved — remove the signer and re-add it")
			}
			if recorded := chainNode.Status.CosmosignerSigningDigest; recorded != "" {
				if chainNode.CosmosignerSigningDigest() != recorded {
					if chainNode.Status.CosmosignerAppliedDigest == "" || chainNode.Status.CosmosignerPublicKey == "" {
						return nil, fmt.Errorf(".spec.cosmosigner cannot be migrated with webhooks disabled until the controller records its applied public key; restore the previous configuration and wait for one reconcile")
					}
				}
				// The digest hashes the backend identity and replicas — not whether the node is still a
				// validator. Dropping .spec.validator keeps a Vault/GCP digest identical while removing
				// the validator the signer was protecting, so additionally require the signer to still
				// resolve the recorded serving identity through a validator target.
				serving := chainNode.Status.CosmosignerServingIdentity
				if serving != "" && !chainNode.IsValidator() {
					return nil, fmt.Errorf(".spec.cosmosigner: the validator the signer was serving can no longer be resolved (webhooks disabled) — removing the validator block would leave its on-chain key without its signing path")
				}
				// Legacy digest (pre-serving-identity) with no current validator target: the served
				// validator was already dropped and its identity can no longer be reconstructed —
				// unverifiable, so reject conservatively. An unchanged legacy spec still resolves a
				// validator target and passes, letting the controller backfill the serving identity.
				if serving == "" && chainNode.CosmosignerValidatorTargetedIdentity() == "" {
					return nil, fmt.Errorf(".spec.cosmosigner: a signer served a validator on this chain but its recorded identity predates this version and the validator block is gone (webhooks disabled) — restore .spec.validator, or repair the configuration with webhooks enabled")
				}
				// A matching digest proves this identity rolled out and served; skip the addition guard.
			} else if marker := chainNode.Status.CosmosignerAtEstablishment; marker != nil {
				// No digest yet (pre-rollout window). A non-empty marker means this signer was responsible
				// for the on-chain validator key at establishment.
				if *marker != "" && !chainNode.IsValidator() {
					// Pre-digest DEMOTION: .spec.validator was dropped while the pre-provisioned Vault/GCP
					// signer is kept, so IsValidator() is now false — the digest/serving guards above never
					// ran and the addition guard below is skipped. ensurePod would recreate the node as a
					// non-validator/sentry and strip its local key, leaving the on-chain validator with no
					// signing path. Reject until the signer records its serving digest.
					return nil, fmt.Errorf(".spec.cosmosigner: the signer was responsible for an on-chain validator key at establishment but has not recorded a signing digest (webhooks disabled) — keep .spec.validator until it rolls out, or repair the configuration with webhooks enabled")
				}
			}
		}
	}

	// Once genesis has been created (status.chainID set), the genesis-initializing .spec.validator.init
	// is fixed: its validator is part of the immutable genesis validator set, and the whole init block
	// (chainID, genesis validators, accounts, gentx parameters, ...) determines what initGenesis builds.
	// Reject adding init to a running node, removing it from a genesis validator, or changing it — any of
	// these can leave a validator in genesis whose key the node no longer uses, or rebuild a different
	// genesis under the same chain ID if the node and its <chainID>-genesis ConfigMap are recreated.
	defaultPrivKeySecret := fmt.Sprintf("%s-priv-key", chainNode.GetName())
	newHasInit := chainNode.Spec.Validator != nil && chainNode.Spec.Validator.Init != nil
	if old != nil {
		// Webhook update path: compare against the previous spec.
		if old.Status.ChainID != "" {
			oldHasInit := old.Spec.Validator != nil && old.Spec.Validator.Init != nil
			switch {
			case newHasInit && !oldHasInit:
				return nil, fmt.Errorf(".spec.validator.init cannot be added after genesis has been created")
			case oldHasInit && !newHasInit:
				return nil, fmt.Errorf(".spec.validator.init cannot be removed after genesis has been created: its validator is part of the immutable genesis validator set")
			case oldHasInit && newHasInit:
				// A managed signer migration may intentionally change signing keys. When cosmosigner is
				// involved, keep the non-signing genesis configuration immutable while leaving the
				// on-chain key choice to the user.
				oldFP := old.Spec.Validator.GenesisSigningFingerprint(defaultPrivKeySecret)
				newFP := chainNode.Spec.Validator.GenesisSigningFingerprint(defaultPrivKeySecret)
				if old.Spec.Cosmosigner != nil || chainNode.Spec.Cosmosigner != nil {
					oldFP = genesisConfigurationFingerprint(old.Spec.Validator.Init, old.Spec.Validator.Info, old.Spec.Validator.GetAccountPrefix(), old.Spec.Validator.GetValPrefix(), old.Spec.Validator.GetAccountHDPath())
					newFP = genesisConfigurationFingerprint(chainNode.Spec.Validator.Init, chainNode.Spec.Validator.Info, chainNode.Spec.Validator.GetAccountPrefix(), chainNode.Spec.Validator.GetValPrefix(), chainNode.Spec.Validator.GetAccountHDPath())
				}
				if oldFP != newFP {
					return nil, fmt.Errorf(".spec.validator.init is immutable after genesis has been created: changing it would regenerate a different genesis for the same chain ID")
				}
			}
		}
	} else if chainNode.Status.ChainID != "" && chainNode.Status.GenesisSigningDigest != "" {
		// No-webhook reconcile path: no previous spec, so compare against what the controller recorded in
		// status when genesis was established.
		if chainNode.Status.GenesisSigningDigest == GenesisDigestExternal {
			// This node consumed an external genesis and is therefore not part of that genesis' validator
			// set; adding .validator.init now would set it up as a genesis validator outside that set.
			if newHasInit {
				return nil, fmt.Errorf(".spec.validator.init cannot be added after genesis has been created: this node consumed an external genesis and is not part of its validator set")
			}
		} else if chainNode.Spec.Validator.GenesisSigningFingerprint(defaultPrivKeySecret) != chainNode.Status.GenesisSigningDigest &&
			!chainNode.GenesisSigningDigestAllowsRefresh(chainNode.Status.GenesisSigningDigest) {
			// A node that initialized genesis recorded its init fingerprint; a current config that no
			// longer matches — init changed or removed — is rejected.
			return nil, fmt.Errorf(".spec.validator.init cannot be changed or removed after genesis has been created: its validator is part of the immutable genesis validator set")
		}
	}

	return chainNode.tmKMSDeprecationWarnings(), nil
}

// GenesisSigningDigestAllowsRefresh reports whether a recorded raw genesis fingerprint
// has the same non-signing genesis configuration as the current spec. Managed signer migrations may
// intentionally change keys, so the controller refreshes the raw signing baseline after migration.
func (chainNode *ChainNode) GenesisSigningDigestAllowsRefresh(recorded string) bool {
	if chainNode.Spec.Validator == nil || recorded == "" || recorded == GenesisDigestExternal {
		return false
	}
	if chainNode.Spec.Cosmosigner == nil && chainNode.Status.CosmosignerAppliedDigest == "" {
		return false
	}
	v := chainNode.Spec.Validator
	configuration := genesisConfigurationFingerprint(v.Init, v.Info, v.GetAccountPrefix(), v.GetValPrefix(), v.GetAccountHDPath())
	return strings.HasSuffix(recorded, "\x00"+configuration)
}

// GenesisSigningFingerprint mirrors NodeSetValidatorConfig.GenesisSigningFingerprint for a standalone
// ChainNode's ValidatorConfig: a stable fingerprint of the signing material, account-derivation
// settings and genesis identity that bind a genesis-initializing validator to the immutable genesis.
func (v *ValidatorConfig) GenesisSigningFingerprint(defaultPrivKeySecret string) string {
	if v == nil {
		return genesisSigningFingerprint(nil, nil, nil, nil, "", "", "", defaultPrivKeySecret)
	}
	return genesisSigningFingerprint(v.PrivateKeySecret, v.TmKMS, v.Init, v.Info, v.GetAccountPrefix(), v.GetValPrefix(), v.GetAccountHDPath(), defaultPrivKeySecret)
}

// validateGenesisValidators rejects duplicate signing keys or account mnemonics among
// .spec.validator.init.genesisValidators on a standalone ChainNode. GenesisValidators is shared with
// ChainNodeSet via GenesisInitConfig, but on a ChainNode it is otherwise unvalidated, so two entries
// referencing the same privKeySecret/accountMnemonicSecret — or one colliding with the init
// validator's own resolved priv-key/account secret — would be accepted here and only fail later at
// genesis creation.
func (chainNode *ChainNode) validateGenesisValidators() error {
	v := chainNode.Spec.Validator
	if v == nil || v.Init == nil {
		return nil
	}

	// Seed with the init validator's own resolved secrets so a genesis validator cannot collide with
	// the validator performing the initialization.
	privKeySecrets := map[string]string{v.GetPrivKeySecretName(chainNode): ".spec.validator"}
	accountSecrets := map[string]string{v.GetAccountSecretName(chainNode): ".spec.validator"}

	for i, gv := range v.Init.GenesisValidators {
		pkPath := fmt.Sprintf(".spec.validator.init.genesisValidators[%d].privKeySecret", i)
		if prev, ok := privKeySecrets[gv.PrivKeySecret]; ok {
			return fmt.Errorf("%s %q is already used by %s; each genesis validator must use a distinct private key", pkPath, gv.PrivKeySecret, prev)
		}
		privKeySecrets[gv.PrivKeySecret] = pkPath

		accPath := fmt.Sprintf(".spec.validator.init.genesisValidators[%d].accountMnemonicSecret", i)
		if prev, ok := accountSecrets[gv.AccountMnemonicSecret]; ok {
			return fmt.Errorf("%s %q is already used by %s; each genesis validator must use a distinct account", accPath, gv.AccountMnemonicSecret, prev)
		}
		accountSecrets[gv.AccountMnemonicSecret] = accPath
	}
	return nil
}

// isControlledByChainNodeSet is the webhook-local alias of IsControlledByChainNodeSet.
func isControlledByChainNodeSet(chainNode *ChainNode) bool {
	return chainNode.IsControlledByChainNodeSet()
}

// IsControlledByChainNodeSet reports whether the ChainNode carries a controller owner reference to a
// ChainNodeSet of this API group, i.e. it is a generated child rather than a user-created standalone
// node. It checks the APIVersion, controller flag and a non-empty UID so a hand-written reference to
// a foreign or non-existent object does not pass as easily; this is a footgun guard for
// controller-managed fields and labels, not a security boundary (a user able to create ChainNodes
// can already disrupt their own validators).
func (chainNode *ChainNode) IsControlledByChainNodeSet() bool {
	for _, ref := range chainNode.GetOwnerReferences() {
		if ref.Kind == "ChainNodeSet" &&
			ref.APIVersion == GroupVersion.String() &&
			ref.Controller != nil && *ref.Controller &&
			ref.UID != "" {
			return true
		}
	}
	return false
}

func validateSnapshotsConfig(config *VolumeSnapshotsConfig, path string) error {
	if config.Retention != nil && config.Retain != nil {
		return fmt.Errorf("%s.retention and %s.retain are mutually exclusive", path, path)
	}
	if config.ExportTarball != nil && config.ExportTarball.GCS != nil {
		if err := config.ExportTarball.GCS.Validate(fmt.Sprintf("%s.exportTarball.gcs", path)); err != nil {
			return err
		}
	}
	return nil
}
