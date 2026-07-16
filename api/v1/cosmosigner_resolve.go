package v1

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/voluzi/cosmopilot/v2/pkg/utils"
)

// HasLegacyPerInstanceCosmosignerStatus reports whether status still records the pre-group-identity
// signer naming scheme for group. Those entries must not be collapsed automatically on upgrade.
func (nodeSet *ChainNodeSet) HasLegacyPerInstanceCosmosignerStatus(group string) bool {
	prefix := fmt.Sprintf("%s-%s-", nodeSet.GetName(), group)
	const suffix = "-signer"
	modernSentries := map[string]struct{}{}
	for _, signer := range nodeSet.ResolveCosmosigners() {
		st := nodeSet.GetCosmosignerStatus(signer.Name)
		if !signer.TargetsValidator() && st != nil && st.ServingGroup == "" && st.AtEstablishment != nil {
			modernSentries[signer.Name] = struct{}{}
		}
	}
	for _, st := range nodeSet.Status.Cosmosigners {
		// A modern signer for group foo-1 looks like legacy instance 1 of foo. Validator signers persist
		// their exact group; sentries persist the at-establishment marker before they can reach teardown.
		if st.ServingGroup != "" && st.ServingGroup != group {
			continue
		}
		if st.ServingGroup == "" && st.AtEstablishment != nil && *st.AtEstablishment == "" {
			continue
		}
		if _, current := modernSentries[st.Name]; current {
			continue
		}
		if !strings.HasPrefix(st.Name, prefix) || !strings.HasSuffix(st.Name, suffix) {
			continue
		}
		instance := strings.TrimSuffix(strings.TrimPrefix(st.Name, prefix), suffix)
		if index, err := strconv.Atoi(instance); err == nil && index >= 0 {
			return true
		}
	}
	return false
}

// ResolvedSigner describes one managed cosmosigner deployment a ChainNodeSet should run. A
// ChainNodeSet can run many: the top-level .spec.cosmosigner (one signer over its target groups)
// plus one per .spec.nodes[].cosmosigner (a per-group signer). The whole controller/webhook operates
// on this struct so the single-signer assumptions live in one place: ResolveCosmosigners.
type ResolvedSigner struct {
	// Name is the signer's resource base name and the key of its status entry:
	// <nodeset>-signer | <nodeset>-<group>-signer.
	Name string

	// Spec is the signer configuration.
	Spec *Cosmosigner

	// TargetGroups are the node groups whose pods this signer dials.
	TargetGroups []string

	// ValidatorGroup is the validator group this signer serves (ReservedValidatorGroupName for the
	// legacy singleton), or "" for a sentry-mode signer over regular groups. A signer-targeted
	// validator group holds ONE consensus identity regardless of instances — the instances are
	// redundant signing endpoints of the same validator, whose key flow runs on instance 0.
	ValidatorGroup string

	// SoftwareKeySecret is the resolved priv-key secret the software backend mounts and the Vault
	// import source. "" for a sentry signer with no explicit key or a pre-provisioned Vault/GCP signer.
	SoftwareKeySecret string
}

// ResolveCosmosigners enumerates every managed signer this ChainNodeSet should run.
func (nodeSet *ChainNodeSet) ResolveCosmosigners() []ResolvedSigner {
	var signers []ResolvedSigner
	if c := nodeSet.Spec.Cosmosigner; c != nil {
		signers = append(signers, nodeSet.resolveTopLevelSigner(c))
	}
	for i := range nodeSet.Spec.Nodes {
		g := &nodeSet.Spec.Nodes[i]
		if g.Cosmosigner == nil {
			continue
		}
		signers = append(signers, nodeSet.resolveGroupSigners(g)...)
	}
	return signers
}

