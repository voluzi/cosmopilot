package dataexporter

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"cloud.google.com/go/storage"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/option"
)

type restoreTestExporter struct {
	Exporter
	data []byte
	err  error
}

func (e restoreTestExporter) Read(ctx context.Context, _, _ string) (io.ReadCloser, error) {
	if e.err != nil {
		return nil, e.err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return io.NopCloser(bytes.NewReader(e.data)), nil
}

func TestRestoreExporterArchiveFormats(t *testing.T) {
	for _, compression := range []Compression{CompressionNone, CompressionGzip, CompressionZstd, CompressionLz4} {
		t.Run(string(compression), func(t *testing.T) {
			source := t.TempDir()
			require.NoError(t, os.Mkdir(filepath.Join(source, "db"), 0755))
			require.NoError(t, os.WriteFile(filepath.Join(source, "db", "state"), []byte("block data"), 0600))
			require.NoError(t, os.WriteFile(filepath.Join(source, "priv_validator_state.json"), []byte(`{"height":"42"}`), 0600))
			var archive bytes.Buffer
			require.NoError(t, writeTarball(source, &archive, compression))
			digest := sha256.Sum256(archive.Bytes())
			for _, expected := range []string{"", hex.EncodeToString(digest[:])} {
				target := t.TempDir()
				require.NoError(t, Restore(t.Context(), restoreTestExporter{data: archive.Bytes()}, target, "bucket", "snapshot"+compression.Extension(), expected))
				data, err := os.ReadFile(filepath.Join(target, "db", "state"))
				require.NoError(t, err)
				require.Equal(t, "block data", string(data))
				state, err := os.ReadFile(filepath.Join(target, "priv_validator_state.json"))
				require.NoError(t, err)
				require.JSONEq(t, `{"height":"42"}`, string(state))
			}
		})
	}
}

func TestRestoreRejectsCorruption(t *testing.T) {
	source := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(source, "data"), []byte("original"), 0600))
	for _, compression := range []Compression{CompressionNone, CompressionGzip, CompressionZstd, CompressionLz4} {
		var archive bytes.Buffer
		require.NoError(t, writeTarball(source, &archive, compression))
		t.Run(string(compression)+" checksum", func(t *testing.T) {
			err := Restore(t.Context(), restoreTestExporter{data: archive.Bytes()}, t.TempDir(), "bucket", "backup"+compression.Extension(), strings.Repeat("0", 64))
			var stage *RestoreError
			require.ErrorAs(t, err, &stage)
			require.Equal(t, "verification", stage.Stage)
		})
		t.Run(string(compression)+" truncated", func(t *testing.T) {
			require.Error(t, Restore(t.Context(), restoreTestExporter{data: archive.Bytes()[:len(archive.Bytes())/2]}, t.TempDir(), "bucket", "backup"+compression.Extension(), ""))
		})
		if compression != CompressionNone {
			t.Run(string(compression)+" trailer", func(t *testing.T) {
				corrupt := bytes.Clone(archive.Bytes())
				corrupt[len(corrupt)-1] ^= 255
				require.Error(t, Restore(t.Context(), restoreTestExporter{data: corrupt}, t.TempDir(), "bucket", "backup"+compression.Extension(), ""))
			})
		}
	}
}

func TestRestoreVerifiesCompleteObject(t *testing.T) {
	var archive bytes.Buffer
	require.NoError(t, writeTarball(t.TempDir(), &archive, CompressionNone))
	stored := append(bytes.Clone(archive.Bytes()), []byte("trailing stored bytes")...)
	digest := sha256.Sum256(stored)
	require.NoError(t, Restore(t.Context(), restoreTestExporter{data: stored}, t.TempDir(), "bucket", "backup.tar", hex.EncodeToString(digest[:])))
	shortDigest := sha256.Sum256(archive.Bytes())
	require.Error(t, Restore(t.Context(), restoreTestExporter{data: stored}, t.TempDir(), "bucket", "backup.tar", hex.EncodeToString(shortDigest[:])))
}

