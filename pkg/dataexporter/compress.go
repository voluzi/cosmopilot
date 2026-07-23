package dataexporter

import (
	"archive/tar"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"

	"github.com/c2h5oh/datasize"
	"github.com/klauspost/compress/zstd"
	"github.com/klauspost/pgzip"
	"github.com/pierrec/lz4/v4"
)

// Compression identifies the format applied to a tar archive.
type Compression string

const (
	CompressionNone Compression = "none"
	CompressionGzip Compression = "gzip"
	CompressionZstd Compression = "zstd"
	CompressionLz4  Compression = "lz4"
)

const (
	// Covers tar block padding plus worst-case PAX path and link headers per entry.
	archiveEntryHeadroom = uint64(16 * 1024)
	archiveFixedHeadroom = uint64(1024 * 1024)
)

// ParseCompression validates and normalizes a compression name.
func ParseCompression(value string) (Compression, error) {
	compression := Compression(strings.ToLower(strings.TrimSpace(value)))
	switch compression {
	case CompressionNone, CompressionGzip, CompressionZstd, CompressionLz4:
		return compression, nil
	default:
		return "", fmt.Errorf("unsupported compression %q: expected one of none, gzip, zstd, lz4", value)
	}
}

// Extension returns the complete archive extension for the format.
func (c Compression) Extension() string {
	switch c {
	case CompressionNone:
		return ".tar"
	case CompressionZstd:
		return ".tar.zst"
	case CompressionLz4:
		return ".tar.lz4"
	default:
		return ".tar.gz"
	}
}

// ContentType returns the media type stored on the destination object.
func (c Compression) ContentType() string {
	switch c {
	case CompressionNone:
		return "application/x-tar"
	case CompressionZstd:
		return "application/zstd"
	case CompressionLz4:
		return "application/x-lz4"
	default:
		return "application/gzip"
	}
}

func writeTarball(dir string, out io.Writer, compression Compression) error {
	compressed, err := newCompressionWriter(out, compression)
	if err != nil {
		return err
	}
	tw := tar.NewWriter(compressed)

	err = filepath.Walk(dir, func(path string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}

		relPath, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		linkTarget := ""
		if info.Mode()&os.ModeSymlink != 0 {
			linkTarget, err = os.Readlink(path)
			if err != nil {
				return fmt.Errorf("read symlink %q: %w", path, err)
			}
		}
		hdr, err := tar.FileInfoHeader(info, linkTarget)
		if err != nil {
			return fmt.Errorf("create tar header for %q: %w", path, err)
		}
		hdr.Name = filepath.ToSlash(relPath)

		if err := tw.WriteHeader(hdr); err != nil {
			return fmt.Errorf("write tar header for %q: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			return nil
		}

		f, err := os.Open(path)
		if err != nil {
			return fmt.Errorf("open %q: %w", path, err)
		}
		_, copyErr := io.Copy(tw, f)
		closeErr := f.Close()
		if copyErr != nil {
			return fmt.Errorf("archive %q: %w", path, copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close %q: %w", path, closeErr)
		}
		return nil
	})
	if err != nil {
		_ = tw.Close()
		_ = compressed.Close()
		return err
	}
	if err := tw.Close(); err != nil {
		_ = compressed.Close()
		return fmt.Errorf("close tar writer: %w", err)
	}
	if err := compressed.Close(); err != nil {
		return fmt.Errorf("close %s writer: %w", compression, err)
	}
	return nil
}

func newCompressionWriter(out io.Writer, compression Compression) (io.WriteCloser, error) {
	switch compression {
	case CompressionNone:
		return nopWriteCloser{Writer: out}, nil
	case CompressionGzip:
		writer, err := pgzip.NewWriterLevel(out, pgzip.BestSpeed)
		if err != nil {
			return nil, fmt.Errorf("create gzip writer: %w", err)
		}
		return writer, nil
	case CompressionZstd:
		writer, err := zstd.NewWriter(
			out,
			zstd.WithEncoderLevel(zstd.EncoderLevelFromZstd(1)),
			zstd.WithEncoderConcurrency(0),
		)
		if err != nil {
			return nil, fmt.Errorf("create zstd writer: %w", err)
		}
		return writer, nil
	case CompressionLz4:
		writer := lz4.NewWriter(out)
		if err := writer.Apply(lz4.ConcurrencyOption(0)); err != nil {
			return nil, fmt.Errorf("configure lz4 writer: %w", err)
		}
		return writer, nil
	default:
		return nil, fmt.Errorf("unsupported compression %q", compression)
	}
}

type nopWriteCloser struct {
	io.Writer
}

func (nopWriteCloser) Close() error {
	return nil
}

func estimateArchiveUpperBound(dir string, sourceSize datasize.ByteSize, compression Compression) (datasize.ByteSize, error) {
	var entries uint64
	if err := filepath.Walk(dir, func(_ string, info fs.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() {
			entries++
		}
		return nil
	}); err != nil {
		return 0, fmt.Errorf("estimate archive overhead: %w", err)
	}
	sourceBytes := uint64(sourceSize)
	if sourceBytes > math.MaxUint64-archiveFixedHeadroom {
		return 0, fmt.Errorf("estimated archive size overflows uint64")
	}
	if entries > (math.MaxUint64-sourceBytes-archiveFixedHeadroom)/archiveEntryHeadroom {
		return 0, fmt.Errorf("estimated archive size overflows uint64")
	}
	estimate := sourceBytes + entries*archiveEntryHeadroom + archiveFixedHeadroom
	if compression != CompressionNone {
		codecHeadroom := estimate/100 + archiveFixedHeadroom
		if estimate > math.MaxUint64-codecHeadroom {
			return 0, fmt.Errorf("estimated compressed archive size overflows uint64")
		}
		estimate += codecHeadroom
	}
	return datasize.ByteSize(estimate), nil
}

func isArchiveObjectName(baseName, objectName string) bool {
	extensions := []string{
		CompressionNone.Extension(),
		CompressionGzip.Extension(),
		CompressionZstd.Extension(),
		CompressionLz4.Extension(),
	}
	for _, extension := range extensions {
		if objectName == baseName+extension {
			return true
		}
	}

	partPrefix := baseName + "-part-"
	if strings.HasPrefix(objectName, partPrefix) {
		remainder := strings.TrimPrefix(objectName, partPrefix)
		if isDecimal(remainder) {
			return true
		}
		for _, extension := range extensions {
			if strings.HasSuffix(remainder, extension) && isDecimal(strings.TrimSuffix(remainder, extension)) {
				return true
			}
			partNumber, temporarySuffix, found := strings.Cut(remainder, extension+"-temp-")
			if found && isDecimal(partNumber) && isCompositionTemporarySuffix(temporarySuffix) {
				return true
			}
		}
	}

	for _, extension := range extensions {
		tempPrefix := baseName + extension + "-temp-"
		if !strings.HasPrefix(objectName, tempPrefix) {
			continue
		}
		if isCompositionTemporarySuffix(strings.TrimPrefix(objectName, tempPrefix)) {
			return true
		}
	}
	return false
}

func isCompositionTemporarySuffix(value string) bool {
	parts := strings.Split(value, "-")
	return len(parts) == 2 && isDecimal(parts[0]) && isDecimal(parts[1])
}

func isDecimal(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}
