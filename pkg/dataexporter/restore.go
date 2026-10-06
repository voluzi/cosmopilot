package dataexporter

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"
)

// RestoreResult is also written to the Kubernetes container termination message.
type RestoreResult struct {
	Stage   string `json:"stage"`
	Message string `json:"message"`
	Height  string `json:"height,omitempty"`
}

// RestoreError identifies the failed installation stage without parsing logs.
type RestoreError struct {
	Stage string
	Err   error
}

func (e *RestoreError) Error() string { return e.Stage + ": " + e.Err.Error() }
func (e *RestoreError) Unwrap() error { return e.Err }

const restoreMarker = ".cosmopilot-restore-complete"

// Restore streams a single exported object into an empty data directory. Failed extraction leaves
// partial data untouched; recreate the volume before attempting installation again.
func Restore(ctx context.Context, exporter Exporter, dir, bucket, name, digest string) (RestoreResult, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return RestoreResult{}, &RestoreError{"target", err}
	}
	for _, entry := range entries {
		if entry.Name() == restoreMarker {
			data, err := os.ReadFile(filepath.Join(dir, restoreMarker))
			if err != nil {
				return RestoreResult{}, &RestoreError{"target", err}
			}
			result := RestoreResult{Stage: "complete"}
			if len(data) != 0 {
				if err := json.Unmarshal(data, &result); err != nil {
					return RestoreResult{}, &RestoreError{"target", err}
				}
			}
			return result, nil
		}
	}
	for _, entry := range entries {
		if entry.Name() != "lost+found" {
			return RestoreResult{}, &RestoreError{"target", errors.New("restore target is not empty; recreate the data volume to retry")}
		}
	}
	compression, err := restoreCompression(name)
	if err != nil {
		return RestoreResult{}, &RestoreError{"download", err}
	}
	object, metadata, err := exporter.Read(ctx, bucket, name)
	if err != nil {
		return RestoreResult{}, &RestoreError{"download", err}
	}
	defer func() { _ = object.Close() }()
	hash := sha256.New()
	stream := io.TeeReader(&restoreObjectReader{ctx: ctx, reader: object}, hash)
	decoded, closeDecoder, err := restoreDecoder(stream, compression)
	if err != nil {
		return RestoreResult{}, restoreExtractionError(err)
	}
	defer func() { _ = closeDecoder() }()
	if err := extractRestoreTar(dir, decoded); err != nil {
		return RestoreResult{}, err
	}
	// Tar EOF does not consume codec trailers or all bytes of the stored object.
	if _, err := io.Copy(io.Discard, decoded); err != nil {
		return RestoreResult{}, restoreExtractionError(err)
	}
	if err := closeDecoder(); err != nil {
		return RestoreResult{}, restoreExtractionError(err)
	}
	if _, err := io.Copy(io.Discard, stream); err != nil {
		return RestoreResult{}, &RestoreError{"download", err}
	}
	if err := object.Close(); err != nil {
		return RestoreResult{}, &RestoreError{"download", err}
	}
	if digest != "" && !strings.EqualFold(hex.EncodeToString(hash.Sum(nil)), digest) {
		return RestoreResult{}, &RestoreError{"verification", errors.New("SHA-256 mismatch")}
	}

	// File contents are synced as they are written; the marker must not outlive a crash that loses
	// their directory entries, so directories go first, then the marker, then the root again.
	result := RestoreResult{Stage: "complete", Height: metadata[HeightMetadataKey]}
	data, err := json.Marshal(result)
	if err != nil {
		return RestoreResult{}, &RestoreError{"target", err}
	}
	marker := filepath.Join(dir, restoreMarker)
	err = filepath.WalkDir(dir, func(p string, entry fs.DirEntry, err error) error {
		// The provisioner's lost+found may be unreadable and holds nothing restored.
		if p == filepath.Join(dir, "lost+found") {
			return fs.SkipDir
		}
		if err != nil || !entry.IsDir() {
			return err
		}
		return syncPath(p)
	})
	// The marker appears only complete: a crash mid-write must not leave an empty or partial one,
	// which a retry would read as a finished restore without a height.
	pending := marker + ".tmp"
	if err == nil {
		err = os.WriteFile(pending, data, 0600)
	}
	if err == nil {
		err = syncPath(pending)
	}
	if err == nil {
		err = os.Rename(pending, marker)
	}
	if err := errors.Join(err, syncPath(dir)); err != nil {
		// A marker that was not durably committed must not vouch for this volume on retry.
		_ = os.Remove(pending)
		_ = os.Remove(marker)
		return RestoreResult{}, &RestoreError{"target", err}
	}
	return result, nil
}

func syncPath(name string) error {
	f, err := os.Open(name)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}

type restoreObjectReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *restoreObjectReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, &RestoreError{"download", err}
	}
	n, err := r.reader.Read(p)
	if err != nil && err != io.EOF {
		return n, &RestoreError{"download", err}
	}
	return n, err
}

func restoreCompression(name string) (Compression, error) {
	for _, c := range []Compression{CompressionNone, CompressionGzip, CompressionZstd, CompressionLz4} {
		if !strings.HasSuffix(name, c.Extension()) {
			continue
		}
		return c, nil
	}
	return "", fmt.Errorf("unsupported archive extension: %q", name)
}

func restoreDecoder(r io.Reader, c Compression) (io.Reader, func() error, error) {
	switch c {
	case CompressionGzip:
		reader, err := gzip.NewReader(r)
		if err != nil {
			return nil, nil, err
		}
		return reader, reader.Close, nil
	case CompressionZstd:
		reader, err := zstd.NewReader(r, zstd.WithDecoderConcurrency(1))
		if err != nil {
			return nil, nil, err
		}
		return reader, func() error { reader.Close(); return nil }, nil
	case CompressionLz4:
		return lz4.NewReader(r), func() error { return nil }, nil
	default:
		return r, func() error { return nil }, nil
	}
}

func extractRestoreTar(dir string, reader io.Reader) error {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return &RestoreError{"target", err}
	}
	root := strings.TrimSuffix(dir, string(os.PathSeparator)) + string(os.PathSeparator)
	tail := &restoreTarReader{reader: reader}
	tr := tar.NewReader(tail)
	seen := map[string]bool{}
	for {
		beforeNext := tail.total
		hdr, err := tr.Next()
		if err == io.EOF {
			if tail.total-beforeNext < 1024 || tail.total%512 != 0 || !bytes.Equal(tail.tail[:], make([]byte, 1024)) {
				return restoreExtractionError(io.ErrUnexpectedEOF)
			}
			return nil
		}
		if err != nil {
			return restoreExtractionError(err)
		}
		name := path.Clean(hdr.Name)
		if path.IsAbs(hdr.Name) || strings.Contains(hdr.Name, "\\") || name == ".." || strings.HasPrefix(name, "../") {
			return restoreExtractionError(fmt.Errorf("unsafe archive path %q", hdr.Name))
		}
		for _, component := range strings.Split(hdr.Name, "/") {
			if component == ".." {
				return restoreExtractionError(fmt.Errorf("unsafe archive path %q", hdr.Name))
			}
		}
		if name == "." && hdr.Typeflag == tar.TypeDir {
			continue
		}
		// Archives of restored data may carry an old marker; only this restore can mark completion.
		if name == restoreMarker || name == restoreMarker+".tmp" {
			// Consume its data here so it cannot pass for the end-of-archive blocks below.
			if _, err := io.Copy(io.Discard, tr); err != nil {
				return restoreExtractionError(err)
			}
			continue
		}
		if strings.HasPrefix(name, restoreMarker+"/") || name == "lost+found" || strings.HasPrefix(name, "lost+found/") {
			return restoreExtractionError(fmt.Errorf("reserved archive path %q", hdr.Name))
		}
		if seen[name] {
			return restoreExtractionError(fmt.Errorf("duplicate archive path %q", name))
		}
		seen[name] = true
		target := filepath.Join(dir, filepath.FromSlash(name))
		if !strings.HasPrefix(target, root) {
			return restoreExtractionError(fmt.Errorf("unsafe archive path %q", hdr.Name))
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0755); err != nil {
				return restoreExtractionError(err)
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				return restoreExtractionError(err)
			}
			f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, os.FileMode(hdr.Mode)&0777|0600)
			if err != nil {
				return restoreExtractionError(err)
			}
			_, copyErr := io.Copy(f, tr)
			syncErr := f.Sync()
			closeErr := f.Close()
			if err := errors.Join(copyErr, syncErr, closeErr); err != nil {
				return restoreExtractionError(err)
			}
		default:
			return restoreExtractionError(fmt.Errorf("unsupported archive entry %q (type %d)", name, hdr.Typeflag))
		}
	}
}

func restoreExtractionError(err error) error {
	var restoreErr *RestoreError
	if errors.As(err, &restoreErr) {
		return restoreErr
	}
	return &RestoreError{"extraction", err}
}

// archive/tar accepts EOF at a header boundary without the two end-of-archive blocks.
// Require those blocks so an unverified, truncated tar cannot start a node.
type restoreTarReader struct {
	reader io.Reader
	tail   [1024]byte
	total  int64
}

func (r *restoreTarReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.total += int64(n)
	if n >= len(r.tail) {
		copy(r.tail[:], p[n-len(r.tail):n])
	} else {
		copy(r.tail[:], r.tail[n:])
		copy(r.tail[len(r.tail)-n:], p[:n])
	}
	return n, err
}
