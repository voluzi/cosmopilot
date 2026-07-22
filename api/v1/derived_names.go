package v1

import (
	"fmt"
	"strconv"
	"strings"
)

// nameFeatures declares which optional subsystems a CR enables, so validateDerivedNameLengths only
// checks the derived names that will actually be created.
type nameFeatures struct {
	cosmosigner     bool
	cosmoguardNode  bool
	cosmoguardGroup bool
}

// validateDerivedNameLengths checks every DNS-label(63)-bound resource name a base name derives and
// returns an error naming the first one that overflows. Only the label-bound names matter: most
// Kubernetes objects allow a 253-char subdomain, but Services, StatefulSet pods, PDBs and pod
// hostnames are capped at 63, and the main Service is named exactly `base`, so `base` itself is
// already 63-bound and any suffixed 63-bound name dominates it. `subject` is the tail hint naming
// what to shorten ("ChainNode name" / "ChainNodeSet or node-group name").
func validateDerivedNameLengths(base, subject string, f nameFeatures) error {
	for _, c := range []struct {
		on     bool
		suffix string
		desc   string
	}{
		{true, "-internal", "internal Service"}, // always created; 63-bound floor
		{f.cosmosigner, "-signer-privval", "cosmosigner discovery Service"},
		{f.cosmoguardNode, "-cg-peer", "CosmoGuard peer Service"},
		{f.cosmoguardGroup, "-cg-upstream", "CosmoGuard upstream Service"},
	} {
		if !c.on {
			continue
		}
		if n := base + c.suffix; len(n) > 63 {
			return fmt.Errorf("the %s name %q (%d chars) exceeds the 63-character limit: shorten the %s", c.desc, n, len(n), subject)
		}
	}
	return nil
}

// reservedNameSuffixes are the tokens cosmopilot appends to a CR's metadata.name to derive resource
// names operator-wide. A CR whose own name ends in one collides with a resource that a *different* CR
// derives (e.g. a CR named `foo-cg` collides with the CosmoGuard Service that `foo` derives), so the
// two owners fight over the shared name. Every entry maps to a real derived resource:
//   - node/group Services: -internal, -p2p, -grpc
//   - secrets: -tls, -priv-key, -account, -cg-cluster
//   - configmaps / one-shot pods: -upgrades, -init-data
//   - tmkms: -tmkms, -tmkms-generate-identity, -tmkms-vault-upload
//   - CosmoGuard: -cg, -cg-peer, -cg-cluster, -cg-dashboard, -cg-upstream
//   - cosmosigner: -signer, -signer-privval, -signer-import, -signer-pubkey
//   - cosmoseed: -seed, -seed-headless
var reservedNameSuffixes = []string{
	"-internal", "-p2p", "-grpc",
	"-tls", "-priv-key", "-account",
	"-upgrades", "-init-data",
	"-tmkms", "-tmkms-generate-identity", "-tmkms-vault-upload",
	"-cg", "-cg-peer", "-cg-cluster", "-cg-dashboard", "-cg-upstream",
	"-signer", "-signer-privval", "-signer-import", "-signer-pubkey",
	"-seed", "-seed-headless",
}

// reservedNodeSetNameSuffixes is the narrow subset of reservedNameSuffixes that a ChainNodeSet
// bare-materializes — resources named exactly "<set><suffix>", with no node-group/ordinal segment
// in between. Only two families qualify: the top-level .spec.cosmosigner ("<set>-signer" and its
// -privval/-import/-pubkey material) and cosmoseed's route/headless Services ("<set>-seed",
// "<set>-seed-headless"). Every other operator-derived name inserts a "-<group>"/"-validator"/
// ordinal segment, so it can never collide with the ChainNodeSet's own name. Reserving the full
// operator set on the ChainNodeSet name was over-broad; concrete-derived-name checks
// (validateGeneratedNameLengths, validateServiceNameCollisions, validateGroupChildReservedNames)
// cover the real collisions a node group can cause.
var reservedNodeSetNameSuffixes = []string{
	"-signer", "-signer-privval", "-signer-import", "-signer-pubkey",
	"-seed", "-seed-headless",
}

