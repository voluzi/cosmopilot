// Package dataexporter provides interfaces and implementations for exporting
// blockchain node data to cloud storage providers.
package dataexporter

import (
	"context"
	"crypto/sha256"
	"fmt"
	"hash"
	"io"
	"unicode/utf8"
)

// HeightMetadataKey identifies the archived dataset height in S3 and GCS object metadata.
const HeightMetadataKey = "cosmopilot-height"

// UploadResult describes acknowledged archive output, without attesting source provenance.
type UploadResult struct {
	Version         int         `json:"version"`
	Layout          string      `json:"layout"`
	ObjectCount     int         `json:"objectCount"`
	Compression     Compression `json:"compression"`
	ObjectKey       string      `json:"objectKey,omitempty"`
	SHA256          string      `json:"sha256,omitempty"`
	SizeBytes       int64       `json:"sizeBytes,omitempty"`
	CleanupRequired bool        `json:"cleanupRequired"`
}

// Validate checks the version-1 archive result contract.
func (r UploadResult) Validate() error {
	if r.Version != 1 {
		return fmt.Errorf("unsupported upload result version %d", r.Version)
	}
	if r.ObjectCount < 1 {
		return fmt.Errorf("upload result object count must be positive")
	}
	compression, err := ParseCompression(string(r.Compression))
	if err != nil || compression != r.Compression {
		return fmt.Errorf("invalid upload result compression")
	}
	switch r.Layout {
	case "single":
		if r.ObjectCount != 1 || r.ObjectKey == "" || !utf8.ValidString(r.ObjectKey) || r.SizeBytes <= 0 {
			return fmt.Errorf("single upload result requires one exact object key and positive size")
		}
		if len(r.SHA256) != 64 {
			return fmt.Errorf("upload result requires a lowercase SHA-256")
		}
		for _, c := range r.SHA256 {
			if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
				return fmt.Errorf("upload result requires a lowercase SHA-256")
			}
		}
	case "split":
		if r.ObjectKey != "" || r.SHA256 != "" || r.SizeBytes != 0 {
			return fmt.Errorf("split upload result cannot identify a single archive object")
		}
	default:
		return fmt.Errorf("unsupported upload result layout %q", r.Layout)
	}
	return nil
}

type uploadDigest struct {
	hash      hash.Hash
	sizeBytes int64
}

func newUploadDigest() *uploadDigest { return &uploadDigest{hash: sha256.New()} }

func (d *uploadDigest) Write(p []byte) (int, error) {
	n, err := d.hash.Write(p)
	d.sizeBytes += int64(n)
	return n, err
}

func (d *uploadDigest) result(compression Compression, layout string, count int, key string) UploadResult {
	result := UploadResult{Version: 1, Layout: layout, ObjectCount: count, Compression: compression}
	if layout == "single" {
		result.ObjectKey = key
		result.SHA256 = fmt.Sprintf("%x", d.hash.Sum(nil))
		result.SizeBytes = d.sizeBytes
	}
	return result
}

// Provider identifies a cloud storage provider.
type Provider string

// Exporter provides methods to upload, read and delete data snapshots from cloud storage.
type Exporter interface {
	// Read opens one exact object key; the caller must close the returned stream.
	Read(ctx context.Context, bucket, name string) (io.ReadCloser, map[string]string, error)

	// Provider returns the cloud storage provider type.
	Provider() Provider

	// Upload uploads a directory as a tar archive to the specified bucket.
	// The name parameter specifies the object name in the bucket.
	// Options can be provided to customize the upload behavior.
	Upload(dir, bucket, name string, opts ...UploadOption) error

	// Delete removes an object from the specified bucket.
	// Options can be provided to customize the delete behavior.
	Delete(bucket, name string, opts ...DeleteOption) error
}

// FromProvider creates an Exporter for the specified provider.
// Returns an error if the provider is not supported.
func FromProvider(p Provider) (Exporter, error) {
	switch p {
	case GCS:
		return NewGcsExporter()
	case S3:
		return NewS3Exporter(context.Background(), S3Config{})
	default:
		return nil, fmt.Errorf("unsupported provider: %s", p)
	}
}
