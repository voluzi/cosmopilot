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
			case oldHasInit && newHasInit && old.Spec.Validator.GenesisSigningFingerprint(defaultPrivKeySecret) != chainNode.Spec.Validator.GenesisSigningFingerprint(defaultPrivKeySecret):
				return nil, fmt.Errorf(".spec.validator.init is immutable after genesis has been created: changing it would regenerate a different genesis for the same chain ID")
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

func validateSnapshotsConfig(config *VolumeSnapshotsConfig, path string) error {
	if config.Retention != nil && config.Retain != nil {
		return fmt.Errorf("%s.retention and %s.retain are mutually exclusive", path, path)
	}
	return nil
}