// resolveTopLevelSigner resolves the single top-level .spec.cosmosigner signer. It never expands per
// instance: the webhook rejects targeting a multi-instance validator group at the top level.
func (nodeSet *ChainNodeSet) resolveTopLevelSigner(c *Cosmosigner) ResolvedSigner {
	s := ResolvedSigner{Name: fmt.Sprintf("%s-signer", nodeSet.GetName()), Spec: c}

	if len(c.NodeGroups) == 0 {
		if nodeSet.Spec.Validator != nil {
			s.TargetGroups = []string{ReservedValidatorGroupName}
			s.ValidatorGroup = ReservedValidatorGroupName
		}
	} else {
		s.TargetGroups = append(s.TargetGroups, c.NodeGroups...)
		for _, name := range c.NodeGroups {
			if name == ReservedValidatorGroupName && nodeSet.Spec.Validator != nil {
				s.ValidatorGroup = ReservedValidatorGroupName
				continue
			}
			for i := range nodeSet.Spec.Nodes {
				if nodeSet.Spec.Nodes[i].Name == name && nodeSet.Spec.Nodes[i].Validator != nil {
					s.ValidatorGroup = name
				}
			}
		}
	}

	s.SoftwareKeySecret = nodeSet.resolveSignerKeySecret(c, s.ValidatorGroup)
	return s
}

// resolveGroupSigners resolves the signer for a group carrying .cosmosigner: one signer per group,
// holding a single consensus identity and dialing every instance pod. A multi-instance validator
// group is ONE validator with redundant signing endpoints, never N validators — multiple validators
// require multiple groups, each with its own signer.
func (nodeSet *ChainNodeSet) resolveGroupSigners(g *NodeGroupSpec) []ResolvedSigner {
	c := g.Cosmosigner

	if g.Validator == nil {
		// Sentry group: one signer over the whole group, out-of-band identity.
		return []ResolvedSigner{{
			Name:              fmt.Sprintf("%s-%s-signer", nodeSet.GetName(), g.Name),
			Spec:              c,
			TargetGroups:      []string{g.Name},
			SoftwareKeySecret: nodeSet.resolveSignerKeySecret(c, ""),
		}}
	}

	return []ResolvedSigner{{
		Name:              fmt.Sprintf("%s-%s-signer", nodeSet.GetName(), g.Name),
		Spec:              c,
		TargetGroups:      []string{g.Name},
		ValidatorGroup:    g.Name,
		SoftwareKeySecret: nodeSet.resolveSignerKeySecret(c, g.Name),
	}}
}

// resolveSignerKeySecret resolves the priv-key secret a signer's software backend mounts and its
// Vault import source: the targeted validator group's single identity key when a validator is
// targeted, otherwise the sentry signer's explicit privateKeySecret.
func (nodeSet *ChainNodeSet) resolveSignerKeySecret(c *Cosmosigner, validatorGroup string) string {
	if validatorGroup != "" {
		return nodeSet.validatorKeySecret(validatorGroup)
	}
	if c.UsesSoftwareBackend() && c.Backend.Software.PrivateKeySecret != nil {
		return *c.Backend.Software.PrivateKeySecret
	}
	return ""
}

// validatorKeySecret resolves the priv-key secret of a signer-targeted validator group: the legacy
// singleton's key, or the group's explicit or default key. A signer-targeted group holds ONE
// consensus identity regardless of its instance count, so an explicit privateKeySecret always names
// that identity; the default follows the generated instance-0 ChainNode name convention (instance
// 0's genesis/create-validator flow produces the key).
func (nodeSet *ChainNodeSet) validatorKeySecret(group string) string {
	if group == ReservedValidatorGroupName {
		if v := nodeSet.Spec.Validator; v != nil {
			if s := v.PrivateKeySecret; s != nil && *s != "" {
				return *s
			}
		}
		return fmt.Sprintf("%s-validator-priv-key", nodeSet.GetName())
	}
	for i := range nodeSet.Spec.Nodes {
		g := &nodeSet.Spec.Nodes[i]
		if g.Name != group || g.Validator == nil {
			continue
		}
		if s := g.Validator.PrivateKeySecret; s != nil && *s != "" {
			return *s
		}
		return fmt.Sprintf("%s-%s-0-priv-key", nodeSet.GetName(), group)
	}
	return ""
}