func TestRestoreRejectsUnsafeArchiveEntries(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers []*tar.Header
	}{
		{"traversal", []*tar.Header{{Name: "../escape", Typeflag: tar.TypeReg}}},
		{"absolute", []*tar.Header{{Name: "/escape", Typeflag: tar.TypeReg}}},
		{"symlink", []*tar.Header{{Name: "link", Linkname: "../escape", Typeflag: tar.TypeSymlink}}},
		{"descending symlink", []*tar.Header{{Name: "link", Linkname: "file", Typeflag: tar.TypeSymlink}}},
		{"hardlink", []*tar.Header{{Name: "link", Linkname: "file", Typeflag: tar.TypeLink}}},
		{"device", []*tar.Header{{Name: "device", Typeflag: tar.TypeChar}}},
		{"duplicate", []*tar.Header{{Name: "file", Typeflag: tar.TypeReg}, {Name: "file", Typeflag: tar.TypeReg}}},
		{"conflicting parent", []*tar.Header{{Name: "file", Typeflag: tar.TypeReg}, {Name: "file/child", Typeflag: tar.TypeReg}}},
		{"restore marker child", []*tar.Header{{Name: ".cosmopilot-restore-complete/file", Typeflag: tar.TypeReg}}},
		{"provisioner directory", []*tar.Header{{Name: "lost+found/file", Typeflag: tar.TypeReg}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var archive bytes.Buffer
			tw := tar.NewWriter(&archive)
			for _, header := range tc.headers {
				require.NoError(t, tw.WriteHeader(header))
			}
			require.NoError(t, tw.Close())
			root := t.TempDir()
			target := filepath.Join(root, "target")
			require.NoError(t, os.Mkdir(target, 0755))
			require.Error(t, Restore(t.Context(), restoreTestExporter{data: archive.Bytes()}, target, "bucket", "snapshot.tar", ""))
			_, err := os.Stat(filepath.Join(root, "escape"))
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestRestoreSkipsArchivedCompletionMarker(t *testing.T) {
	for _, name := range []string{restoreMarker, "./" + restoreMarker} {
		t.Run(name, func(t *testing.T) {
			var archive bytes.Buffer
			tw := tar.NewWriter(&archive)
			for _, entry := range []struct{ name, data string }{{name, "old completion marker"}, {"db/state", "block data"}} {
				require.NoError(t, tw.WriteHeader(&tar.Header{Name: entry.name, Typeflag: tar.TypeReg, Mode: 0600, Size: int64(len(entry.data))}))
				_, err := tw.Write([]byte(entry.data))
				require.NoError(t, err)
			}
			require.NoError(t, tw.Close())
			for _, digest := range []string{"", strings.Repeat("0", 64)} {
				target := t.TempDir()
				err := Restore(t.Context(), restoreTestExporter{data: archive.Bytes()}, target, "bucket", "snapshot.tar", digest)
				if digest != "" {
					var stage *RestoreError
					require.ErrorAs(t, err, &stage)
					require.Equal(t, "verification", stage.Stage)
					_, err = os.Stat(filepath.Join(target, restoreMarker))
					require.ErrorIs(t, err, os.ErrNotExist)
					continue
				}
				require.NoError(t, err)
				data, err := os.ReadFile(filepath.Join(target, "db/state"))
				require.NoError(t, err)
				require.Equal(t, "block data", string(data))
				marker, err := os.ReadFile(filepath.Join(target, restoreMarker))
				require.NoError(t, err)
				require.Empty(t, marker)
			}
		})
	}
}

func TestRestoreRefusesExistingDataAndUnknownExtensions(t *testing.T) {
	target := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(target, "existing"), []byte("keep me"), 0600))
	require.Error(t, Restore(t.Context(), restoreTestExporter{}, target, "bucket", "backup.tar", ""))
	data, err := os.ReadFile(filepath.Join(target, "existing"))
	require.NoError(t, err)
	require.Equal(t, "keep me", string(data))
	for _, key := range []string{"backup-part-01", "backup.zip"} {
		require.Error(t, Restore(t.Context(), restoreTestExporter{}, t.TempDir(), "bucket", key, ""))
	}
}

func TestRestoreDownloadFailures(t *testing.T) {
	err := Restore(t.Context(), restoreTestExporter{err: io.ErrUnexpectedEOF}, t.TempDir(), "bucket", "snapshot.tar", "")
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, Restore(ctx, restoreTestExporter{}, t.TempDir(), "bucket", "snapshot.tar", ""), context.Canceled)
}

