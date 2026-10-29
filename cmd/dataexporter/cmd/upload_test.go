package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/voluzi/cosmopilot/v5/pkg/dataexporter"
)

type uploadTestExporter struct {
	dataexporter.Exporter
	height       string
	legacyCalled bool
}

func (e *uploadTestExporter) Upload(_, _, _ string, opts ...dataexporter.UploadOption) error {
	e.legacyCalled = true
	var options dataexporter.UploadOptions
	for _, opt := range opts {
		opt(&options)
	}
	e.height = options.Height
	return nil
}

func TestUploadHeightFlagOverridesEnvironment(t *testing.T) {
	t.Setenv("DATA_HEIGHT", "150")
	original := exporter
	t.Cleanup(func() { exporter = original })
	for _, tc := range []struct {
		args   []string
		height string
	}{
		{[]string{"dir", "bucket", "snapshot"}, "150"},
		{[]string{"--height", "0", "dir", "bucket", "snapshot"}, "0"},
		{[]string{"--height=", "dir", "bucket", "snapshot"}, ""},
	} {
		recorder := &uploadTestExporter{}
		exporter = recorder
		command := newUploadCmd(dataexporter.DefaultChunkSize)
		command.SetArgs(tc.args)
		require.NoError(t, command.Execute())
		require.Equal(t, tc.height, recorder.height)
	}
}

type resultTestExporter struct {
	*uploadTestExporter
	result dataexporter.UploadResult
	err    error
	ctx    context.Context
}

func (e *resultTestExporter) UploadWithResult(ctx context.Context, _, _, _ string, _ ...dataexporter.UploadOption) (dataexporter.UploadResult, error) {
	e.ctx = ctx
	if err := ctx.Err(); err != nil {
		return dataexporter.UploadResult{}, err
	}
	return e.result, e.err
}

func successfulUploadResult() dataexporter.UploadResult {
	return dataexporter.UploadResult{Version: 1, Layout: "single", ObjectCount: 1, ObjectKey: "prefix/snapshot.tar.gz", SHA256: strings.Repeat("a", 64), SizeBytes: 150, Compression: dataexporter.CompressionGzip}
}

func TestUploadResultFile(t *testing.T) {
	original := exporter
	t.Cleanup(func() { exporter = original })
	for _, result := range []dataexporter.UploadResult{
		successfulUploadResult(),
		{Version: 1, Layout: "split", ObjectCount: 1, Compression: dataexporter.CompressionZstd, CleanupRequired: true},
	} {
		t.Run(result.Layout, func(t *testing.T) {
			recorder := &resultTestExporter{uploadTestExporter: &uploadTestExporter{}, result: result}
			exporter = recorder
			destination := filepath.Join(t.TempDir(), "result.json")
			command := newUploadCmd(dataexporter.DefaultChunkSize)
			command.SetArgs([]string{"dir", "bucket", "snapshot", "--result-file", destination})
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			require.NoError(t, command.ExecuteContext(ctx))
			require.Same(t, ctx, recorder.ctx)
			require.False(t, recorder.legacyCalled)
			data, err := os.ReadFile(destination)
			require.NoError(t, err)
			require.LessOrEqual(t, len(data), 3072)
			if result.Layout == "single" {
				require.JSONEq(t, `{"version":1,"layout":"single","objectCount":1,"compression":"gzip","objectKey":"prefix/snapshot.tar.gz","sha256":"`+strings.Repeat("a", 64)+`","sizeBytes":150,"cleanupRequired":false}`, string(data))
			} else {
				require.JSONEq(t, `{"version":1,"layout":"split","objectCount":1,"compression":"zstd","cleanupRequired":true}`, string(data))
			}
		})
	}
}

