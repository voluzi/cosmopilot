package dataexporter

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/klauspost/compress/zstd"
	"github.com/pierrec/lz4/v4"
)

func TestWriteTarballCompressionFormats(t *testing.T) {
	testContent := map[string]string{
		"data/application.db": "application state",
		"config/config.toml":  "moniker = \"snapshot-test\"",
	}

	tests := []struct {
		name        string
		compression Compression
		extension   string
		open        func(io.Reader) (io.Reader, io.Closer, error)
	}{
		{
			name:        "uncompressed tar",
			compression: CompressionNone,
			extension:   ".tar",
			open: func(r io.Reader) (io.Reader, io.Closer, error) {
				return r, io.NopCloser(bytes.NewReader(nil)), nil
			},
		},
		{
			name:        "gzip",
			compression: CompressionGzip,
			extension:   ".tar.gz",
			open: func(r io.Reader) (io.Reader, io.Closer, error) {
				reader, err := gzip.NewReader(r)
				return reader, reader, err
			},
		},
		{
			name:        "zstd",
			compression: CompressionZstd,
			extension:   ".tar.zst",
			open: func(r io.Reader) (io.Reader, io.Closer, error) {
				reader, err := zstd.NewReader(r)
				if err != nil {
					return nil, nil, err
				}
				return reader, reader.IOReadCloser(), nil
			},
		},
		{
			name:        "lz4",
			compression: CompressionLz4,
			extension:   ".tar.lz4",
			open: func(r io.Reader) (io.Reader, io.Closer, error) {
				return lz4.NewReader(r), io.NopCloser(bytes.NewReader(nil)), nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, content := range testContent {
				path := filepath.Join(dir, name)
				if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
					t.Fatalf("create test directory: %v", err)
				}
				if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
					t.Fatalf("write test file: %v", err)
				}
			}

			var output bytes.Buffer
			if err := writeTarball(dir, &output, tt.compression); err != nil {
				t.Fatalf("writeTarball() error = %v", err)
			}
			if got := tt.compression.Extension(); got != tt.extension {
				t.Fatalf("Extension() = %q, want %q", got, tt.extension)
			}

			archiveReader, closer, err := tt.open(bytes.NewReader(output.Bytes()))
			if err != nil {
				t.Fatalf("open compressed archive: %v", err)
			}
			defer closer.Close()

			got := readTarFiles(t, archiveReader)
			for name, content := range testContent {
				if got[name] != content {
					t.Errorf("archive file %q = %q, want %q", name, got[name], content)
				}
			}
		})
	}
}

func TestWriteTarballEmptyDirectory(t *testing.T) {
	var output bytes.Buffer
	if err := writeTarball(t.TempDir(), &output, CompressionGzip); err != nil {
		t.Fatalf("writeTarball() error = %v", err)
	}
	reader, err := gzip.NewReader(bytes.NewReader(output.Bytes()))
	if err != nil {
		t.Fatalf("open gzip archive: %v", err)
	}
	defer reader.Close()
	if files := readTarFiles(t, reader); len(files) != 0 {
		t.Fatalf("empty archive contains %d files", len(files))
	}
}

func TestWriteTarballMissingDirectory(t *testing.T) {
	var output bytes.Buffer
	if err := writeTarball(filepath.Join(t.TempDir(), "missing"), &output, CompressionGzip); err == nil {
		t.Fatal("writeTarball() expected an error")
	}
}

func TestParseCompression(t *testing.T) {
	for _, value := range []Compression{CompressionNone, CompressionGzip, CompressionZstd, CompressionLz4} {
		t.Run(string(value), func(t *testing.T) {
			got, err := ParseCompression(string(value))
			if err != nil {
				t.Fatalf("ParseCompression() error = %v", err)
			}
			if got != value {
				t.Fatalf("ParseCompression() = %q, want %q", got, value)
			}
		})
	}

	if _, err := ParseCompression("brotli"); err == nil {
		t.Fatal("ParseCompression() expected an error for unsupported compression")
	}
}

func readTarFiles(t *testing.T, reader io.Reader) map[string]string {
	t.Helper()

	files := make(map[string]string)
	tarReader := tar.NewReader(reader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			return files
		}
		if err != nil {
			t.Fatalf("read tar header: %v", err)
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			continue
		}
		content, err := io.ReadAll(tarReader)
		if err != nil {
			t.Fatalf("read %s: %v", header.Name, err)
		}
		files[header.Name] = string(content)
	}
}

func TestCompressionContentType(t *testing.T) {
	tests := map[Compression]string{
		CompressionNone: "application/x-tar",
		CompressionGzip: "application/gzip",
		CompressionZstd: "application/zstd",
		CompressionLz4:  "application/x-lz4",
	}
	for compression, want := range tests {
		t.Run(fmt.Sprintf("%s", compression), func(t *testing.T) {
			if got := compression.ContentType(); got != want {
				t.Fatalf("ContentType() = %q, want %q", got, want)
			}
		})
	}
}

func TestEstimateArchiveUpperBoundIncludesArchiveOverhead(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.db"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	estimate, err := estimateArchiveUpperBound(dir, 1, CompressionNone)
	if err != nil {
		t.Fatalf("estimateArchiveUpperBound() error = %v", err)
	}
	if estimate <= 1 {
		t.Fatalf("estimateArchiveUpperBound() = %d, want archive overhead above source size", estimate)
	}

	compressedEstimate, err := estimateArchiveUpperBound(dir, 1, CompressionZstd)
	if err != nil {
		t.Fatalf("estimateArchiveUpperBound() error = %v", err)
	}
	if compressedEstimate <= estimate {
		t.Fatalf("compressed estimate = %d, want codec headroom above tar estimate %d", compressedEstimate, estimate)
	}
}

func TestIsArchiveObjectName(t *testing.T) {
	tests := []struct {
		name       string
		objectName string
		want       bool
	}{
		{name: "single archive", objectName: "snapshot.tar.zst", want: true},
		{name: "raw upload part", objectName: "snapshot-part-00000000", want: true},
		{name: "final split archive", objectName: "snapshot-part-00000000.tar.lz4", want: true},
		{name: "single archive composition temporary", objectName: "snapshot.tar.gz-temp-0-1", want: true},
		{name: "split archive composition temporary", objectName: "snapshot-part-0.tar.gz-temp-0-1", want: true},
		{name: "similar snapshot", objectName: "snapshot-old.tar.gz", want: false},
		{name: "archive backup", objectName: "snapshot.tar.gz.backup", want: false},
		{name: "invalid part", objectName: "snapshot-part-invalid.tar.lz4", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isArchiveObjectName("snapshot", tt.objectName); got != tt.want {
				t.Fatalf("isArchiveObjectName() = %t, want %t", got, tt.want)
			}
		})
	}
}
