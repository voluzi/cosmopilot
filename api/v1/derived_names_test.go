package v1

import (
	"strings"
	"testing"
)

// repeat builds a base name of exactly n characters.
func repeat(n int) string { return strings.Repeat("a", n) }

func TestValidateDerivedNameLengthsBoundary(t *testing.T) {
	for _, tc := range []struct {
		name     string
		baseLen  int
		features nameFeatures
		wantErr  bool
		wantDesc string // substring expected in the error
	}{
		// Always-on internal Service (suffix 9): 54+9=63 passes, 55+9=64 fails.
		{"internal at 63", 54, nameFeatures{}, false, ""},
		{"internal over 63", 55, nameFeatures{}, true, "internal Service"},

		// cosmosigner discovery Service (suffix 15): binds before internal overflows.
		// base 48 -> 63 passes; base 49 -> 64 fails while internal (58) still fits.
		{"signer at 63", 48, nameFeatures{cosmosigner: true}, false, ""},
		{"signer over 63", 49, nameFeatures{cosmosigner: true}, true, "cosmosigner discovery Service"},

		// CosmoGuard upstream Service (suffix 12): longer than internal, so it binds.
		// base 51 -> 63 passes; base 52 -> 64 fails while internal (61) still fits.
		{"guard upstream at 63", 51, nameFeatures{cosmoguardGroup: true}, false, ""},
		{"guard upstream over 63", 52, nameFeatures{cosmoguardGroup: true}, true, "CosmoGuard upstream Service"},

		// CosmoGuard node peer (suffix 8) is shorter than the always-on internal floor, so the
		// internal Service is what actually overflows first for an over-long guarded node name.
		{"guard node at 63", 54, nameFeatures{cosmoguardNode: true}, false, ""},
		{"guard node over 63", 55, nameFeatures{cosmoguardNode: true}, true, "internal Service"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateDerivedNameLengths(repeat(tc.baseLen), "test name", tc.features)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("base of %d chars with %+v must be rejected", tc.baseLen, tc.features)
				}
				if !strings.Contains(err.Error(), tc.wantDesc) {
					t.Fatalf("error %q must name %q", err, tc.wantDesc)
				}
				return
			}
			if err != nil {
				t.Fatalf("base of %d chars with %+v must pass, got %v", tc.baseLen, tc.features, err)
			}
		})
	}
}

func TestValidateReservedResourceName(t *testing.T) {
	// Every reserved suffix is rejected on create.
	for _, suffix := range reservedNameSuffixes {
		name := "foo" + suffix
		if err := ValidateReservedResourceName(name, true); err == nil {
			t.Fatalf("metadata.name %q ending in reserved suffix %q must be rejected on create", name, suffix)
		}
		// Reserved only on create: an already-existing CR keeps updating.
		if err := ValidateReservedResourceName(name, false); err != nil {
			t.Fatalf("reserved name %q must stay valid on update, got %v", name, err)
		}
	}

	// Non-colliding names pass on create.
	for _, name := range []string{"foo", "foo-bar", "my-node", "foo-signer-0", "foo-cg-0"} {
		if err := ValidateReservedResourceName(name, true); err != nil {
			t.Fatalf("non-colliding name %q must be accepted on create, got %v", name, err)
		}
	}
}

func TestValidateReservedStatefulChildName(t *testing.T) {
	// StatefulSet pod / PVC child names are rejected on create.
	for _, name := range []string{"foo-signer-0", "foo-signer-12", "data-foo-signer-3", "foo-cg-0", "foo-cg-7", "bar-cg-10", "foo-seed-0", "foo-seed-9", "bar-seed-15"} {
		if err := ValidateReservedStatefulChildName(name, true); err == nil {
			t.Fatalf("child name %q must be reserved on create", name)
		}
		// Create-only: existing CRs keep updating.
		if err := ValidateReservedStatefulChildName(name, false); err != nil {
			t.Fatalf("child name %q must stay valid on update, got %v", name, err)
		}
	}

	// Non-canonical or unrelated names remain available on create.
	for _, name := range []string{"foo-cg-x", "foo-signer-canary", "foo-cg-00", "foo-signer-01", "foo-bar-0", "foo-cg", "foo-signer", "foo-seed", "foo-seed-x", "foo-seed-00"} {
		if err := ValidateReservedStatefulChildName(name, true); err != nil {
			t.Fatalf("name %q must remain available, got %v", name, err)
		}
	}
}
