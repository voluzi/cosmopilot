package v1

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/utils/ptr"

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

// GetImage returns the configured signer image: the explicit per-CR override when set, otherwise
// operatorDefault (the operator-wide cosmosigner image, wired from the `-cosmosigner-image` /
// `COSMOSIGNER_IMAGE` flag — see ControllerRunOptions.CosmosignerImage), and DefaultCosmosignerImage
// only when even that is empty (e.g. a manager run with no image configured at all).
func (c *Cosmosigner) GetImage(operatorDefault string) string {
	if c != nil && c.Image != nil && *c.Image != "" {
		return *c.Image
	}
	if operatorDefault != "" {
		return operatorDefault
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

// groupCosmosigner returns the Cosmosigner block targeting the given group: the group's own
// .spec.nodes[].cosmosigner, or the top-level .spec.cosmosigner when it lists that group. Returns nil
// when no signer targets the group.
func (nodeSet *ChainNodeSet) groupCosmosigner(group string) *Cosmosigner {
	for i := range nodeSet.Spec.Nodes {
		g := &nodeSet.Spec.Nodes[i]
		if g.Name == group && g.Cosmosigner != nil {
			return g.Cosmosigner
		}
	}
	if c := nodeSet.Spec.Cosmosigner; c != nil {
		if len(c.NodeGroups) == 0 {
			if group == ReservedValidatorGroupName && nodeSet.Spec.Validator != nil {
				return c
			}
		} else {
			for _, n := range c.NodeGroups {
				if n == group {
					return c
				}
			}
		}
	}
	return nil
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

// GetKeyVersion returns the pinned Vault Transit key version, defaulting to version 1.
func (b *CosmosignerVaultBackend) GetKeyVersion() int {
	if b != nil && b.KeyVersion != nil {
		return *b.KeyVersion
	}
	return 1
}

// ImportTargetFingerprint fingerprints the import TARGET half of a key import: the Vault destination
// (address, namespace, mount, key) plus the resolved source secret name. It deliberately excludes the
// key material, so a controller that can no longer read the source Secret (deleted after a completed
// bootstrap import) can still verify that a recorded import annotation belongs to the CURRENT
// target/source — a stale annotation from a different Vault destination or source secret never
// satisfies the current spec.
func (b *CosmosignerVaultBackend) ImportTargetFingerprint(sourceSecret string) string {
	ns := ""
	if b.Namespace != nil {
		ns = *b.Namespace
	}
	return utils.Sha256(fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%d\x00%s", b.Address, ns, b.GetVaultMount(), b.KeyName, b.GetKeyVersion(), sourceSecret))
}

// LegacyImportTargetFingerprint reproduces the import target fingerprint written before Vault key
// versions were pinned. It is retained only to migrate existing version-1 status records.
func (b *CosmosignerVaultBackend) LegacyImportTargetFingerprint(sourceSecret string) string {
	ns := ""
	if b.Namespace != nil {
		ns = *b.Namespace
	}
	return utils.Sha256(fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s", b.Address, ns, b.GetVaultMount(), b.KeyName, sourceSecret))
}

// ImportFingerprint returns the full fingerprint of a completed key import:
// "<targetFingerprint>.<materialHash>". A change to the Vault target, the source secret name, or the
// key BYTES (an in-place Secret update) produces a different value and so triggers a fresh import —
// preventing Vault from silently holding a stale key while genesis/signing flows consume new bytes.
// The two-part form lets the absent-source fast-path match on the target half alone (see
// ImportTargetFingerprint). Both controllers stamp this value into controller-owned status;
// sharing one implementation keeps their import protocols in lockstep.
func (b *CosmosignerVaultBackend) ImportFingerprint(sourceSecret string, keyMaterial []byte) string {
	return b.ImportTargetFingerprint(sourceSecret) + "." + utils.Sha256(string(keyMaterial))
}

// LegacyImportFingerprint reproduces the full import proof written before Vault key versions were
// pinned. Callers must only accept it through ImportRecordMatches, which limits compatibility to
// version 1.
func (b *CosmosignerVaultBackend) LegacyImportFingerprint(sourceSecret string, keyMaterial []byte) string {
	return b.LegacyImportTargetFingerprint(sourceSecret) + "." + utils.Sha256(string(keyMaterial))
}

// ImportRecordMatchesTarget reports whether a recorded key import belongs to the given import target
// (see ImportTargetFingerprint) regardless of which key bytes were imported. Used by the absent-source
// fast-path: only an import completed for the CURRENT target/source proves the spec's Vault key holds
// the registered material.
func ImportRecordMatchesTarget(record, targetFingerprint string) bool {
	return strings.HasPrefix(record, targetFingerprint+".")
}

// ImportRecordMatchesTarget reports whether record proves an import into this backend and source.
// Version 1 also accepts the pre-key-version format so an operator upgrade does not attempt to import
// over an already-existing Vault Transit key.
func (b *CosmosignerVaultBackend) ImportRecordMatchesTarget(record, sourceSecret string) bool {
	if ImportRecordMatchesTarget(record, b.ImportTargetFingerprint(sourceSecret)) {
		return true
	}
	return b.GetKeyVersion() == 1 && ImportRecordMatchesTarget(record, b.LegacyImportTargetFingerprint(sourceSecret))
}

// ImportRecordMatches reports whether record proves that these exact source bytes were imported into
// this backend. Legacy compatibility is deliberately limited to version 1, the version created by the
// old unversioned import path.
func (b *CosmosignerVaultBackend) ImportRecordMatches(record, sourceSecret string, keyMaterial []byte) bool {
	if record == b.ImportFingerprint(sourceSecret, keyMaterial) {
		return true
	}
	return b.GetKeyVersion() == 1 && record == b.LegacyImportFingerprint(sourceSecret, keyMaterial)
}

// CosmosignerValidatorTargetedIdentity returns the signer's effective signing identity ONLY when it
// targets this node's validator, and "" otherwise (no signer, or the node is not a validator). The
// at-establishment marker stores this value rather than the bare key identity: a sentry-mode signer
// must record "" so that a later retargeting of the same key onto a validator does not masquerade as
// the establishing configuration (the key may not be the on-chain validator key).
func (chainNode *ChainNode) CosmosignerValidatorTargetedIdentity() string {
	if chainNode.Spec.Cosmosigner == nil || !chainNode.IsValidator() {
		return ""
	}
	return chainNode.CosmosignerSigningIdentity()
}

// SetEstablishedChainID records the chain ID and — atomically, in the same status write the caller
// is about to perform — the write-once cosmosigner-at-establishment marker, holding the
// validator-targeted signer identity ("" when none, including sentry mode). Recording both together
// closes the window in which a chain is established but the marker is still nil, during which an
// unverifiable signer addition could slip past the no-webhook guard and then be blessed by a late
// marker write. A no-op for an empty chainID (not yet known).
func (chainNode *ChainNode) SetEstablishedChainID(chainID string) {
	if chainID == "" {
		return
	}
	chainNode.Status.ChainID = chainID
	if chainNode.Status.CosmosignerAtEstablishment == nil {
		identity := chainNode.CosmosignerValidatorTargetedIdentity()
		chainNode.Status.CosmosignerAtEstablishment = &identity
	}
}

// SetEstablishedChainID is the ChainNodeSet counterpart of the ChainNode method: it records the
// chain ID and, for every signer present at establishment, the write-once at-establishment marker
// (its validator-targeted identity, "" for sentry). A signer added after establishment has no entry
// and so stays subject to the no-webhook addition guard.
func (nodeSet *ChainNodeSet) SetEstablishedChainID(chainID string) {
	if chainID == "" {
		return
	}
	establishing := nodeSet.Status.ChainID == ""
	nodeSet.Status.ChainID = chainID
	for _, s := range nodeSet.ResolveCosmosigners() {
		st := nodeSet.EnsureCosmosignerStatus(s.Name)
		if st.AtEstablishment == nil {
			id := s.ValidatorTargetedIdentity()
			if id == "" {
				id = nodeSet.genesisSentryEstablishmentIdentity(s)
			}
			st.AtEstablishment = &id
			// Pin the served group for a validator-targeted signer, so the pre-digest no-webhook guard
			// can reject moving validator-ness to a SIBLING target group before the rollout digest
			// records the served group. Without this, a signer targeting multiple groups could swap which
			// group is the validator while keeping the same backend identity — the identity check alone
			// would still pass. Recorded once, alongside the write-once marker; the rollout later records
			// the same value with the digest.
			if s.ValidatorGroup != "" {
				st.ServingGroup = s.ValidatorGroup
			}
		}
		if establishing && s.TargetsValidator() && st.LocalKeyEverServed == nil {
			st.LocalKeyEverServed = ptr.To(nodeSet.SignerUsesLocalValidatorKey(s))
		}
	}
}

// genesisSentryEstablishmentIdentity returns the at-establishment identity to record for a SOFTWARE
// SENTRY signer whose key is listed in this ChainNodeSet's own init.genesisValidators — that key's
// signing identity — or "" for any signer that is not such a sentry. Only software sentries populate
// SoftwareKeySecret, and the genesis set is keyed by privateKeySecret name, so this is exactly the
// genesis-registered sentry case the controller can prove from spec; every other sentry (Vault/GCP, or
// software but not genesis-registered) records "" and stays freely rotatable/removable on the no-webhook
// path. Because the genesis set is immutable, this is stable whether evaluated at establishment or when
// a genesis-sentry signer's status entry is first created later (see initCosmosignerLocks).
func (nodeSet *ChainNodeSet) genesisSentryEstablishmentIdentity(s ResolvedSigner) string {
	if s.ValidatorGroup != "" || s.SoftwareKeySecret == "" {
		return ""
	}
	if _, genesis := nodeSet.genesisValidatorPrivKeySecrets()[s.SoftwareKeySecret]; genesis {
		return s.Identity()
	}
	return ""
}

// GenesisSentryEstablishmentIdentity is the exported form of genesisSentryEstablishmentIdentity, used
// by the controller to backfill AtEstablishment when a genesis-registered software sentry signer's
// status entry is first created after establishment (SetEstablishedChainID runs only once).
func (nodeSet *ChainNodeSet) GenesisSentryEstablishmentIdentity(s ResolvedSigner) string {
	return nodeSet.genesisSentryEstablishmentIdentity(s)
}

// IsCosmosignerTargetGroup reports whether any managed signer (top-level or per-group) targets the
// given group — i.e. that group's nodes listen for a remote signer and mount no local key.
func (nodeSet *ChainNodeSet) IsCosmosignerTargetGroup(group string) bool {
	for _, s := range nodeSet.ResolveCosmosigners() {
		for _, t := range s.TargetGroups {
			if t == group {
				return true
			}
		}
	}
	return false
}

// Validate performs self-contained validation of a Cosmosigner block. allowNodeGroups is true only
// for the top-level .spec.cosmosigner on a ChainNodeSet; a standalone ChainNode's block and a
// per-group .spec.nodes[].cosmosigner (whose target is the enclosing group) must leave nodeGroups
// empty. path is the field path used in error messages.
func (c *Cosmosigner) Validate(path string, allowNodeGroups bool) error {
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
		if c.Backend.Vault.GetKeyVersion() < 1 {
			return fmt.Errorf("%s.backend.vault.keyVersion must be at least 1", path)
		}
		if c.Backend.Vault.UploadGenerated && c.Backend.Vault.GetKeyVersion() != 1 {
			return fmt.Errorf("%s.backend.vault.uploadGenerated requires keyVersion 1 because Vault imports create the initial key version", path)
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

	// nodeGroups is only valid on the top-level .spec.cosmosigner of a ChainNodeSet.
	if !allowNodeGroups && len(c.NodeGroups) > 0 {
		return fmt.Errorf("%s.nodeGroups is not valid here (the target is fixed): only the top-level .spec.cosmosigner selects node groups", path)
	}

	// An empty raftTLSSecret would render a Secret volume with an empty name.
	if c.RaftTLSSecret != nil && *c.RaftTLSSecret == "" {
		return fmt.Errorf("%s.raftTLSSecret must not be empty when set", path)
	}
	if replicas > 1 && c.RaftTLSSecret == nil && !c.UnsafeAllowInsecureRaft {
		return fmt.Errorf("%s.raftTLSSecret is required when replicas is greater than 1; set unsafeAllowInsecureRaft only for an isolated test network", path)
	}
	if c.RaftTLSSecret != nil && c.UnsafeAllowInsecureRaft {
		return fmt.Errorf("%s.raftTLSSecret and unsafeAllowInsecureRaft are mutually exclusive", path)
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
		return normalizedVaultIdentity(v.Address, ns, v.GetVaultMount(), v.KeyName, v.GetKeyVersion())
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

// localKeySigningIdentity fingerprints a locally-held consensus key by its secret name.
func localKeySigningIdentity(secret string) string {
	return "software\x00" + secret
}

// CosmosignerSigningDigest fingerprints a standalone ChainNode's managed signer for status
// persistence (effective signing identity, replica count, and validator targeting). Empty when no
// cosmosigner is set.
func (chainNode *ChainNode) CosmosignerSigningDigest() string {
	if chainNode.Spec.Cosmosigner == nil {
		return ""
	}
	preimage := fmt.Sprintf("%s\x00%d\x00%t", chainNode.CosmosignerSigningIdentity(), chainNode.Spec.Cosmosigner.GetReplicas(), chainNode.IsValidator())
	return utils.Sha256(preimage)
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

// ValidatorResolvesSigningIdentity reports whether the standalone node's validator resolves the
// given effective signing identity through its OWN signing path (local key or tmKMS) — i.e. ignoring
// any .spec.cosmosigner. See the ChainNodeSet counterpart for the rationale.
func (chainNode *ChainNode) ValidatorResolvesSigningIdentity(identity string) bool {
	if identity == "" || chainNode.Spec.Validator == nil {
		return false
	}
	if id, ok := tmkmsNormalizedVaultKey(chainNode.Spec.Validator.TmKMS); ok {
		return id == identity
	}
	if chainNode.UsesTmKms() {
		return identity == "tmkms\x00unconfigured"
	}
	return localKeySigningIdentity(chainNode.Spec.Validator.GetPrivKeySecretName(chainNode)) == identity
}

// validatorGroupSigningIdentity returns the effective own-path (local key or tmKMS) consensus-key
// fingerprint of a validator group's representative (instance 0), or the legacy singleton, ignoring
// any cosmosigner.
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
	return localKeySigningIdentity(nodeSet.validatorKeySecret(group))
}

// ValidatorGroupResolvesSigningIdentity reports whether a validator group's own local/tmKMS path
// points at the recorded signer identity.
func (nodeSet *ChainNodeSet) ValidatorGroupResolvesSigningIdentity(group string, cfg *NodeSetValidatorConfig, identity string) bool {
	return identity != "" && nodeSet.validatorGroupSigningIdentity(group, cfg) == identity
}

// ValidateCosmosignerReservedNameNoWebhook applies the reserved-name rule on the no-webhook
// reconcile path, where create cannot be distinguished from update. It enforces only while the
// object has never been successfully reconciled (isEstablished == false, i.e. empty status), so a
// pre-existing legacy resource with a reserved name keeps updating while a NEW no-webhook resource
// with a reserved name is rejected before the controllers start fighting over derived names.
func ValidateCosmosignerReservedNameNoWebhook(name string, isEstablished bool) error {
	if isEstablished {
		return nil
	}
	return ValidateReservedResourceName(name, true)
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
	return validateCosmosignerStateStorageImmutable(".spec.cosmosigner", oldC, newC)
}

// validateCosmosignerStateStorageImmutable rejects a change to the signer's raft-state PVC template
// (size or storage class). A StatefulSet's volumeClaimTemplates cannot be updated in place — the
// reconciler preserves the live template to keep the update admissible — so an accepted change would
// be silently ignored forever. Rejecting it makes the limitation explicit: recreate the signer
// (remove + re-add) to change its state storage.
func validateCosmosignerStateStorageImmutable(path string, oldC, newC *Cosmosigner) error {
	if oldC == nil || newC == nil {
		return nil
	}
	if !CosmosignerStateStorageEqual(oldC.GetStateStorageSize(), oldC.StorageClassName, newC.GetStateStorageSize(), newC.StorageClassName) {
		return fmt.Errorf("%s.stateStorageSize and %s.storageClassName are immutable after creation: a StatefulSet's volumeClaimTemplates cannot be updated — remove the cosmosigner and re-add it to change its state storage", path, path)
	}
	return nil
}

// CosmosignerStateStorageEqual reports whether two (size, storageClassName) pairs describe the same
// PVC template. The size is compared as a parsed resource.Quantity so equivalent forms (e.g. "1024Mi"
// and "1Gi") are equal — the live StatefulSet may serialize a quantity in a different canonical form
// than the spec string, and a recorded lock adopted from live state must not then reject its own
// unchanged spec. The class is compared by pointer value so nil (cluster default) and an explicit ""
// stay distinct. Unparseable sizes fall back to string equality.
func CosmosignerStateStorageEqual(sizeA string, classA *string, sizeB string, classB *string) bool {
	if !ptr.Equal(classA, classB) {
		return false
	}
	qa, ea := resource.ParseQuantity(sizeA)
	qb, eb := resource.ParseQuantity(sizeB)
	if ea != nil || eb != nil {
		return sizeA == sizeB
	}
	return qa.Cmp(qb) == 0
}