func TestRestoreProviderReads(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "test-access")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Contains(t, r.URL.Path, "bucket")
		if strings.Contains(r.URL.Path, "missing") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if strings.Contains(r.URL.Path, "denied") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		assert.Contains(t, r.URL.Path, "snapshot.tar")
		_, _ = fmt.Fprint(w, "stored bytes")
	}))
	defer server.Close()
	s3Exporter, err := NewS3Exporter(t.Context(), S3Config{Region: "us-east-1", Endpoint: server.URL, ForcePathStyle: true})
	require.NoError(t, err)
	gcsClient, err := storage.NewClient(t.Context(), option.WithEndpoint(server.URL), option.WithoutAuthentication())
	require.NoError(t, err)
	defer func() { require.NoError(t, gcsClient.Close()) }()
	for _, exporter := range []Exporter{s3Exporter, &GcsExporter{client: gcsClient}} {
		reader, err := exporter.Read(t.Context(), "bucket", "snapshot.tar")
		require.NoError(t, err)
		data, err := io.ReadAll(reader)
		require.NoError(t, err)
		require.NoError(t, reader.Close())
		require.Equal(t, "stored bytes", string(data))
		for _, key := range []string{"missing", "denied"} {
			_, err := exporter.Read(t.Context(), "bucket", key)
			require.Error(t, err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		_, err = exporter.Read(ctx, "bucket", "snapshot.tar")
		require.True(t, errors.Is(err, context.Canceled), "%v", err)
	}
}

func TestRestoreRequiresTarTerminatorAfterZeroFilledFiles(t *testing.T) {
	source := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(source, "zero-filled"), make([]byte, 2048), 0600))
	var archive bytes.Buffer
	require.NoError(t, writeTarball(source, &archive, CompressionNone))
	truncated := archive.Bytes()[:archive.Len()-1024]
	require.Error(t, Restore(t.Context(), restoreTestExporter{data: truncated}, t.TempDir(), "bucket", "snapshot.tar", ""))
}

func TestRestoreCompletedRetryDoesNotDownload(t *testing.T) {
	source := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(source, "state"), []byte("block data"), 0600))
	var archive bytes.Buffer
	require.NoError(t, writeTarball(source, &archive, CompressionGzip))
	digest := sha256.Sum256(archive.Bytes())
	target := t.TempDir()
	require.NoError(t, Restore(t.Context(), restoreTestExporter{data: archive.Bytes()}, target, "bucket", "snapshot.tar.gz", hex.EncodeToString(digest[:])))
	require.NoError(t, Restore(t.Context(), restoreTestExporter{err: errors.New("unexpected download")}, target, "bucket", "snapshot.tar.gz", hex.EncodeToString(digest[:])))
	data, err := os.ReadFile(filepath.Join(target, "state"))
	require.NoError(t, err)
	require.Equal(t, "block data", string(data))

	// Re-exported application data must not carry completion evidence into another volume.
	var exported bytes.Buffer
	require.NoError(t, writeTarball(target, &exported, CompressionNone))
	tr := tar.NewReader(&exported)
	for {
		header, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		require.NotEqual(t, ".cosmopilot-restore-complete", header.Name)
	}
}

func TestRestoreIncompleteRetryRefusesTarget(t *testing.T) {
	source := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(source, "state"), []byte("block data"), 0600))
	var archive bytes.Buffer
	require.NoError(t, writeTarball(source, &archive, CompressionNone))
	for _, failure := range []string{"extraction", "verification"} {
		t.Run(failure, func(t *testing.T) {
			stored := archive.Bytes()
			digest := strings.Repeat("0", 64)
			if failure == "extraction" {
				stored = stored[:len(stored)-1024]
				digest = ""
			}
			target := t.TempDir()
			require.Error(t, Restore(t.Context(), restoreTestExporter{data: stored}, target, "bucket", "snapshot.tar", digest))
			data, err := os.ReadFile(filepath.Join(target, "state"))
			require.NoError(t, err)
			require.Equal(t, "block data", string(data))
			err = Restore(t.Context(), restoreTestExporter{err: errors.New("unexpected download")}, target, "bucket", "snapshot.tar", "")
			var stage *RestoreError
			require.ErrorAs(t, err, &stage)
			require.Equal(t, "target", stage.Stage)
		})
	}
}

func TestRestoreIntoRelativeDirectory(t *testing.T) {
	source := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(source, "state"), []byte("block data"), 0600))
	var archive bytes.Buffer
	require.NoError(t, writeTarball(source, &archive, CompressionNone))
	t.Chdir(t.TempDir())
	require.NoError(t, Restore(t.Context(), restoreTestExporter{data: archive.Bytes()}, ".", "bucket", "snapshot.tar", ""))
	data, err := os.ReadFile("state")
	require.NoError(t, err)
	require.Equal(t, "block data", string(data))
}