// Identity returns this signer's effective consensus-key fingerprint.
func (s ResolvedSigner) Identity() string {
	return s.Spec.effectiveSigningIdentity(s.SoftwareKeySecret)
}

// ValidatorTargetedIdentity is Identity() when the signer serves a validator, "" for sentry. Used
// for the at-establishment marker so a later sentry→validator retarget of the same key does not
// masquerade as the establishing configuration.
func (s ResolvedSigner) ValidatorTargetedIdentity() string {
	if s.ValidatorGroup == "" {
		return ""
	}
	return s.Identity()
}

// TargetsValidator reports whether this signer serves a validator (vs a sentry group).
func (s ResolvedSigner) TargetsValidator() bool {
	return s.ValidatorGroup != ""
}

// Digest fingerprints this signer for status persistence: identity, replica count and target-group
// set. The NUL-separated, length-prefixed preimage stays unambiguous regardless of group names.
func (s ResolvedSigner) Digest() string {
	groups := append([]string(nil), s.TargetGroups...)
	sort.Strings(groups)
	preimage := fmt.Sprintf("%s\x00%d\x00%d\x00%s",
		s.Identity(), s.Spec.GetReplicas(), len(groups), strings.Join(groups, "\x00"))
	return utils.Sha256(preimage)
}

// signerFieldPath returns the spec field path a resolved signer originates from, for error messages:
// ".spec.cosmosigner" for the top-level signer, ".spec.nodes[i].cosmosigner" for a per-group one.
func (nodeSet *ChainNodeSet) signerFieldPath(s ResolvedSigner) string {
	if s.Spec == nodeSet.Spec.Cosmosigner {
		return ".spec.cosmosigner"
	}
	if len(s.TargetGroups) > 0 {
		for i := range nodeSet.Spec.Nodes {
			if nodeSet.Spec.Nodes[i].Name == s.TargetGroups[0] {
				return fmt.Sprintf(".spec.nodes[%d].cosmosigner", i)
			}
		}
	}
	return ".spec.cosmosigner"
}

// equalGroupSet reports whether two target-group slices contain the same set of names.
func equalGroupSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	set := make(map[string]struct{}, len(a))
	for _, g := range a {
		set[g] = struct{}{}
	}
	for _, g := range b {
		if _, ok := set[g]; !ok {
			return false
		}
	}
	return true
}

// GetCosmosignerStatus returns the status entry for a signer name, or nil.
func (nodeSet *ChainNodeSet) GetCosmosignerStatus(name string) *CosmosignerStatus {
	for i := range nodeSet.Status.Cosmosigners {
		if nodeSet.Status.Cosmosigners[i].Name == name {
			return &nodeSet.Status.Cosmosigners[i]
		}
	}
	return nil
}

// EnsureCosmosignerStatus returns the (created if absent) status entry for a signer name.
func (nodeSet *ChainNodeSet) EnsureCosmosignerStatus(name string) *CosmosignerStatus {
	if s := nodeSet.GetCosmosignerStatus(name); s != nil {
		return s
	}
	nodeSet.Status.Cosmosigners = append(nodeSet.Status.Cosmosigners, CosmosignerStatus{Name: name})
	return &nodeSet.Status.Cosmosigners[len(nodeSet.Status.Cosmosigners)-1]
}

// RemoveCosmosignerStatus drops the status entry for a signer name.
func (nodeSet *ChainNodeSet) RemoveCosmosignerStatus(name string) {
	out := nodeSet.Status.Cosmosigners[:0]
	for _, s := range nodeSet.Status.Cosmosigners {
		if s.Name != name {
			out = append(out, s)
		}
	}
	nodeSet.Status.Cosmosigners = out
}