// ValidateReservedResourceName rejects creating a ChainNode/ChainNodeSet whose name ends in a suffix
// cosmopilot derives from another CR's name (see reservedNameSuffixes). Only enforced on create so
// pre-existing CRs with such names keep updating; the reconcilers' ownership guards remain the
// backstop for them.
func ValidateReservedResourceName(name string, isCreate bool) error {
	if !isCreate {
		return nil
	}
	for _, suffix := range reservedNameSuffixes {
		if strings.HasSuffix(name, suffix) {
			return fmt.Errorf("metadata.name %q is reserved: the %q suffix collides with a resource name cosmopilot derives from another resource; choose a different name", name, suffix)
		}
	}
	return nil
}

// ValidateReservedNodeSetName rejects creating a ChainNodeSet whose name ends in a suffix the
// ChainNodeSet itself bare-materializes (see reservedNodeSetNameSuffixes). Unlike a ChainNode — which
// can collide with any operator-derived suffix and so uses the full reservedNameSuffixes set via
// ValidateReservedResourceName — a ChainNodeSet only ever creates a resource named exactly its own
// name for the signer and cosmoseed families; every other derived name carries an extra segment.
// Only enforced on create, so pre-existing ChainNodeSets keep updating.
func ValidateReservedNodeSetName(name string, isCreate bool) error {
	if !isCreate {
		return nil
	}
	for _, suffix := range reservedNodeSetNameSuffixes {
		if strings.HasSuffix(name, suffix) {
			return fmt.Errorf("metadata.name %q is reserved: the %q suffix collides with a resource name cosmopilot derives for this ChainNodeSet's signer or seed nodes; choose a different name", name, suffix)
		}
	}
	return nil
}

// ValidateReservedStatefulChildName rejects a CR name that exactly matches a StatefulSet pod (or
// raft-state PVC) name cosmopilot derives: the cosmosigner children `<x>-signer-<n>` and the
// CosmoGuard children `<x>-cg-<n>`. Such a name is not caught by ValidateReservedResourceName because
// it ends in an ordinal, not a reserved suffix. Only enforced on create.
func ValidateReservedStatefulChildName(name string, isCreate bool) error {
	if !isCreate {
		return nil
	}
	lastDash := strings.LastIndexByte(name, '-')
	if lastDash < 0 {
		return nil
	}
	base := name[:lastDash]
	if !strings.HasSuffix(base, "-signer") && !strings.HasSuffix(base, "-cg") {
		return nil
	}
	ordinal := name[lastDash+1:]
	n, err := strconv.ParseInt(ordinal, 10, 32)
	if err != nil || n < 0 || strconv.FormatInt(n, 10) != ordinal {
		return nil
	}
	return fmt.Errorf("metadata.name %q is reserved: it collides with a StatefulSet pod or PVC name cosmopilot derives (cosmosigner %q / CosmoGuard %q); choose a different name", name, "-signer-<n>", "-cg-<n>")
}

// ValidateReservedResourceNameNoWebhook applies the full-operator-set reserved-name rule
// (ValidateReservedResourceName) on the ChainNode no-webhook reconcile path, where create cannot be
// distinguished from update. It enforces only while the object has never been successfully reconciled
// (isEstablished == false, i.e. empty status), so a pre-existing legacy resource with a reserved name
// keeps updating while a NEW no-webhook resource with a reserved name is rejected before the
// controllers start fighting over derived names.
func ValidateReservedResourceNameNoWebhook(name string, isEstablished bool) error {
	if isEstablished {
		return nil
	}
	return ValidateReservedResourceName(name, true)
}

// ValidateReservedNodeSetNameNoWebhook is the ChainNodeSet counterpart of
// ValidateReservedResourceNameNoWebhook: it applies the narrow ChainNodeSet reserved-name rule
// (ValidateReservedNodeSetName) on the no-webhook reconcile path, enforcing only while the object has
// never been reconciled (isEstablished == false).
func ValidateReservedNodeSetNameNoWebhook(name string, isEstablished bool) error {
	if isEstablished {
		return nil
	}
	return ValidateReservedNodeSetName(name, true)
}
