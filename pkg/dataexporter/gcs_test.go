package dataexporter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
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
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, err = gcs.uploadChunks(ctx, cancel, reader, "bucket", "object", totalSize, totalSize, opts)
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
		{"single copy", "1MB", "1MB", "1B"},
	} {
		for _, height := range []string{"", "0", "150"} {
			t.Run(tc.name+"/height="+height, func(t *testing.T) {
				fixture := newGCSFixture(t)
				dir := t.TempDir()
				payload := bytes.Repeat([]byte("block data"), 4000)
				require.NoError(t, os.WriteFile(filepath.Join(dir, "state"), payload, 0600))
				exporter := fixture.exporter
				require.NoError(t, exporter.Upload(dir, "bucket", "snapshot", WithCompression(CompressionNone), WithHeight(height), WithChunkSize(tc.chunk), WithPartSize(tc.part), WithSizeLimit(tc.limit)))
				objects := fixture.objects
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

type gcsFixture struct {
	exporter *GcsExporter
	mu       sync.Mutex
	objects  map[string]*storageObject
	fail     func(*http.Request, string) bool
	before   func(*http.Request)
	loseAck  string
	composed []string
	copied   []string
}

func newGCSFixture(t *testing.T) *gcsFixture {
	t.Helper()
	fixture := &gcsFixture{objects: map[string]*storageObject{}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fixture.before != nil {
			fixture.before(r)
		}
		fixture.mu.Lock()
		defer fixture.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fail := func(code int, message string) {
			w.WriteHeader(code)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": message}})
		}
		name := ""
		if _, tail, ok := strings.Cut(r.URL.Path, "/o/"); ok {
			name = strings.TrimSuffix(tail, "/compose")
		}
		if fixture.fail != nil && fixture.fail(r, name) {
			fail(http.StatusForbidden, "injected provider failure: "+name)
			return
		}
		var object storageObject
		switch {
		case r.Method == http.MethodDelete:
			delete(fixture.objects, name)
			w.WriteHeader(http.StatusNoContent)
			return
		case strings.HasSuffix(r.URL.Path, "/compose"):
			fixture.composed = append(fixture.composed, name)
			var request struct {
				Destination storageObject `json:"destination"`
				Sources     []struct {
					Name string `json:"name"`
				} `json:"sourceObjects"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				fail(400, err.Error())
				return
			}
			object = request.Destination
			object.Name = name
			for _, source := range request.Sources {
				stored, ok := fixture.objects[source.Name]
				if !ok {
					fail(400, "missing compose source "+source.Name)
					return
				}
				object.Data = append(object.Data, stored.Data...)
			}
		case strings.Contains(r.URL.Path, "/rewriteTo/"):
			if err := json.NewDecoder(r.Body).Decode(&object); err != nil {
				fail(400, err.Error())
				return
			}
			parts := strings.Split(r.URL.Path, "/o/")
			object.Name = parts[2]
			fixture.copied = append(fixture.copied, object.Name)
			stored, ok := fixture.objects[strings.Split(parts[1], "/rewriteTo/")[0]]
			if !ok {
				fail(400, "missing copy source")
				return
			}
			object.Data = append([]byte(nil), stored.Data...)
		default:
			_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err != nil {
				fail(400, err.Error())
				return
			}
			reader := multipart.NewReader(r.Body, params["boundary"])
			metadata, err := reader.NextPart()
			if err != nil {
				fail(400, err.Error())
				return
			}
			if err := json.NewDecoder(metadata).Decode(&object); err != nil {
				fail(400, err.Error())
				return
			}
			data, err := reader.NextPart()
			if err != nil {
				fail(400, err.Error())
				return
			}
			object.Data, err = io.ReadAll(data)
			if err != nil {
				fail(400, err.Error())
				return
			}
		}
		object.Generation = "7"
		object.Size = strconv.Itoa(len(object.Data))
		fixture.objects[object.Name] = &object
		if fixture.loseAck == object.Name {
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = connection.Close()
			return
		}
		if strings.Contains(r.URL.Path, "/rewriteTo/") {
			_ = json.NewEncoder(w).Encode(map[string]any{"done": true, "resource": object})
		} else {
			_ = json.NewEncoder(w).Encode(object)
		}
	}))
	t.Cleanup(server.Close)
	client, err := storage.NewClient(t.Context(), option.WithEndpoint(server.URL+"/storage/v1/"), option.WithoutAuthentication())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	fixture.exporter = &GcsExporter{client: client}
	return fixture
}

func TestGcsUploadRejectsIntermediateComposeFailure(t *testing.T) {
	fixture := newGCSFixture(t)
	fixture.fail = func(r *http.Request, name string) bool {
		return r.Method == http.MethodPost && name == "snapshot.tar-temp-0-0"
	}
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "state"), bytes.Repeat([]byte("block data"), 4000), 0600))
	err := fixture.exporter.Upload(dir, "bucket", "snapshot", WithCompression(CompressionNone), WithChunkSize("1KB"))
	require.ErrorContains(t, err, "injected provider failure: snapshot.tar-temp-0-0")
	require.NotContains(t, fixture.composed, "snapshot.tar", "failed intermediate composition must stop publication")
}

func TestGcsUploadResultMatchesCompletedBytes(t *testing.T) {
	payload := make([]byte, 70000)
	_, err := rand.New(rand.NewSource(42)).Read(payload)
	require.NoError(t, err)
	for _, compression := range []Compression{CompressionNone, CompressionGzip, CompressionZstd, CompressionLz4} {
		t.Run(string(compression), func(t *testing.T) {
			fixture := newGCSFixture(t)
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "state"), payload, 0600))
			result, err := fixture.exporter.UploadWithResult(t.Context(), dir, "bucket", "snapshot", WithCompression(compression), WithChunkSize("1KB"), WithBufferSize("64KB"))
			require.NoError(t, err)
			require.NoError(t, result.Validate())
			key := "snapshot" + compression.Extension()
			stored := fixture.objects[key].Data
			sum := sha256.Sum256(stored)
			require.Equal(t, UploadResult{Version: 1, Layout: "single", ObjectCount: 1, ObjectKey: key, SHA256: fmt.Sprintf("%x", sum), SizeBytes: int64(len(stored)), Compression: compression}, result)
			require.Len(t, fixture.objects, 1)
			require.Greater(t, len(fixture.composed), 2)
			decoded, closeDecoder, err := restoreDecoder(bytes.NewReader(stored), compression)
			require.NoError(t, err)
			require.Equal(t, string(payload), readTarFiles(t, decoded)["state"])
			_, err = io.Copy(io.Discard, decoded)
			require.NoError(t, err)
			require.NoError(t, closeDecoder())
		})
	}
}

func TestGcsSplitResultNeverClaimsSingleObject(t *testing.T) {
	for _, tc := range []struct {
		name, chunk, part string
		size              int
	}{
		{"composed", "1KB", "2KB", 7000},
		{"copied", "2KB", "2KB", 7000},
		{"estimate composed", "1KB", "1MB", 1},
		{"estimate copied", "1MB", "1MB", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := newGCSFixture(t)
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "state"), bytes.Repeat([]byte("x"), tc.size), 0600))
			result, err := fixture.exporter.UploadWithResult(t.Context(), dir, "bucket", "snapshot", WithCompression(CompressionNone), WithSizeLimit("1B"), WithChunkSize(tc.chunk), WithPartSize(tc.part), WithBufferSize("64KB"))
			require.NoError(t, err)
			require.Equal(t, UploadResult{Version: 1, Layout: "split", ObjectCount: len(fixture.objects), Compression: CompressionNone}, result)
			require.NoError(t, result.Validate())
		})
	}
}

func TestGcsUploadResultSeparatesCleanupFromCompletion(t *testing.T) {
	for _, path := range []string{"single compose", "split compose", "split copy", "single copy", "intermediate cleanup"} {
		t.Run(path, func(t *testing.T) {
			fixture := newGCSFixture(t)
			fixture.fail = func(r *http.Request, _ string) bool { return r.Method == http.MethodDelete }
			options := []UploadOption{WithCompression(CompressionNone), WithChunkSize("1KB"), WithPartSize("2KB"), WithBufferSize("64KB")}
			if strings.Contains(path, "copy") {
				options = append(options, WithChunkSize("2KB"))
			}
			if strings.HasPrefix(path, "split") || path == "single copy" {
				options = append(options, WithSizeLimit("1B"))
			}
			dir := t.TempDir()
			size := 7000
			if path == "single copy" {
				size = 1
			}
			if path == "intermediate cleanup" {
				size = 70000
			}
			require.NoError(t, os.WriteFile(filepath.Join(dir, "state"), bytes.Repeat([]byte("x"), size), 0600))
			result, err := fixture.exporter.UploadWithResult(t.Context(), dir, "bucket", "snapshot", options...)
			require.NoError(t, err)
			require.True(t, result.CleanupRequired)
			require.NoError(t, result.Validate())
			finalCount := 0
			for name := range fixture.objects {
				if strings.HasSuffix(name, ".tar") {
					finalCount++
				}
			}
			require.Equal(t, finalCount, result.ObjectCount)
			for _, final := range append(fixture.composed, fixture.copied...) {
				count := 0
				for _, other := range append(fixture.composed, fixture.copied...) {
					if other == final {
						count++
					}
				}
				require.Equal(t, 1, count, "confirmed output must never be rewritten for cleanup")
			}
		})
	}
}

func TestGcsUploadResultRejectsProviderFailures(t *testing.T) {
	for _, stage := range []string{"part", "intermediate", "final", "copy"} {
		t.Run(stage, func(t *testing.T) {
			fixture := newGCSFixture(t)
			fixture.fail = func(r *http.Request, name string) bool {
				switch stage {
				case "part":
					return !strings.Contains(r.URL.Path, "/o/")
				case "intermediate":
					return name == "snapshot.tar-temp-0-0"
				case "final":
					return name == "snapshot.tar"
				default:
					return strings.Contains(r.URL.Path, "/rewriteTo/")
				}
			}
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "state"), bytes.Repeat([]byte("x"), 40000), 0600))
			options := []UploadOption{WithCompression(CompressionNone), WithChunkSize("1KB"), WithBufferSize("64KB")}
			if stage == "copy" {
				options = append(options, WithPartSize("1KB"), WithSizeLimit("1B"))
			}
			result, err := fixture.exporter.UploadWithResult(t.Context(), dir, "bucket", "snapshot", options...)
			require.ErrorContains(t, err, "injected provider failure")
			require.Equal(t, UploadResult{}, result)
			require.NotContains(t, fixture.objects, "snapshot.tar")
		})
	}
}

type failedArchiveReader struct{ err error }

func (r failedArchiveReader) Read([]byte) (int, error) { return 0, r.err }

func TestGcsUploadChunksRejectsReadFailure(t *testing.T) {
	for _, readErr := range []error{errors.New("archive source failed"), io.ErrUnexpectedEOF, fmt.Errorf("compressor failed: %w", io.ErrUnexpectedEOF)} {
		t.Run(readErr.Error(), func(t *testing.T) {
			fixture := newGCSFixture(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			opts := defaultUploadOptions()
			opts.ChunkSize = 1024
			opts.BufferSize = 1024
			reader := io.MultiReader(bytes.NewReader(make([]byte, 1024)), failedArchiveReader{err: readErr})
			result, err := fixture.exporter.uploadChunks(ctx, cancel, reader, "bucket", "snapshot", 1024, 1024, opts)
			require.ErrorIs(t, err, readErr)
			require.Equal(t, UploadResult{}, result)
			require.NotContains(t, fixture.objects, "snapshot.tar.gz")
		})
	}
}

func TestGcsUploadLostCompletionAcknowledgementReturnsNoResult(t *testing.T) {
	for _, copyOutput := range []bool{false, true} {
		t.Run(fmt.Sprint(copyOutput), func(t *testing.T) {
			fixture := newGCSFixture(t)
			fixture.loseAck = "snapshot.tar"
			opts := []UploadOption{WithCompression(CompressionNone), WithChunkSize("1MB"), WithBufferSize("64KB")}
			if copyOutput {
				opts = append(opts, WithSizeLimit("1B"), WithPartSize("1MB"))
			}
			result, err := fixture.exporter.UploadWithResult(t.Context(), t.TempDir(), "bucket", "snapshot", opts...)
			require.Error(t, err)
			require.Equal(t, UploadResult{}, result)
			require.Contains(t, fixture.objects, "snapshot.tar", "remote commit does not prove its acknowledgement")
		})
	}
}

func TestGcsLegacyUploadRetainsCleanupFailure(t *testing.T) {
	fixture := newGCSFixture(t)
	fixture.fail = func(r *http.Request, _ string) bool { return r.Method == http.MethodDelete }
	err := fixture.exporter.Upload(t.TempDir(), "bucket", "snapshot", WithCompression(CompressionNone), WithChunkSize("1MB"), WithBufferSize("64KB"))
	require.EqualError(t, err, "upload completed; temporary object cleanup required")
	require.Contains(t, fixture.objects, "snapshot.tar")
	require.Equal(t, []string{"snapshot.tar"}, fixture.composed)
}

func TestGcsUploadCancellationStopsRequests(t *testing.T) {
	for _, stage := range []string{"part", "intermediate", "final", "cleanup"} {
		t.Run(stage, func(t *testing.T) {
			fixture := newGCSFixture(t)
			started := make(chan struct{})
			var once sync.Once
			var active atomic.Int64
			fixture.before = func(r *http.Request) {
				match := stage == "part" && !strings.Contains(r.URL.Path, "/o/") ||
					stage == "intermediate" && strings.Contains(r.URL.Path, "-temp-") ||
					stage == "final" && strings.HasSuffix(r.URL.Path, "/snapshot.tar/compose") ||
					stage == "cleanup" && r.Method == http.MethodDelete
				if !match {
					return
				}
				active.Add(1)
				defer active.Add(-1)
				_, _ = io.Copy(io.Discard, r.Body)
				once.Do(func() { close(started) })
				select {
				case <-r.Context().Done():
				case <-time.After(5 * time.Second):
				}
			}
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "state"), bytes.Repeat([]byte("x"), 70000), 0600))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			finished := make(chan struct{})
			var result UploadResult
			var err error
			go func() {
				defer close(finished)
				result, err = fixture.exporter.UploadWithResult(ctx, dir, "bucket", "snapshot", WithCompression(CompressionNone), WithChunkSize("1KB"), WithBufferSize("64KB"), WithConcurrentUploadJobs(2))
			}()
			select {
			case <-started:
			case <-time.After(5 * time.Second):
				t.Fatal("request did not start")
			}
			cancel()
			select {
			case <-finished:
			case <-time.After(5 * time.Second):
				t.Fatal("upload did not stop")
			}
			if stage == "cleanup" {
				require.NoError(t, err)
				require.True(t, result.CleanupRequired)
				require.NoError(t, result.Validate())
				completed := 0
				for _, name := range fixture.composed {
					if name == "snapshot.tar" {
						completed++
					}
				}
				require.Equal(t, 1, completed)
			} else {
				require.ErrorIs(t, err, context.Canceled)
				require.Equal(t, UploadResult{}, result)
			}
			require.Eventually(t, func() bool { return active.Load() == 0 }, 3*time.Second, 10*time.Millisecond)
		})
	}
}

func TestGcsUploadPreservesOrderAcrossThreeCompositionRounds(t *testing.T) {
	fixture := newGCSFixture(t)
	fixture.before = func(r *http.Request) {
		if strings.Contains(r.URL.Path, "-temp-0-0/compose") {
			time.Sleep(20 * time.Millisecond)
		}
	}
	dir := t.TempDir()
	payload := make([]byte, 1100000)
	_, err := rand.New(rand.NewSource(72)).Read(payload)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "state"), payload, 0600))
	result, err := fixture.exporter.UploadWithResult(t.Context(), dir, "bucket", "snapshot", WithCompression(CompressionNone), WithChunkSize("1KB"), WithBufferSize("64KB"))
	require.NoError(t, err)
	require.Contains(t, fixture.composed, "snapshot.tar-temp-1-0")
	stored := fixture.objects["snapshot.tar"].Data
	sum := sha256.Sum256(stored)
	require.Equal(t, fmt.Sprintf("%x", sum), result.SHA256)
	require.Equal(t, int64(len(stored)), result.SizeBytes)
	require.Equal(t, string(payload), readTarFiles(t, bytes.NewReader(stored))["state"])
	require.Len(t, fixture.objects, 1)
}
