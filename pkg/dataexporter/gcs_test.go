package dataexporter

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

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

// TestGcsUploadChunksStopsAfterFirstPartFailure fails every part upload and checks the exporter stops
// reading the archive instead of consuming and uploading all of it.
func TestGcsUploadChunksStopsAfterFirstPartFailure(t *testing.T) {
	var uploads atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		uploads.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"code":403,"message":"forbidden"}}`))
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

	err = gcs.uploadChunks(context.Background(), reader, "bucket", "object", totalSize, totalSize, opts)
	require.ErrorContains(t, err, "upload failed")
	require.Less(t, reader.read.Load(), int64(totalSize.Bytes())/2, "the archive must not be fully consumed after a part fails")
	require.Positive(t, uploads.Load(), "the fake server must receive the part uploads")
	require.Less(t, uploads.Load(), int64(20))
}
