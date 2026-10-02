package cosmosigner

import "testing"

func TestRequireSupportedImage(t *testing.T) {
	for _, tc := range []struct {
		image   string
		refused bool
	}{
		{"ghcr.io/voluzi/cosmosigner:3.0.0", true},
		{"registry:5000/signer:v3.0.9@sha256:abc", true},
		{"signer:3.1.0-rc.1", true},
		{"signer:3.1.0", false}, {"signer:v3.1.0", false}, {"signer:4.0.0", false},
		{"signer:edge", false}, {"signer@sha256:abc", false},
	} {
		t.Run(tc.image, func(t *testing.T) {
			if err := RequireSupportedImage(tc.image); (err != nil) != tc.refused {
				t.Fatalf("RequireSupportedImage(%q) = %v", tc.image, err)
			}
		})
	}
}
