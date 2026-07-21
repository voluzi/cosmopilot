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

// SignerUsesLocalValidatorKey reports whether the desired signer path consumes the targeted
// validator's local priv-key secret, either directly through software signing or by importing it
// into Vault. It is false for sentries and pre-provisioned Vault/GCP validator signers.
func (nodeSet *ChainNodeSet) SignerUsesLocalValidatorKey(s ResolvedSigner) bool {
	if !s.TargetsValidator() || s.Spec == nil {
		return false
	}
	if s.Spec.UsesSoftwareBackend() {
		return true
	}
	if !s.Spec.UsesVaultBackend() {
		return false
	}

	initializesGenesis := false
	if s.ValidatorGroup == ReservedValidatorGroupName {
		initializesGenesis = nodeSet.Spec.Validator != nil && nodeSet.Spec.Validator.Init != nil
	} else {
		for i := range nodeSet.Spec.Nodes {
			group := &nodeSet.Spec.Nodes[i]
			if group.Name == s.ValidatorGroup && group.Validator != nil {
				initializesGenesis = group.Validator.Init != nil
				break
			}
		}
	}
	return s.Spec.VaultUploadsGenerated(initializesGenesis)
}

// Digest fingerprints this signer for lifecycle status: identity, replica count, target-group set,
// and the validator group it serves. Including ValidatorGroup makes moving validator status between
// already-targeted groups a migration even when the target-name set itself is unchanged.
func (s ResolvedSigner) Digest() string {
	groups := append([]string(nil), s.TargetGroups...)
	sort.Strings(groups)
	preimage := fmt.Sprintf("%s\x00%d\x00%d\x00%s\x00%s",
		s.Identity(), s.Spec.GetReplicas(), len(groups), strings.Join(groups, "\x00"), s.ValidatorGroup)
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

// DesiredReplacementSigner identifies the desired signer that preserves a recorded signer's
// validator identity or sentry target set across a manifest placement move.
func (nodeSet *ChainNodeSet) DesiredReplacementSigner(desired []ResolvedSigner, st *CosmosignerStatus) (ResolvedSigner, bool) {
	if st == nil {
		return ResolvedSigner{}, false
	}
	if st.ServingGroup == "" && (st.ServingIdentity != "" || st.SigningDigest != "") {
		return ResolvedSigner{}, false
	}
	for _, s := range desired {
		if st.ServingGroup == "" {
			if st.AtEstablishment != nil && *st.AtEstablishment != "" {
				if nodeSet.GenesisSentryEstablishmentIdentity(s) == *st.AtEstablishment {
					return s, true
				}
				continue
			}
			if equalGroupMultiset(st.TargetGroups, s.TargetGroups) {
				return s, true
			}
			continue
		}
		if s.ValidatorGroup != st.ServingGroup {
			continue
		}
		switch {
		case st.ServingIdentity != "" && s.ValidatorTargetedIdentity() == st.ServingIdentity:
			return s, true
		case st.ServingIdentity == "" && st.SigningDigest == "":
			return s, true
		}
	}
	return ResolvedSigner{}, false
}

// equalGroupMultiset reports whether two target-group slices contain the same names with the same
// multiplicity, independent of order.
func equalGroupMultiset(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	counts := make(map[string]int, len(a))
	for _, g := range a {
		counts[g]++
	}
	for _, g := range b {
		if counts[g] == 0 {
			return false
		}
		counts[g]--
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

// CosmosignerResourceName returns the stable Kubernetes resource name for a resolved signer.
func (nodeSet *ChainNodeSet) CosmosignerResourceName(s ResolvedSigner) string {
	if st := nodeSet.GetCosmosignerStatus(s.Name); st != nil && st.ResourceName != "" {
		return st.ResourceName
	}
	return s.Name
}

// CosmosignerStatusResourceName returns the stable Kubernetes resource name recorded in status.
func CosmosignerStatusResourceName(st *CosmosignerStatus) string {
	if st != nil && st.ResourceName != "" {
		return st.ResourceName
	}
	if st == nil {
		return ""
	}
	return st.Name
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

// ServedValidatorHasMultipleInstances reports whether a removed signer served a validator group
// whose pods were redundant endpoints of one signer-held identity. Without the signer, those pods
// regain per-instance local/createValidator semantics and no longer represent one validator.
func (nodeSet *ChainNodeSet) ServedValidatorHasMultipleInstances(group string) bool {
	if group == "" || group == ReservedValidatorGroupName {
		return false
	}
	for i := range nodeSet.Spec.Nodes {
		g := &nodeSet.Spec.Nodes[i]
		if g.Name == group {
			return g.Validator != nil && g.GetInstances() > 1
		}
	}
	return false
}
