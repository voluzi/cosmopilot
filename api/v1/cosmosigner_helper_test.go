package v1

import "testing"

// TestCosmosignerGetImagePrecedence verifies the image resolution order: an explicit per-CR
// .spec.cosmosigner.image always wins; otherwise the operator-wide default (wired from the
// -cosmosigner-image/COSMOSIGNER_IMAGE flag) is used; only when that is also empty does the
// hardcoded DefaultCosmosignerImage constant apply.
func TestCosmosignerGetImagePrecedence(t *testing.T) {
	explicit := "explicit/image:v1"
	c := &Cosmosigner{Image: &explicit}
	if got := c.GetImage("operator/default:v2"); got != explicit {
		t.Fatalf("explicit image must win, got %q", got)
	}

	unset := &Cosmosigner{}
	if got := unset.GetImage("operator/default:v2"); got != "operator/default:v2" {
		t.Fatalf("operator default must be used when unset, got %q", got)
	}

	if got := unset.GetImage(""); got != DefaultCosmosignerImage {
		t.Fatalf("hardcoded default must be used when nothing else is configured, got %q", got)
	}
}
