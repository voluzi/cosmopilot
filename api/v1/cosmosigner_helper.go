package v1

import (
	"fmt"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/voluzi/cosmopilot/v2/pkg/utils"
)

// GetReplicas returns the configured number of signer replicas, defaulting to
// DefaultCosmosignerReplicas.
func (c *Cosmosigner) GetReplicas() int32 {
	if c != nil && c.Replicas != nil {
		return *c.Replicas
	}
	return DefaultCosmosignerReplicas
}

// GetImage returns the configured signer image, defaulting to DefaultCosmosignerImage.
func (c *Cosmosigner) GetImage() string {
	if c != nil && c.Image != nil && *c.Image != "" {
		return *c.Image
	}
	return DefaultCosmosignerImage
}

// GetStateStorageSize returns the configured per-replica state PVC size, defaulting to
// DefaultCosmosignerStateStorageSize.
func (c *Cosmosigner) GetStateStorageSize() string {
	if c != nil && c.StateStorageSize != nil {
		return *c.StateStorageSize
	}
	return DefaultCosmosignerStateStorageSize
}

// GetLogLevel returns the configured log level, defaulting to DefaultCosmosignerLogLevel.
func (c *Cosmosigner) GetLogLevel() string {
	if c != nil && c.LogLevel != nil && *c.LogLevel != "" {
		return *c.LogLevel
	}
	return DefaultCosmosignerLogLevel
}

// GetServiceAccountName returns the configured service account, empty for the namespace default.
func (c *Cosmosigner) GetServiceAccountName() string {
	if c != nil && c.ServiceAccountName != nil {
		return *c.ServiceAccountName
	}
	return ""
}

// GetResources returns the compute resources for the signer container, defaulting to a small
// request/limit when unset.
func (c *Cosmosigner) GetResources() corev1.ResourceRequirements {
	if c != nil && c.Resources != nil {
		return *c.Resources
	}
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(DefaultCosmosignerCpu),
			corev1.ResourceMemory: resource.MustParse(DefaultCosmosignerMemory),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse(DefaultCosmosignerCpu),
			corev1.ResourceMemory: resource.MustParse(DefaultCosmosignerMemory),
		},
	}
}

// UsesSoftwareBackend reports whether the software backend is configured.
func (c *Cosmosigner) UsesSoftwareBackend() bool {
	return c != nil && c.Backend.Software != nil
}

// UsesVaultBackend reports whether the Vault backend is configured.
func (c *Cosmosigner) UsesVaultBackend() bool {
	return c != nil && c.Backend.Vault != nil
}

// UsesGcpKmsBackend reports whether the GCP KMS backend is configured.
func (c *Cosmosigner) UsesGcpKmsBackend() bool {
	return c != nil && c.Backend.GcpKMS != nil
}

// VaultUploadsGenerated reports whether the Vault backend imports the validator's locally-generated
// key: explicitly via uploadGenerated, or implicitly when the targeted validator initializes a new
// genesis (the documented auto-default — a fresh genesis always generates its consensus key
// locally, so it must be imported for the signer to hold it).
func (c *Cosmosigner) VaultUploadsGenerated(initTarget bool) bool {
	return c.UsesVaultBackend() && (c.Backend.Vault.UploadGenerated || initTarget)
}

// CosmosignerTargetInitializesGenesis reports whether the cosmosigner-targeted validator (legacy
// singleton or a targeted validator group) initializes a new genesis.
func (nodeSet *ChainNodeSet) CosmosignerTargetInitializesGenesis() bool {
	for name := range nodeSet.CosmosignerTargetGroups() {
		if name == ReservedValidatorGroupName {
			if nodeSet.Spec.Validator != nil && nodeSet.Spec.Validator.Init != nil {
				return true
			}
			continue
		}
		for _, g := range nodeSet.Spec.Nodes {
			if g.Name == name && g.Validator != nil && g.Validator.Init != nil {
				return true
			}
		}
	}
	return false
}

// RequiresLocalPrivKey reports whether the backend needs a local priv_validator_key.json
// secret to be present: the software backend mounts it directly, and the Vault backend with
// uploadGenerated imports it. GCP and pre-provisioned Vault backends do not.
func (c *Cosmosigner) RequiresLocalPrivKey() bool {
	if c == nil {
		return false
	}
	if c.UsesSoftwareBackend() {
		return true
	}
	if c.UsesVaultBackend() && c.Backend.Vault.UploadGenerated {
		return true
	}
	return false
}

