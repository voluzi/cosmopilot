package dataexporter

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
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

func TestGcsUploadHeightMetadata(t *testing.T) {
	for _, tc := range []struct {
		name, chunk, part, limit string
	}{
		{"single", "1MB", "1MB", "5TB"},
		{"multiple composition rounds", "1KB", "1MB", "5TB"},
		{"split composition", "1KB", "2KB", "1B"},
		{"split copy", "4KB", "4KB", "1B"},
	} {
		for _, height := range []string{"", "0", "150"} {
			t.Run(tc.name+"/height="+height, func(t *testing.T) {
				var mu sync.Mutex
				objects := map[string]*storageObject{}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					mu.Lock()
					defer mu.Unlock()
					w.Header().Set("Content-Type", "application/json")
					var object storageObject
					switch {
					case r.Method == http.MethodDelete:
						delete(objects, strings.Split(r.URL.Path, "/o/")[1])
						w.WriteHeader(http.StatusNoContent)
						return
					case strings.HasSuffix(r.URL.Path, "/compose"):
						var request struct {
							Destination storageObject `json:"destination"`
							Sources     []struct {
								Name string `json:"name"`
							} `json:"sourceObjects"`
						}
						if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
							t.Error(err)
							w.WriteHeader(400)
							return
						}
						object = request.Destination
						object.Name = strings.TrimSuffix(strings.Split(r.URL.Path, "/o/")[1], "/compose")
						for _, src := range request.Sources {
							object.Data = append(object.Data, objects[src.Name].Data...)
						}
					case strings.Contains(r.URL.Path, "/rewriteTo/"):
						if err := json.NewDecoder(r.Body).Decode(&object); err != nil {
							t.Error(err)
							w.WriteHeader(400)
							return
						}
						parts := strings.Split(r.URL.Path, "/o/")
						object.Name = parts[2]
						object.Data = objects[strings.Split(parts[1], "/rewriteTo/")[0]].Data
					default:
						_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
						if err != nil {
							t.Error(err)
							w.WriteHeader(400)
							return
						}
						reader := multipart.NewReader(r.Body, params["boundary"])
						metadata, err := reader.NextPart()
						if err != nil {
							t.Error(err)
							w.WriteHeader(400)
							return
						}
						if err := json.NewDecoder(metadata).Decode(&object); err != nil {
							t.Error(err)
							w.WriteHeader(400)
							return
						}
						data, err := reader.NextPart()
						if err != nil {
							t.Error(err)
							w.WriteHeader(400)
							return
						}
						object.Data, err = io.ReadAll(data)
						if err != nil {
							t.Error(err)
							w.WriteHeader(400)
							return
						}
					}
					object.Generation = "7"
					object.Size = strconv.Itoa(len(object.Data))
					objects[object.Name] = &object
					if strings.Contains(r.URL.Path, "/rewriteTo/") {
						_ = json.NewEncoder(w).Encode(map[string]any{"done": true, "resource": object})
					} else {
						_ = json.NewEncoder(w).Encode(object)
					}
				}))
				defer server.Close()
				client, err := storage.NewClient(t.Context(), option.WithEndpoint(server.URL+"/storage/v1/"), option.WithoutAuthentication())
				require.NoError(t, err)
				defer client.Close()
				dir := t.TempDir()
				payload := bytes.Repeat([]byte("block data"), 4000)
				require.NoError(t, os.WriteFile(filepath.Join(dir, "state"), payload, 0600))
				exporter := &GcsExporter{client: client}
				require.NoError(t, exporter.Upload(dir, "bucket", "snapshot", WithCompression(CompressionNone), WithHeight(height), WithChunkSize(tc.chunk), WithPartSize(tc.part), WithSizeLimit(tc.limit)))
				var names []string
				for name := range objects {
					names = append(names, name)
				}
				sort.Strings(names)
				require.NotEmpty(t, names)
				var archive []byte
				for _, name := range names {
					require.True(t, strings.HasSuffix(name, ".tar"), name)
					value, exists := objects[name].Metadata["cosmopilot-height"]
					require.Equal(t, height, value)
					require.Equal(t, height != "", exists)
					archive = append(archive, objects[name].Data...)
				}
				require.Equal(t, string(payload), readTarFiles(t, bytes.NewReader(archive))["state"])
			})
		}
	}
}

type storageObject struct {
	Name       string            `json:"name"`
	Metadata   map[string]string `json:"metadata"`
	Generation string            `json:"generation"`
	Size       string            `json:"size"`
	Data       []byte            `json:"-"`
}
