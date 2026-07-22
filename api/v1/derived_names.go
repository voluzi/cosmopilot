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
var reservedNameSuffixes = []string{
	"-internal", "-p2p", "-grpc",
	"-tls", "-priv-key", "-account",
	"-upgrades", "-init-data",
	"-tmkms", "-tmkms-generate-identity", "-tmkms-vault-upload",
	"-cg", "-cg-peer", "-cg-cluster", "-cg-dashboard", "-cg-upstream",
	"-signer", "-signer-privval", "-signer-import", "-signer-pubkey",
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