// GetVaultMount returns the Vault transit mount path, defaulting to DefaultCosmosignerVaultMount.
func (b *CosmosignerVaultBackend) GetVaultMount() string {
	if b != nil && b.Mount != nil && *b.Mount != "" {
		return *b.Mount
	}
	return DefaultCosmosignerVaultMount
}

// ImportFingerprint returns a stable fingerprint of a completed key import: the Vault target
// (address, namespace, mount, key) plus the resolved source secret the key came from. A change to
// either side triggers a fresh import — a new target must receive the key, and a new source means a
// different key must be (re)imported and verified. Both controllers stamp this value into the
// key-imported annotation; sharing one implementation keeps their import protocols in lockstep.
func (b *CosmosignerVaultBackend) ImportFingerprint(sourceSecret string) string {
	ns := ""
	if b.Namespace != nil {
		ns = *b.Namespace
	}
	return utils.Sha256(fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s", b.Address, ns, b.GetVaultMount(), b.KeyName, sourceSecret))
}

// CosmosignerTargetGroups returns the set of node group names the cosmosigner deployment signs
// for. An empty nodeGroups list defaults to the legacy singleton validator group. Returns nil when
// no cosmosigner is configured.
func (nodeSet *ChainNodeSet) CosmosignerTargetGroups() map[string]struct{} {
	if nodeSet.Spec.Cosmosigner == nil {
		return nil
	}
	targets := map[string]struct{}{}
	if len(nodeSet.Spec.Cosmosigner.NodeGroups) == 0 {
		if nodeSet.Spec.Validator != nil {
			targets[ReservedValidatorGroupName] = struct{}{}
		}
		return targets
	}
	for _, g := range nodeSet.Spec.Cosmosigner.NodeGroups {
		targets[g] = struct{}{}
	}
	return targets
}

// IsCosmosignerTargetGroup reports whether the given group is targeted by the cosmosigner
// deployment.
func (nodeSet *ChainNodeSet) IsCosmosignerTargetGroup(group string) bool {
	_, ok := nodeSet.CosmosignerTargetGroups()[group]
	return ok
}

// CosmosignerValidatorTargetSecret resolves the private-key secret of the single validator the
// cosmosigner deployment signs for, so the signer uses the exact consensus key that genesis or
// create-validator registers. It returns ("", false) for a sentry-mode signer that targets only
// regular (non-validator) groups. The webhook guarantees at most one validator target.
func (nodeSet *ChainNodeSet) CosmosignerValidatorTargetSecret() (string, bool) {
	if nodeSet.Spec.Cosmosigner == nil {
		return "", false
	}
	for name := range nodeSet.CosmosignerTargetGroups() {
		if name == ReservedValidatorGroupName {
			if nodeSet.Spec.Validator == nil {
				continue
			}
			if s := nodeSet.Spec.Validator.PrivateKeySecret; s != nil && *s != "" {
				return *s, true
			}
			return fmt.Sprintf("%s-validator-priv-key", nodeSet.GetName()), true
		}
		for _, g := range nodeSet.Spec.Nodes {
			if g.Name != name || g.Validator == nil {
				continue
			}
			if s := g.Validator.PrivateKeySecret; s != nil && *s != "" {
				return *s, true
			}
			// Validator groups targeted by cosmosigner are single-instance (webhook-enforced), so
			// instance 0's default key is the validator's key.
			return fmt.Sprintf("%s-%s-0-priv-key", nodeSet.GetName(), name), true
		}
	}
	return "", false
}

// Validate performs self-contained validation of a Cosmosigner block. isNodeSet indicates whether
// the block lives on a ChainNodeSet (where nodeGroups is meaningful) or a standalone ChainNode
// (where nodeGroups must be empty). path is the field path used in error messages.
func (c *Cosmosigner) Validate(path string, isNodeSet bool) error {
	if c == nil {
		return nil
	}

	// Exactly one backend must be configured.
	backends := 0
	if c.Backend.Software != nil {
		backends++
	}
	if c.Backend.Vault != nil {
		backends++
	}
	if c.Backend.GcpKMS != nil {
		backends++
	}
	if backends == 0 {
		return fmt.Errorf("%s.backend must configure exactly one of software, vault or gcpKms", path)
	}
	if backends > 1 {
		return fmt.Errorf("%s.backend must configure exactly one of software, vault or gcpKms, not multiple", path)
	}

	// Replicas must be an odd number so the embedded raft cluster can form a quorum.
	replicas := c.GetReplicas()
	if replicas < 1 {
		return fmt.Errorf("%s.replicas must be at least 1", path)
	}
	if replicas%2 == 0 {
		return fmt.Errorf("%s.replicas must be an odd number (1, 3, 5, ...) so the raft cluster can form a quorum", path)
	}

	// Validate state storage size when set.
	if c.StateStorageSize != nil {
		if _, err := resource.ParseQuantity(*c.StateStorageSize); err != nil {
			return fmt.Errorf("bad format for %s.stateStorageSize: %v", path, err)
		}
	}

	// Backend-specific required fields.
	switch {
	case c.Backend.Vault != nil:
		if c.Backend.Vault.Address == "" {
			return fmt.Errorf("%s.backend.vault.address is required", path)
		}
		if c.Backend.Vault.KeyName == "" {
			return fmt.Errorf("%s.backend.vault.keyName is required", path)
		}
		// Both name and key are required: the controller mounts the token with the selector key as a
		// SubPath, so an empty key would mount the whole secret directory over the token file path.
		if c.Backend.Vault.TokenSecret == nil || c.Backend.Vault.TokenSecret.Name == "" || c.Backend.Vault.TokenSecret.Key == "" {
			return fmt.Errorf("%s.backend.vault.tokenSecret.name and .key are required", path)
		}
		if cs := c.Backend.Vault.CertificateSecret; cs != nil && (cs.Name == "" || cs.Key == "") {
			return fmt.Errorf("%s.backend.vault.certificateSecret.name and .key are required when set", path)
		}
	case c.Backend.GcpKMS != nil:
		if c.Backend.GcpKMS.KeyVersion == "" {
			return fmt.Errorf("%s.backend.gcpKms.keyVersion is required", path)
		}
		if cs := c.Backend.GcpKMS.CredentialsSecret; cs != nil && (cs.Name == "" || cs.Key == "") {
			return fmt.Errorf("%s.backend.gcpKms.credentialsSecret.name and .key are required when set", path)
		}
	}

	// nodeGroups only applies to a ChainNodeSet.
	if !isNodeSet && len(c.NodeGroups) > 0 {
		return fmt.Errorf("%s.nodeGroups is only valid on a ChainNodeSet", path)
	}

	// An empty raftTLSSecret would render a Secret volume with an empty name.
	if c.RaftTLSSecret != nil && *c.RaftTLSSecret == "" {
		return fmt.Errorf("%s.raftTLSSecret must not be empty when set", path)
	}

	return nil
}

// effectiveSigningIdentity returns a fingerprint of the actual consensus key the cosmosigner signs
// with. softwareKeySecret is the resolved key secret used by the software backend.
func (c *Cosmosigner) effectiveSigningIdentity(softwareKeySecret string) string {
	if c == nil {
		return ""
	}
	switch {
	case c.UsesVaultBackend():
		v := c.Backend.Vault
		ns := ""
		if v.Namespace != nil {
			ns = *v.Namespace
		}
		return fmt.Sprintf("vault\x00%s\x00%s\x00%s\x00%s", v.Address, ns, v.GetVaultMount(), v.KeyName)
	case c.UsesGcpKmsBackend():
		return "gcpkms\x00" + c.Backend.GcpKMS.KeyVersion
	case c.UsesSoftwareBackend():
		return "software\x00" + softwareKeySecret
	}
	return ""
}

// CosmosignerSigningIdentity returns a fingerprint of the consensus key a standalone ChainNode's
// cosmosigner signs with, resolving the software backend to the node's own key secret.
func (chainNode *ChainNode) CosmosignerSigningIdentity() string {
	c := chainNode.Spec.Cosmosigner
	if c == nil {
		return ""
	}
	softwareKey := ""
	if c.UsesSoftwareBackend() {
		switch {
		case chainNode.Spec.Validator != nil:
			softwareKey = chainNode.Spec.Validator.GetPrivKeySecretName(chainNode)
		case c.Backend.Software.PrivateKeySecret != nil && *c.Backend.Software.PrivateKeySecret != "":
			softwareKey = *c.Backend.Software.PrivateKeySecret
		default:
			softwareKey = fmt.Sprintf("%s-priv-key", chainNode.GetName())
		}
	}
	return c.effectiveSigningIdentity(softwareKey)
}

// CosmosignerSigningIdentity returns a fingerprint of the consensus key a ChainNodeSet's cosmosigner
// signs with, resolving the software backend to the targeted validator's key secret.
func (nodeSet *ChainNodeSet) CosmosignerSigningIdentity() string {
	c := nodeSet.Spec.Cosmosigner
	if c == nil {
		return ""
	}
	softwareKey := ""
	if c.UsesSoftwareBackend() {
		if secret, ok := nodeSet.CosmosignerValidatorTargetSecret(); ok {
			softwareKey = secret
		} else if s := c.Backend.Software.PrivateKeySecret; s != nil {
			softwareKey = *s
		}
	}
	return c.effectiveSigningIdentity(softwareKey)
}

// localKeySigningIdentity fingerprints a locally-held consensus key by its secret name.
func localKeySigningIdentity(secret string) string {
	return "software\x00" + secret
}

// CosmosignerSigningDigest fingerprints a ChainNodeSet's managed signer for status persistence: the
// effective signing identity, the replica count, and the target-group set. Empty when no cosmosigner
// is set. The preimage is length-prefixed and NUL-separated so it stays unambiguous regardless of
// group-name contents.
func (nodeSet *ChainNodeSet) CosmosignerSigningDigest() string {
	if nodeSet.Spec.Cosmosigner == nil {
		return ""
	}
	targets := make([]string, 0)
	for t := range nodeSet.CosmosignerTargetGroups() {
		targets = append(targets, t)
	}
	sort.Strings(targets)
	preimage := fmt.Sprintf("%s\x00%d\x00%d\x00%s",
		nodeSet.CosmosignerSigningIdentity(), nodeSet.Spec.Cosmosigner.GetReplicas(), len(targets), strings.Join(targets, "\x00"))
	return utils.Sha256(preimage)
}

// CosmosignerSigningDigest fingerprints a standalone ChainNode's managed signer for status
// persistence (effective signing identity plus replica count). Empty when no cosmosigner is set.
func (chainNode *ChainNode) CosmosignerSigningDigest() string {
	if chainNode.Spec.Cosmosigner == nil {
		return ""
	}
	preimage := fmt.Sprintf("%s\x00%d", chainNode.CosmosignerSigningIdentity(), chainNode.Spec.Cosmosigner.GetReplicas())
	return utils.Sha256(preimage)
}

// equalStringSet reports whether two set-valued maps contain the same keys.
func equalStringSet(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

// EffectiveSigningIdentity returns a normalized fingerprint of the consensus key this ChainNode
// signs with, across every signing path (local key, tmKMS, cosmosigner). Equivalent keys compare
// equal — e.g. the same Vault Transit key referenced through tmKMS or cosmosigner — so a same-key
// migration is not flagged as a change while a real key change is. Empty when the node neither
// validates nor hosts a signer.
func (chainNode *ChainNode) EffectiveSigningIdentity() string {
	switch {
	case chainNode.Spec.Cosmosigner != nil:
		return chainNode.CosmosignerSigningIdentity()
	case chainNode.UsesTmKms():
		if id, ok := tmkmsNormalizedVaultKey(chainNode.Spec.Validator.TmKMS); ok {
			return id
		}
		return "tmkms\x00unconfigured"
	case chainNode.IsValidator():
		return localKeySigningIdentity(chainNode.Spec.Validator.GetPrivKeySecretName(chainNode))
	default:
		return ""
	}
}

// cosmosignerTargetSigningIdentity returns the effective consensus-key fingerprint of the validator
// the cosmosigner targets (or would target). It is empty when the target set resolves no validator
// (sentry mode over regular groups): such a signer's key is registered out-of-band and carries no
// in-cluster validator identity to protect, so add/remove/rotate stays allowed. When a validator IS
// targeted, the signer's identity (cosmosigner configured) or the validator's own local/tmKMS
// identity (not configured) is returned. targets is the set of targeted group names, resolved from
// whichever spec revision carries the cosmosigner block.
func (nodeSet *ChainNodeSet) cosmosignerTargetSigningIdentity(targets map[string]struct{}) string {
	hasValidatorTarget := false
	for name := range targets {
		if name == ReservedValidatorGroupName {
			if nodeSet.Spec.Validator != nil {
				hasValidatorTarget = true
			}
			continue
		}
		for _, g := range nodeSet.Spec.Nodes {
			if g.Name == name && g.Validator != nil {
				hasValidatorTarget = true
			}
		}
	}
	if !hasValidatorTarget {
		return ""
	}
	if nodeSet.Spec.Cosmosigner != nil {
		return nodeSet.CosmosignerSigningIdentity()
	}
	for name := range targets {
		if name == ReservedValidatorGroupName {
			if nodeSet.Spec.Validator == nil {
				continue
			}
			return nodeSet.validatorGroupSigningIdentity(ReservedValidatorGroupName, nodeSet.Spec.Validator)
		}
		for _, g := range nodeSet.Spec.Nodes {
			if g.Name == name && g.Validator != nil {
				return nodeSet.validatorGroupSigningIdentity(name, g.Validator)
			}
		}
	}
	return ""
}

// validatorGroupSigningIdentity returns the effective consensus-key fingerprint of a validator group
// (or the legacy singleton) that signs through its own local key or tmKMS.
func (nodeSet *ChainNodeSet) validatorGroupSigningIdentity(group string, cfg *NodeSetValidatorConfig) string {
	if cfg == nil {
		return ""
	}
	if id, ok := tmkmsNormalizedVaultKey(cfg.TmKMS); ok {
		return id
	}
	if cfg.TmKMS != nil {
		return "tmkms\x00unconfigured"
	}
	if cfg.PrivateKeySecret != nil && *cfg.PrivateKeySecret != "" {
		return localKeySigningIdentity(*cfg.PrivateKeySecret)
	}
	if group == ReservedValidatorGroupName {
		return localKeySigningIdentity(fmt.Sprintf("%s-validator-priv-key", nodeSet.GetName()))
	}
	return localKeySigningIdentity(fmt.Sprintf("%s-%s-0-priv-key", nodeSet.GetName(), group))
}

// ValidateCosmosignerReservedName rejects creating a ChainNode/ChainNodeSet whose NAME collides
// with the signer resource names another CR would derive. A CR named `foo` that enables cosmosigner
// derives `foo-signer` (StatefulSet/ConfigMap/raft Service) and `foo-signer-privval` (discovery
// Service), while an ordinary ChainNode's own Service/ConfigMap use the raw CR name. Both suffixes
// are therefore reserved:
//   - a CR named `foo-signer` collides with signer-enabled `foo`'s StatefulSet/ConfigMap/Service;
//   - a CR named `foo-signer-privval` (which does NOT end in "-signer") collides with
//     signer-enabled `foo`'s discovery Service.
//
// Only enforced on create (isCreate) so pre-existing CRs with such names keep updating; the
// reconcilers' ownership guards remain the backstop for them.
func ValidateCosmosignerReservedName(name string, isCreate bool) error {
	if !isCreate {
		return nil
	}
	if strings.HasSuffix(name, "-signer") || strings.HasSuffix(name, "-signer-privval") {
		return fmt.Errorf("metadata.name %q is reserved: the \"-signer\"/\"-signer-privval\" suffixes collide with cosmosigner-managed resource names derived from other resources; choose a different name", name)
	}
	return nil
}

// validateCosmosignerReplicasImmutable rejects a change to the signer replica count. Scaling the
// embedded raft cluster is not a plain Kubernetes scale: the membership recorded in the existing
// per-pod raft state is not updated by rendering a new bootstrap list, so scaling down can lose
// quorum and scaling up starts pods outside the existing cluster.
func validateCosmosignerReplicasImmutable(oldC, newC *Cosmosigner) error {
	if oldC == nil || newC == nil {
		return nil
	}
	if oldC.GetReplicas() != newC.GetReplicas() {
		return fmt.Errorf(".spec.cosmosigner.replicas is immutable after creation: changing it does not migrate the raft membership in the signer's state and can break quorum")
	}
	return nil
}