func TestUploadWithoutResultFlagUsesLegacyContract(t *testing.T) {
	original := exporter
	t.Cleanup(func() { exporter = original })
	recorder := &resultTestExporter{uploadTestExporter: &uploadTestExporter{}, err: errors.New("result API must not be called")}
	exporter = recorder
	destination := filepath.Join(t.TempDir(), "implicit-result.json")
	t.Setenv("RESULT_FILE", destination)
	command := newUploadCmd(dataexporter.DefaultChunkSize)
	command.SetArgs([]string{"dir", "bucket", "snapshot"})
	require.NoError(t, command.Execute())
	require.True(t, recorder.legacyCalled)
	require.Nil(t, recorder.ctx)
	_, err := os.Stat(destination)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestUploadResultFileRejectsInvalidResults(t *testing.T) {
	original := exporter
	t.Cleanup(func() { exporter = original })
	cases := []struct {
		name   string
		change func(*dataexporter.UploadResult)
	}{
		{"version", func(r *dataexporter.UploadResult) { r.Version = 2 }},
		{"layout", func(r *dataexporter.UploadResult) { r.Layout = "unknown" }},
		{"count", func(r *dataexporter.UploadResult) { r.ObjectCount = 0 }},
		{"single count", func(r *dataexporter.UploadResult) { r.ObjectCount = 2 }},
		{"key", func(r *dataexporter.UploadResult) { r.ObjectKey = "" }},
		{"invalid UTF8 key", func(r *dataexporter.UploadResult) { r.ObjectKey = string([]byte{255}) }},
		{"digest", func(r *dataexporter.UploadResult) { r.SHA256 = "123" }},
		{"uppercase digest", func(r *dataexporter.UploadResult) { r.SHA256 = strings.Repeat("A", 64) }},
		{"nonhex digest", func(r *dataexporter.UploadResult) { r.SHA256 = strings.Repeat("z", 64) }},
		{"size", func(r *dataexporter.UploadResult) { r.SizeBytes = 0 }},
		{"negative size", func(r *dataexporter.UploadResult) { r.SizeBytes = -1 }},
		{"compression", func(r *dataexporter.UploadResult) { r.Compression = " GZIP " }},
		{"unknown compression", func(r *dataexporter.UploadResult) { r.Compression = "brotli" }},
		{"split key", func(r *dataexporter.UploadResult) { r.Layout = "split"; r.SHA256 = ""; r.SizeBytes = 0 }},
		{"split digest", func(r *dataexporter.UploadResult) { r.Layout = "split"; r.ObjectKey = ""; r.SizeBytes = 0 }},
		{"split size", func(r *dataexporter.UploadResult) { r.Layout = "split"; r.ObjectKey = ""; r.SHA256 = "" }},
		{"oversized", func(r *dataexporter.UploadResult) { r.ObjectKey = strings.Repeat("é", 3072) }},
		{"escaped oversized", func(r *dataexporter.UploadResult) { r.ObjectKey = strings.Repeat("<", 1000) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := successfulUploadResult()
			tc.change(&result)
			exporter = &resultTestExporter{uploadTestExporter: &uploadTestExporter{}, result: result}
			destination := filepath.Join(t.TempDir(), "result.json")
			require.NoError(t, os.WriteFile(destination, []byte("unchanged"), 0600))
			command := newUploadCmd(dataexporter.DefaultChunkSize)
			command.SetArgs([]string{"dir", "bucket", "snapshot", "--result-file", destination})
			require.Error(t, command.Execute())
			data, err := os.ReadFile(destination)
			require.NoError(t, err)
			require.Equal(t, "unchanged", string(data))
		})
	}
}

func TestUploadResultFileFailure(t *testing.T) {
	original := exporter
	t.Cleanup(func() { exporter = original })
	for _, failure := range []string{"upload", "cancel", "write", "unsupported", "empty path"} {
		t.Run(failure, func(t *testing.T) {
			recorder := &resultTestExporter{uploadTestExporter: &uploadTestExporter{}, result: successfulUploadResult()}
			exporter = recorder
			destination := filepath.Join(t.TempDir(), "result.json")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch failure {
			case "upload":
				recorder.err = errors.New("upload rejected")
			case "cancel":
				cancel()
			case "write":
				destination = filepath.Join(t.TempDir(), "missing", "result.json")
			case "unsupported":
				exporter = &uploadTestExporter{}
			case "empty path":
				destination = ""
			}
			command := newUploadCmd(dataexporter.DefaultChunkSize)
			command.SetArgs([]string{"dir", "bucket", "snapshot", "--result-file", destination})
			err := command.ExecuteContext(ctx)
			require.Error(t, err)
			if failure == "cancel" {
				require.ErrorIs(t, err, context.Canceled)
			}
			if failure == "upload" {
				require.ErrorIs(t, err, recorder.err)
			}
			if destination != "" {
				_, err := os.Stat(destination)
				require.ErrorIs(t, err, os.ErrNotExist)
			}
		})
	}
}

func TestUploadResultFileSizeBoundary(t *testing.T) {
	original := exporter
	t.Cleanup(func() { exporter = original })
	for _, size := range []int{3072, 3073} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			result := successfulUploadResult()
			result.ObjectKey = "k"
			base, err := json.Marshal(result)
			require.NoError(t, err)
			result.ObjectKey = strings.Repeat("k", size-len(base)+1)
			exporter = &resultTestExporter{uploadTestExporter: &uploadTestExporter{}, result: result}
			destination := filepath.Join(t.TempDir(), "result.json")
			command := newUploadCmd(dataexporter.DefaultChunkSize)
			command.SetArgs([]string{"dir", "bucket", "snapshot", "--result-file", destination})
			err = command.Execute()
			if size > 3072 {
				require.ErrorContains(t, err, "3072")
				_, err := os.Stat(destination)
				require.ErrorIs(t, err, os.ErrNotExist)
			} else {
				require.NoError(t, err)
				data, err := os.ReadFile(destination)
				require.NoError(t, err)
				require.Len(t, data, size)
				var reported dataexporter.UploadResult
				require.NoError(t, json.Unmarshal(data, &reported))
				require.Equal(t, result.ObjectKey, reported.ObjectKey)
			}
		})
	}
}
