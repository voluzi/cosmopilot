package dataexporter

import (
	"archive/tar"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

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