// validatorConfigForGroup returns the validator config of a group (the legacy singleton via
// ReservedValidatorGroupName, or a named group), or nil when the group has no validator.
func (nodeSet *ChainNodeSet) validatorConfigForGroup(group string) *NodeSetValidatorConfig {
	if group == ReservedValidatorGroupName {
		return nodeSet.Spec.Validator
	}
	for i := range nodeSet.Spec.Nodes {
		g := &nodeSet.Spec.Nodes[i]
		if g.Name == group {
			return g.Validator
		}
	}
	return nil
}

// validatorEffectiveIdentity returns the effective consensus-key fingerprint of a validator group's
// single signing identity: the identity of whatever managed signer serves it, or — when none does —
// the validator's own local/tmKMS path (its representative instance 0). Empty when the group has no
// validator, has zero instances, or no longer exists.
func (nodeSet *ChainNodeSet) validatorEffectiveIdentity(group string) string {
	if group == "" {
		return ""
	}
	if id, ok := nodeSet.groupSignerIdentity(group); ok {
		return id
	}
	cfg := nodeSet.validatorConfigForGroup(group)
	if cfg == nil {
		return ""
	}
	if group != ReservedValidatorGroupName {
		for i := range nodeSet.Spec.Nodes {
			if nodeSet.Spec.Nodes[i].Name == group && nodeSet.Spec.Nodes[i].GetInstances() == 0 {
				return ""
			}
		}
	}
	return nodeSet.validatorGroupSigningIdentity(group, cfg)
}

// signerSameKeyMigration reports whether, on an established chain, the validator this signer serves
// already resolves the signer's key through its previous (old-revision) signing path — e.g. a
// tmKMS→cosmosigner migration on the same Vault key. Equivalent keys compare equal via the
// normalized signing identity, so such a migration is not misread as a key change. Only meaningful
// for a validator-targeted signer.
func (nodeSet *ChainNodeSet) signerSameKeyMigration(old *ChainNodeSet, s ResolvedSigner) bool {
	if old == nil || old.Status.ChainID == "" || s.ValidatorGroup == "" {
		return false
	}
	oldIdentity := old.validatorEffectiveIdentity(s.ValidatorGroup)
	return oldIdentity != "" && oldIdentity == s.Identity()
}

// signerDigestRecordedMatches reports whether the status already records this exact signer's digest,
// proving the current identity was rolled out and served. Used on the no-webhook path (old == nil) as
// the same-key waiver: a newly added signer (no entry, or a different digest) stays subject to the
// registers-genesis rule.
func (nodeSet *ChainNodeSet) signerDigestRecordedMatches(s ResolvedSigner) bool {
	st := nodeSet.GetCosmosignerStatus(s.Name)
	return st != nil && st.SigningDigest != "" && st.SigningDigest == s.Digest()
}

// ServedValidatorResolvesIdentity reports whether the specific validator a removed signer served
// still resolves `identity` through its OWN signing path — the condition under which dropping the
// signer keeps the on-chain key signing. An unknown/removed validator resolves nothing.
func (nodeSet *ChainNodeSet) ServedValidatorResolvesIdentity(group string, identity string) bool {
	if identity == "" || group == "" {
		return false
	}
	if group == ReservedValidatorGroupName {
		return nodeSet.Spec.Validator != nil &&
			nodeSet.validatorGroupSigningIdentity(ReservedValidatorGroupName, nodeSet.Spec.Validator) == identity
	}
	for i := range nodeSet.Spec.Nodes {
		g := &nodeSet.Spec.Nodes[i]
		if g.Name != group {
			continue
		}
		if g.Validator == nil || g.GetInstances() == 0 {
			return false
		}
		return nodeSet.validatorGroupSigningIdentity(group, g.Validator) == identity
	}
	return false
}
