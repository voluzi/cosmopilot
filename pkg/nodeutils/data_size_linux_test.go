//go:build linux

package nodeutils

import (
	"net/http"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestDataSizeUsesFilesystemFragmentSize(t *testing.T) {
	server := newDataSizeTestServer(t, "/data", func(_ string, stats *syscall.Statfs_t) error {
		stats.Blocks = 10
		stats.Bfree = 5
		stats.Bavail = 1
		stats.Bsize = 4096
		stats.Frsize = 1024
		return nil
	})

	response := requestDataSize(server)

	assert.Equal(t, http.StatusOK, response.Code)
	assert.Equal(t, "5120", response.Body.String())
}
