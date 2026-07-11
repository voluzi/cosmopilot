package v1

import (
	"context"
	"fmt"

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

func (chainNode *ChainNode) Validate(old *ChainNode) (admission.Warnings, error) {
	// Validate persistence size
	_, err := resource.ParseQuantity(chainNode.GetPersistenceSize())
	if err != nil {
		return nil, fmt.Errorf("bad format for .spec.size: %v", err)
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
		hasValidatorKey := isValidator && chainNode.Spec.Validator.PrivateKeySecret != nil && *chainNode.Spec.Validator.PrivateKeySecret != ""

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
		// imports it) — not a pre-provisioned Vault/GCP key with a different pubkey. This is waived
		// for a same-key migration on an established chain: the previous signing path already put this
		// exact key on-chain, so a pre-provisioned key that matches it is correct (e.g. tmKMS→
		// cosmosigner on the same Vault key, where the key is already in Vault).
		sameKeyMigration := old != nil && old.Status.ChainID != "" && old.EffectiveSigningIdentity() == chainNode.EffectiveSigningIdentity()
		if registers && !sameKeyMigration {
			matches := c.UsesSoftwareBackend() || (c.UsesVaultBackend() && c.Backend.Vault.UploadGenerated)
			if !matches {
				return nil, fmt.Errorf(".spec.cosmosigner on a validator that initializes genesis or uses createValidator requires the software backend or vault.uploadGenerated so the registered consensus key matches the signer")
			}
		}
	}

	if old != nil {
		if err := validateCosmosignerReplicasImmutable(old.Spec.Cosmosigner, chainNode.Spec.Cosmosigner); err != nil {
			return nil, err
		}
		// Once the chain is established, the validator's consensus pubkey is fixed in the on-chain
		// validator set. Changing the effective signing key — including adding, removing or switching
		// the cosmosigner backend — would make the node sign with a key not in that set. Equivalent
		// keys (e.g. the same Vault key via tmKMS or cosmosigner) compare equal, so a same-key
		// migration is still allowed. The guard only applies when the OLD node was a validator: a
		// non-validator sentry node's key is registered out-of-band, so adding/removing its signer is
		// not blocked. For an old validator, an update that empties the identity (dropping both the
		// validator block and the signer) is also rejected — it would leave the on-chain validator
		// with no signing path at all.
		if old.Status.ChainID != "" && (old.Spec.Cosmosigner != nil || chainNode.Spec.Cosmosigner != nil) {
			oldIdentity := old.EffectiveSigningIdentity()
			newIdentity := chainNode.EffectiveSigningIdentity()
			if oldIdentity != "" && old.Spec.Validator != nil && (newIdentity == "" || oldIdentity != newIdentity) {
				return nil, fmt.Errorf("the consensus signing key is immutable after the chain is established: changing or removing the cosmosigner/signing configuration would leave the validator signing with a key not in the on-chain validator set (or not signing at all)")
			}
		}
	}

	// No-webhook reconcile path (old is nil): the controller records the cosmosigner signing identity
	// in status once established, so a later removal or change of the signing configuration is rejected
	// here (the webhook update path above already covers this when webhooks are enabled).
	if old == nil && chainNode.Status.CosmosignerSigningDigest != "" {
		if chainNode.Spec.Cosmosigner == nil {
			return nil, fmt.Errorf(".spec.cosmosigner cannot be removed after the chain is established (webhooks disabled): the validator's key is fixed on-chain")
		}
		if chainNode.CosmosignerSigningDigest() != chainNode.Status.CosmosignerSigningDigest {
			return nil, fmt.Errorf(".spec.cosmosigner signing configuration is immutable after the chain is established (webhooks disabled)")
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
				// When cosmosigner is involved on either side, compare using the effective signing
				// identity so a same-key signer migration (e.g. tmKMS→cosmosigner on the same Vault key)
				// is not misread as a genesis change; the init block and account settings are still
				// compared, so genuine changes are rejected.
				oldFP := old.Spec.Validator.GenesisSigningFingerprint(defaultPrivKeySecret)
				newFP := chainNode.Spec.Validator.GenesisSigningFingerprint(defaultPrivKeySecret)
				if old.Spec.Cosmosigner != nil || chainNode.Spec.Cosmosigner != nil {
					oldFP, newFP = old.effectiveGenesisFingerprint(), chainNode.effectiveGenesisFingerprint()
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
		} else if chainNode.Spec.Validator.GenesisSigningFingerprint(defaultPrivKeySecret) != chainNode.Status.GenesisSigningDigest {
			// A node that initialized genesis recorded its init fingerprint; a current config that no
			// longer matches — init changed or removed — is rejected.
			return nil, fmt.Errorf(".spec.validator.init cannot be changed or removed after genesis has been created: its validator is part of the immutable genesis validator set")
		}
	}

	return nil, nil
}

// effectiveGenesisFingerprint is like GenesisSigningFingerprint but uses the node's effective signing
// identity (which normalizes equivalent keys — the same Vault key via tmKMS or cosmosigner) instead
// of the raw private-key-secret + tmKMS material, so a same-key signer migration is not treated as a
// genesis change while genuine init/account changes still are. Callers guarantee Spec.Validator is
// non-nil, so EffectiveSigningIdentity always resolves.
func (chainNode *ChainNode) effectiveGenesisFingerprint() string {
	v := chainNode.Spec.Validator
	return genesisSigningFingerprintWithIdentity(chainNode.EffectiveSigningIdentity(), v.Init, v.Info, v.GetAccountPrefix(), v.GetValPrefix(), v.GetAccountHDPath())
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

// isControlledByChainNodeSet reports whether the ChainNode carries a controller owner reference to a
// ChainNodeSet of this API group, i.e. it is a generated child rather than a user-created standalone
// node. It checks the APIVersion, controller flag and a non-empty UID so a hand-written reference to
// a foreign or non-existent object does not pass as easily; this is a footgun guard for a
// controller-managed field, not a security boundary (a user able to create ChainNodes can already
// disrupt their own validators).
func isControlledByChainNodeSet(chainNode *ChainNode) bool {
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
