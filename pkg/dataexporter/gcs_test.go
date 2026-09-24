package dataexporter

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"cloud.google.com/go/storage"
	"github.com/c2h5oh/datasize"
	"github.com/stretchr/testify/require"
	"google.golang.org/api/option"
)

type countingReader struct {
	r    io.Reader
	read atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read.Add(int64(n))
	return n, err
}

// TestGcsUploadChunksStopsAfterFirstPartFailure fails the first part upload while a second one is in
// flight: the in-flight upload must be aborted and the archive must not be read any further.
func TestGcsUploadChunksStopsAfterFirstPartFailure(t *testing.T) {
	var requests, aborted atomic.Int64
	secondArrived := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			// Fail the first part only once a second part is in flight.
			select {
			case <-secondArrived:
			case <-time.After(5 * time.Second):
			}
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"error":{"code":403,"message":"forbidden"}}`))
			return
		}
		// The server only notices a client abort once the request body has been read.
		_, _ = io.Copy(io.Discard, r.Body)
		if requests.Load() == 2 {
			close(secondArrived)
		}
		select {
		case <-r.Context().Done():
			aborted.Add(1)
		case <-time.After(5 * time.Second):
			w.WriteHeader(http.StatusOK)
		}
	}))
	defer server.Close()

	client, err := storage.NewClient(context.Background(),
		option.WithEndpoint(server.URL+"/storage/v1/"), option.WithoutAuthentication())
	require.NoError(t, err)
	gcs := &GcsExporter{client: client}

	const chunkSize = 64 * datasize.KB
	const totalSize = 200 * chunkSize
	reader := &countingReader{r: bytes.NewReader(make([]byte, totalSize.Bytes()))}
	opts := defaultUploadOptions()
	opts.ChunkSize = chunkSize
	opts.BufferSize = chunkSize
	opts.ConcurrentJobs = 2

	start := time.Now()
	err = gcs.uploadChunks(context.Background(), reader, "bucket", "object", totalSize, totalSize, opts)
	require.ErrorContains(t, err, "upload failed")
	require.Less(t, time.Since(start), 4*time.Second, "the in-flight upload must not run to completion")
	// The server handler records the abort asynchronously, after the client has already returned.
	require.Eventually(t, func() bool { return aborted.Load() == 1 }, 3*time.Second, 10*time.Millisecond,
		"the in-flight part upload must be cancelled")
	require.Less(t, reader.read.Load(), int64(totalSize.Bytes())/2, "the archive must not be fully consumed after a part fails")
}
