package nodeutils

import (
	"context"
	"crypto/rand"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDataSizeWithRealFilesystem(t *testing.T) {
	const fileSize = int64(32 << 20)
	parent := t.TempDir()
	dataPath := filepath.Join(parent, "data")
	sibling := filepath.Join(parent, "sibling")
	require.NoError(t, os.Mkdir(dataPath, 0o700))
	require.NoError(t, os.Mkdir(sibling, 0o700))
	writeRandomFile(t, filepath.Join(dataPath, "allocated"), fileSize)
	server := newDataSizeTestServer(t, dataPath, measureDataSize)
	assertDataSizeResponse(t, server, fileSize)
	writeRandomFile(t, filepath.Join(sibling, "unrelated"), fileSize)
	server.dataSizeSampler.cachedAt = time.Now().Add(-time.Hour)
	assertDataSizeResponse(t, server, fileSize)
	sparse, err := os.Create(filepath.Join(dataPath, "sparse"))
	require.NoError(t, err)
	require.NoError(t, sparse.Truncate(1<<20))
	require.NoError(t, sparse.Close())
	require.NoError(t, os.Symlink(filepath.Join(sibling, "unrelated"), filepath.Join(dataPath, "link")))
	server.dataSizeSampler.cachedAt = time.Now().Add(-time.Hour)
	assertDataSizeResponse(t, server, fileSize+(1<<20)+int64(len(filepath.Join(sibling, "unrelated"))))
}

func writeRandomFile(t *testing.T, path string, size int64) {
	t.Helper()
	f, err := os.Create(path)
	require.NoError(t, err)
	_, err = io.CopyN(f, rand.Reader, size)
	require.NoError(t, err)
	require.NoError(t, f.Sync())
	require.NoError(t, f.Close())
}

func TestUsedBytesFromBlocks(t *testing.T) {
	tests := []struct {
		name            string
		blocks, free    uint64
		block, fragment int64
		want            int64
		wantErr         bool
	}{
		{"fragment", 10, 5, 4096, 1024, 5120, false},
		{"fallback block", 10, 5, 4096, 0, 20480, false},
		{"zero block", 10, 5, 0, 0, 0, true},
		{"negative fragment", 10, 5, 4096, -1, 0, true},
		{"invalid counters", 10, 11, 4096, 0, 0, true},
		{"overflow", math.MaxUint64, 0, 4096, 2, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := usedBytesFromBlocks(tt.blocks, tt.free, tt.block, tt.fragment)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestDataSizeMissingRoot(t *testing.T) {
	server := newDataSizeTestServer(t, filepath.Join(t.TempDir(), "missing"), measureDataSize)
	response := requestDataSize(server)
	assert.Equal(t, http.StatusInternalServerError, response.Code)
}

func TestSameDataRootRejectsReplacementWithSymlink(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "data")
	replacement := filepath.Join(parent, "replacement")
	require.NoError(t, os.Mkdir(root, 0o700))
	require.NoError(t, os.Mkdir(replacement, 0o700))
	opened, err := os.Stat(root)
	require.NoError(t, err)
	assert.True(t, sameDataRoot(root, opened))
	require.NoError(t, os.Rename(root, filepath.Join(parent, "old")))
	require.NoError(t, os.Symlink(replacement, root))
	assert.False(t, sameDataRoot(root, opened))
}

func newDataSizeTestServer(t *testing.T, path string, measure dataSizeMeasureFunc) *NodeUtils {
	t.Helper()
	server := &NodeUtils{cfg: &Options{DataPath: path}, router: mux.NewRouter(), dataSizeSampler: newDataSizeSampler(path, measure)}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { server.dataSizeSampler.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	server.registerRoutes()
	return server
}

func requestDataSize(server *NodeUtils) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	server.router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/data_size", nil))
	return response
}

func assertDataSizeResponse(t *testing.T, server *NodeUtils, want int64) {
	t.Helper()
	response := requestDataSize(server)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	got, err := strconv.ParseInt(response.Body.String(), 10, 64)
	require.NoError(t, err)
	assert.Equal(t, want, got)
}
