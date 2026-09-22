package nodeutils

import (
	"crypto/rand"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/gorilla/mux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDataSizeReturnsFilesystemUsedBytes(t *testing.T) {
	const (
		blocks     = uint64(2_000_000_000)
		freeBlocks = uint64(500_000_000)
		blockSize  = 4096
		want       = "6144000000000"
	)

	server := newDataSizeTestServer(t, "/data", func(path string, stats *syscall.Statfs_t) error {
		assert.Equal(t, "/data", path)
		stats.Blocks = blocks
		stats.Bfree = freeBlocks
		stats.Bavail = 17
		stats.Bsize = blockSize
		return nil
	})

	response := requestDataSize(server)

	assert.Equal(t, http.StatusOK, response.Code)
	assert.Equal(t, want, response.Body.String())
}

func TestDataSizeReturnsInternalServerErrorWhenStatfsFails(t *testing.T) {
	server := newDataSizeTestServer(t, "/data", func(string, *syscall.Statfs_t) error {
		return errors.New("statfs failed")
	})

	response := requestDataSize(server)

	assert.Equal(t, http.StatusInternalServerError, response.Code)
}

func TestDataSizeReturnsInternalServerErrorForMissingPath(t *testing.T) {
	server := newDataSizeTestServer(t, filepath.Join(t.TempDir(), "missing"), syscall.Statfs)

	response := requestDataSize(server)

	assert.Equal(t, http.StatusInternalServerError, response.Code)
}

func TestDataSizeRejectsInvalidStats(t *testing.T) {
	tests := []struct {
		name  string
		stats syscall.Statfs_t
	}{
		{
			name:  "zero block size",
			stats: syscall.Statfs_t{Blocks: 10, Bfree: 5, Bsize: 0},
		},
		{
			name:  "free blocks exceed total blocks",
			stats: syscall.Statfs_t{Blocks: 10, Bfree: 11, Bsize: 4096},
		},
		{
			name:  "used bytes overflow int64",
			stats: syscall.Statfs_t{Blocks: math.MaxUint64, Bfree: 0, Bsize: 2},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := newDataSizeTestServer(t, "/data", func(_ string, stats *syscall.Statfs_t) error {
				*stats = tt.stats
				return nil
			})

			response := requestDataSize(server)

			assert.Equal(t, http.StatusInternalServerError, response.Code)
		})
	}
}

func TestDataSizeSerializesConcurrentMeasurements(t *testing.T) {
	var (
		calls     atomic.Int32
		active    atomic.Int32
		maxActive atomic.Int32
	)
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	statfs := func(_ string, stats *syscall.Statfs_t) error {
		current := active.Add(1)
		defer active.Add(-1)
		for {
			maximum := maxActive.Load()
			if current <= maximum || maxActive.CompareAndSwap(maximum, current) {
				break
			}
		}
		if calls.Add(1) == 1 {
			close(firstEntered)
			<-releaseFirst
		}
		stats.Bsize = 4096
		return nil
	}
	server := newDataSizeTestServer(t, "/data", statfs)

	var requests sync.WaitGroup
	requests.Add(2)
	go func() {
		defer requests.Done()
		requestDataSize(server)
	}()
	<-firstEntered

	secondStarted := make(chan struct{})
	go func() {
		defer requests.Done()
		close(secondStarted)
		requestDataSize(server)
	}()
	<-secondStarted

	if server.dataSizeMu.TryLock() {
		server.dataSizeMu.Unlock()
		t.Fatal("data-size mutex is not held during measurement")
	}
	close(releaseFirst)
	requests.Wait()

	assert.Equal(t, int32(2), calls.Load())
	assert.Equal(t, int32(1), maxActive.Load())
}

func TestDataSizeTracksRealFilesystemAllocation(t *testing.T) {
	const (
		fileSize  = int64(32 << 20)
		tolerance = int64(1 << 20)
	)
	dataPath := t.TempDir()
	server := newDataSizeTestServer(t, dataPath, syscall.Statfs)

	baselineDirect := directFilesystemUsedBytes(t, dataPath)
	baselineResponse := requestDataSize(server)
	require.Equal(t, http.StatusOK, baselineResponse.Code)
	baseline, err := strconv.ParseInt(baselineResponse.Body.String(), 10, 64)
	require.NoError(t, err)
	assert.InDelta(t, baselineDirect, baseline, float64(tolerance))

	file, err := os.Create(filepath.Join(dataPath, "allocated-data"))
	require.NoError(t, err)
	_, err = io.CopyN(file, rand.Reader, fileSize)
	require.NoError(t, err)
	require.NoError(t, file.Sync())
	require.NoError(t, file.Close())
	fileInfo, err := os.Stat(filepath.Join(dataPath, "allocated-data"))
	require.NoError(t, err)
	assert.Equal(t, fileSize, fileInfo.Size())
	fileStats, ok := fileInfo.Sys().(*syscall.Stat_t)
	require.True(t, ok)
	assert.InDelta(t, fileSize, fileStats.Blocks*512, float64(tolerance))

	afterDirect := directFilesystemUsedBytes(t, dataPath)
	afterResponse := requestDataSize(server)
	require.Equal(t, http.StatusOK, afterResponse.Code)
	after, err := strconv.ParseInt(afterResponse.Body.String(), 10, 64)
	require.NoError(t, err)
	assert.InDelta(t, afterDirect, after, float64(tolerance))
	assert.InDelta(t, afterDirect-baselineDirect, after-baseline, float64(tolerance))
}

func newDataSizeTestServer(t *testing.T, dataPath string, statfs statfsFunc) *NodeUtils {
	t.Helper()
	server := &NodeUtils{
		cfg:    &Options{DataPath: dataPath},
		router: mux.NewRouter(),
		statfs: statfs,
	}
	server.registerRoutes()
	return server
}

func requestDataSize(server *NodeUtils) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	server.router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/data_size", nil))
	return response
}

func directFilesystemUsedBytes(t *testing.T, path string) int64 {
	t.Helper()
	var stats syscall.Statfs_t
	require.NoError(t, syscall.Statfs(path, &stats))
	return int64((uint64(stats.Blocks) - uint64(stats.Bfree)) * uint64(stats.Bsize))
}
