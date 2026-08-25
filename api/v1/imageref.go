package v1

import "strings"

// SplitImageRef splits a container image reference into its repository and its tag or digest.
// The returned reference is empty when the image carries neither.
//
// It correctly handles registries with a port (`registry:5000/repo:tag`), where a naive split on
// `:` would mistake the port for a tag, and digest references (`repo@sha256:...`).
func SplitImageRef(image string) (repository, reference string) {
	if i := strings.LastIndex(image, "@"); i >= 0 {
		return image[:i], image[i+1:]
	}

	i := strings.LastIndex(image, ":")
	// A colon that is followed by a path separator belongs to a registry port, not to a tag.
	if i < 0 || strings.Contains(image[i+1:], "/") {
		return image, ""
	}
	return image[:i], image[i+1:]
}

// ImageRefVersion returns the tag or digest of image, falling back to DefaultImageVersion when the
// image carries neither.
func ImageRefVersion(image string) string {
	if _, reference := SplitImageRef(image); reference != "" {
		return reference
	}
	return DefaultImageVersion
}

// ImageRefHasVersion reports whether image carries an explicit tag or digest.
func ImageRefHasVersion(image string) bool {
	_, reference := SplitImageRef(image)
	return reference != ""
}

// JoinImageRef composes a repository with a tag or a digest.
func JoinImageRef(repository, reference string) string {
	// A digest carries its algorithm, e.g. `sha256:abc...`, and attaches with `@`.
	if strings.Contains(reference, ":") {
		return repository + "@" + reference
	}
	return repository + ":" + reference
}
