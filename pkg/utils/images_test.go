package utils

import "testing"

func TestSplitImageRef(t *testing.T) {
	for _, tc := range []struct {
		name       string
		image      string
		repository string
		reference  string
	}{
		{"tag", "alloranetwork/allora-chain:v0.17.1", "alloranetwork/allora-chain", "v0.17.1"},
		{"registry and tag", "registry.ops.allora.run/bryn-test/allorad:986-test", "registry.ops.allora.run/bryn-test/allorad", "986-test"},
		{"no tag", "alloranetwork/allora-chain", "alloranetwork/allora-chain", ""},
		{"registry port and tag", "registry.local:5000/foo/allorad:v1.2.3", "registry.local:5000/foo/allorad", "v1.2.3"},
		{"registry port without tag", "registry.local:5000/foo/allorad", "registry.local:5000/foo/allorad", ""},
		{"digest", "alloranetwork/allora-chain@sha256:abc123", "alloranetwork/allora-chain", "sha256:abc123"},
		{"registry port and digest", "registry.local:5000/foo@sha256:abc123", "registry.local:5000/foo", "sha256:abc123"},
		{"empty", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repository, reference := SplitImageRef(tc.image)
			if repository != tc.repository || reference != tc.reference {
				t.Fatalf("SplitImageRef(%q) = (%q, %q), want (%q, %q)",
					tc.image, repository, reference, tc.repository, tc.reference)
			}
		})
	}
}

func TestImageHasVersion(t *testing.T) {
	for _, tc := range []struct {
		image string
		want  bool
	}{
		{"alloranetwork/allora-chain:v0.17.1", true},
		{"alloranetwork/allora-chain@sha256:abc123", true},
		{"alloranetwork/allora-chain", false},
		// A registry port must not be mistaken for a tag.
		{"registry.local:5000/foo/allorad", false},
	} {
		t.Run(tc.image, func(t *testing.T) {
			if got := ImageHasVersion(tc.image); got != tc.want {
				t.Fatalf("ImageHasVersion(%q) = %v, want %v", tc.image, got, tc.want)
			}
		})
	}
}

func TestJoinImageRef(t *testing.T) {
	if got := JoinImageRef("repo/app", "v1.0.0"); got != "repo/app:v1.0.0" {
		t.Fatalf("JoinImageRef tag = %q", got)
	}
	if got := JoinImageRef("repo/app", "sha256:abc123"); got != "repo/app@sha256:abc123" {
		t.Fatalf("JoinImageRef digest = %q", got)
	}
}

func TestSplitImageRefRoundTrips(t *testing.T) {
	for _, image := range []string{
		"alloranetwork/allora-chain:v0.17.1",
		"registry.local:5000/foo/allorad:v1.2.3",
		"alloranetwork/allora-chain@sha256:abc123",
		"registry.local:5000/foo@sha256:abc123",
	} {
		t.Run(image, func(t *testing.T) {
			repository, reference := SplitImageRef(image)
			if got := JoinImageRef(repository, reference); got != image {
				t.Fatalf("round trip of %q = %q", image, got)
			}
		})
	}
}
