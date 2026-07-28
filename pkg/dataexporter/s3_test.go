package dataexporter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/c2h5oh/datasize"
	"github.com/klauspost/compress/zstd"
)

func TestNewS3ExporterUsesPathStyleAndDefaultCredentials(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "test-access-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret-key")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")

	var requestPath string
	var requestPrefix string
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestPath = r.URL.Path
		requestPrefix = r.URL.Query().Get("prefix")
		authorization = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/xml")
		_, _ = w.Write([]byte("<ListBucketResult><IsTruncated>false</IsTruncated></ListBucketResult>"))
	}))
	defer server.Close()

	exporter, err := NewS3Exporter(context.Background(), S3Config{
		Region:         "us-east-1",
		Endpoint:       server.URL,
		ForcePathStyle: true,
	})
	if err != nil {
		t.Fatalf("NewS3Exporter() error = %v", err)
	}
	if err := exporter.Delete("snapshots", "cosmoshub-1"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if requestPath != "/snapshots" {
		t.Fatalf("request path = %q, want /snapshots", requestPath)
	}
	if requestPrefix != "cosmoshub-1" {
		t.Fatalf("prefix = %q, want cosmoshub-1", requestPrefix)
	}
	if authorization == "" {
		t.Fatal("expected request to be signed by the AWS default credential chain")
	}
}

func TestNewS3ExporterRejectsInvalidEndpoint(t *testing.T) {
	for _, endpoint := range []string{"://bad", "ftp://object-store.example.com"} {
		t.Run(endpoint, func(t *testing.T) {
			_, err := NewS3Exporter(context.Background(), S3Config{Region: "us-east-1", Endpoint: endpoint})
			if err == nil {
				t.Fatal("NewS3Exporter() expected an endpoint validation error")
			}
		})
	}
}

func TestS3UploadCreatesCompressedMultipartObject(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.db"), []byte("cosmos state"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	client := newFakeS3Client()
	exporter := newS3Exporter(client)
	if err := exporter.Upload(dir, "snapshots", "cosmoshub-1",
		WithCompression(CompressionZstd),
		WithChunkSize("6MB"),
		WithConcurrentUploadJobs(2),
	); err != nil {
		t.Fatalf("Upload() error = %v", err)
	}

	object, ok := client.completedObject("cosmoshub-1.tar.zst")
	if !ok {
		t.Fatal("expected cosmoshub-1.tar.zst to be completed")
	}
	decoder, err := zstd.NewReader(bytes.NewReader(object))
	if err != nil {
		t.Fatalf("open zstd object: %v", err)
	}
	defer decoder.Close()
	files := readTarFiles(t, decoder)
	if files["state.db"] != "cosmos state" {
		t.Fatalf("state.db = %q", files["state.db"])
	}
	if got := client.contentType("cosmoshub-1.tar.zst"); got != "application/zstd" {
		t.Fatalf("content type = %q, want application/zstd", got)
	}
	if !client.multipartBodiesAreFiles() {
		t.Fatal("multipart upload bodies must be disk-backed to bound memory usage")
	}
}

func TestS3UploadSplitsOversizedArchives(t *testing.T) {
	dir := t.TempDir()
	payload := bytes.Repeat([]byte("snapshot-data-"), 600000)
	if err := os.WriteFile(filepath.Join(dir, "state.db"), payload, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	client := newFakeS3Client()
	exporter := newS3Exporter(client)
	if err := exporter.Upload(dir, "snapshots", "osmosis-1",
		WithCompression(CompressionNone),
		WithSizeLimit("1B"),
		WithPartSize("6MB"),
		WithChunkSize("6MB"),
	); err != nil {
		t.Fatalf("Upload() error = %v", err)
	}

	names := client.completedNames()
	want := []string{"osmosis-1-part-00000000.tar", "osmosis-1-part-00000001.tar"}
	if fmt.Sprint(names) != fmt.Sprint(want) {
		t.Fatalf("completed objects = %v, want %v", names, want)
	}
	combined := append(client.mustCompletedObject(t, want[0]), client.mustCompletedObject(t, want[1])...)
	files := readTarFiles(t, bytes.NewReader(combined))
	if !bytes.Equal([]byte(files["state.db"]), payload) {
		t.Fatal("split archive did not reconstruct the source data")
	}
}

func TestS3ArchiveRequiresSplitBeforeMultipartLimit(t *testing.T) {
	options := defaultUploadOptions()
	options.ChunkSize = datasize.MustParseString(DefaultS3ChunkSize)

	split, err := s3ArchiveRequiresSplit(datasize.MustParseString("700GB"), options)
	if err != nil {
		t.Fatalf("s3ArchiveRequiresSplit() error = %v", err)
	}
	if !split {
		t.Fatal("s3ArchiveRequiresSplit() = false, want true for an archive exceeding 10,000 chunks")
	}

	split, err = s3ArchiveRequiresSplit(datasize.MustParseString("100GB"), options)
	if err != nil {
		t.Fatalf("s3ArchiveRequiresSplit() error = %v", err)
	}
	if split {
		t.Fatal("s3ArchiveRequiresSplit() = true, want false for an archive within multipart limits")
	}

	options.ChunkSize = datasize.MustParseString("5MB")
	if _, err := s3ArchiveRequiresSplit(datasize.MustParseString("100GB"), options); err == nil {
		t.Fatal("s3ArchiveRequiresSplit() expected an error when part size exceeds the multipart limit")
	}
}

func TestS3UploadAbortsMultipartUploadOnFailure(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.db"), []byte("state"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	client := newFakeS3Client()
	client.uploadErr = errors.New("upload failed")
	exporter := newS3Exporter(client)
	err := exporter.Upload(dir, "snapshots", "failed", WithChunkSize("6MB"))
	if err == nil {
		t.Fatal("Upload() expected an error")
	}
	if client.abortCount != 1 {
		t.Fatalf("abort count = %d, want 1", client.abortCount)
	}
}

func TestS3DeleteRemovesAllObjectsWithPrefix(t *testing.T) {
	client := newFakeS3Client()
	client.listedObjects = []types.Object{
		{Key: aws.String("snapshot.tar.zst")},
		{Key: aws.String("snapshot-part-00000000.tar.lz4")},
		{Key: aws.String("snapshot-part-00000001.tar.lz4")},
		{Key: aws.String("snapshot-old.tar.gz")},
		{Key: aws.String("snapshot.tar.gz.backup")},
		{Key: aws.String("snapshot-part-invalid.tar.lz4")},
	}
	exporter := newS3Exporter(client)

	if err := exporter.Delete("snapshots", "snapshot"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	// Deletions run concurrently, so compare as a set.
	want := []string{"snapshot-part-00000000.tar.lz4", "snapshot-part-00000001.tar.lz4", "snapshot.tar.zst"}
	got := append([]string(nil), client.deletedKeys...)
	sort.Strings(got)
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("deleted keys = %v, want %v", got, want)
	}
}

// TestS3DeleteBatchesAgainstAmazonS3 keeps request volume bounded where batching is accepted: a small
// partSize splits a large snapshot into very many objects, and one DeleteObject per key would cost
// three orders of magnitude more requests than batches of 1000 — long enough to be throttled and to
// block snapshot expiration.
func TestS3DeleteBatchesAgainstAmazonS3(t *testing.T) {
	client := newFakeS3Client()
	for i := range 2500 {
		client.listedObjects = append(client.listedObjects,
			types.Object{Key: aws.String(fmt.Sprintf("snapshot-part-%08d.tar.lz4", i))})
	}
	exporter := newS3Exporter(client) // no custom endpoint: Amazon S3

	if err := exporter.Delete("snapshots", "snapshot"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if len(client.deletedKeys) != 2500 {
		t.Fatalf("deleted keys = %d, want 2500", len(client.deletedKeys))
	}
	if client.singleCalls != 0 {
		t.Fatalf("per-object calls = %d, want 0 against Amazon S3", client.singleCalls)
	}
	if client.batchCalls != 3 {
		t.Fatalf("batch calls = %d, want 3 (2500 keys in batches of 1000)", client.batchCalls)
	}
}

func TestS3DeleteUsesPerObjectAgainstCustomEndpoint(t *testing.T) {
	client := newFakeS3Client()
	client.listedObjects = []types.Object{
		{Key: aws.String("snapshot.tar.zst")},
		{Key: aws.String("snapshot-part-00000000.tar.lz4")},
	}
	exporter := newS3Exporter(client)
	exporter.perObjectDelete = true

	if err := exporter.Delete("snapshots", "snapshot"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if client.batchCalls != 0 {
		t.Fatalf("batch calls = %d, want 0 against an S3-compatible endpoint", client.batchCalls)
	}
	if client.singleCalls != 2 {
		t.Fatalf("per-object calls = %d, want 2", client.singleCalls)
	}
}

// TestS3DeleteSendsNoChecksumHeaderAgainstCustomEndpoint exercises a real SDK client against an
// httptest server and asserts on the bytes actually put on the wire.
//
// The multi-object DeleteObjects API is modelled with RequireChecksum, so the SDK sends
// x-amz-checksum-crc32 and ignores RequestChecksumCalculation entirely. Amazon S3 accepts that, but
// MinIO and other S3-compatible stores require the legacy Content-MD5 and reject it with 400
// MissingContentMD5. Deleting per object sends no body, hence no integrity header at all.
//
// A fake s3API cannot catch this: the header is added by SDK middleware, below the interface.
func TestS3DeleteSendsNoChecksumHeaderAgainstCustomEndpoint(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "test-access-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret-key")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")

	var mu sync.Mutex
	var deleteRequests []*http.Request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		if r.Method == http.MethodDelete {
			mu.Lock()
			deleteRequests = append(deleteRequests, r.Clone(context.Background()))
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		// MinIO rejects a POST ?delete carrying a CRC32 checksum instead of Content-MD5.
		if r.URL.Query().Has("delete") {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`<Error><Code>MissingContentMD5</Code>` +
				`<Message>Missing required header for this request: Content-Md5.</Message></Error>`))
			return
		}
		_, _ = w.Write([]byte("<ListBucketResult><IsTruncated>false</IsTruncated>" +
			"<Contents><Key>cosmoshub-1.tar.gz</Key></Contents></ListBucketResult>"))
	}))
	defer server.Close()

	exporter, err := NewS3Exporter(context.Background(), S3Config{
		Region:         "us-east-1",
		Endpoint:       server.URL,
		ForcePathStyle: true,
	})
	if err != nil {
		t.Fatalf("NewS3Exporter() error = %v", err)
	}
	if err := exporter.Delete("snapshots", "cosmoshub-1"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(deleteRequests) != 1 {
		t.Fatalf("per-object DELETE requests = %d, want 1", len(deleteRequests))
	}
	request := deleteRequests[0]
	if got := request.URL.Path; got != "/snapshots/cosmoshub-1.tar.gz" {
		t.Fatalf("delete path = %q, want /snapshots/cosmoshub-1.tar.gz", got)
	}
	for _, header := range []string{"X-Amz-Checksum-Crc32", "X-Amz-Sdk-Checksum-Algorithm"} {
		if value := request.Header.Get(header); value != "" {
			t.Fatalf("delete request carried %s=%q; S3-compatible stores reject checksummed deletes", header, value)
		}
	}
}

func TestS3DeleteReportsPerObjectFailures(t *testing.T) {
	client := newFakeS3Client()
	client.listedObjects = []types.Object{
		{Key: aws.String("snapshot.tar.zst")},
		{Key: aws.String("snapshot-part-00000000.tar.lz4")},
	}
	client.deleteErr = errors.New("access denied")
	exporter := newS3Exporter(client)

	err := exporter.Delete("snapshots", "snapshot")
	if err == nil {
		t.Fatal("Delete() expected an error")
	}
	if !strings.Contains(err.Error(), "access denied") {
		t.Fatalf("Delete() error = %v, want it to mention the underlying failure", err)
	}
}

type fakeS3Client struct {
	mu            sync.Mutex
	parts         map[string]map[int32][]byte
	contentTypes  map[string]string
	completed     map[string][]byte
	listedObjects []types.Object
	deletedKeys   []string
	batchCalls    int
	singleCalls   int
	uploadErr     error
	deleteErr     error
	abortCount    int
	nextUploadID  int
	diskBacked    bool
}

func newFakeS3Client() *fakeS3Client {
	return &fakeS3Client{
		parts:        make(map[string]map[int32][]byte),
		contentTypes: make(map[string]string),
		completed:    make(map[string][]byte),
		diskBacked:   true,
	}
}

func (f *fakeS3Client) CreateMultipartUpload(_ context.Context, input *s3.CreateMultipartUploadInput, _ ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextUploadID++
	uploadID := fmt.Sprintf("upload-%d", f.nextUploadID)
	f.parts[uploadID] = make(map[int32][]byte)
	f.contentTypes[aws.ToString(input.Key)] = aws.ToString(input.ContentType)
	return &s3.CreateMultipartUploadOutput{UploadId: aws.String(uploadID)}, nil
}

func (f *fakeS3Client) UploadPart(_ context.Context, input *s3.UploadPartInput, _ ...func(*s3.Options)) (*s3.UploadPartOutput, error) {
	if f.uploadErr != nil {
		return nil, f.uploadErr
	}
	if _, ok := input.Body.(*os.File); !ok {
		f.mu.Lock()
		f.diskBacked = false
		f.mu.Unlock()
	}
	content, err := io.ReadAll(input.Body)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.parts[aws.ToString(input.UploadId)][aws.ToInt32(input.PartNumber)] = content
	return &s3.UploadPartOutput{ETag: aws.String(fmt.Sprintf("etag-%d", aws.ToInt32(input.PartNumber)))}, nil
}

func (f *fakeS3Client) CompleteMultipartUpload(_ context.Context, input *s3.CompleteMultipartUploadInput, _ ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	parts := f.parts[aws.ToString(input.UploadId)]
	numbers := make([]int, 0, len(parts))
	for number := range parts {
		numbers = append(numbers, int(number))
	}
	sort.Ints(numbers)
	var object []byte
	for _, number := range numbers {
		object = append(object, parts[int32(number)]...)
	}
	f.completed[aws.ToString(input.Key)] = object
	return &s3.CompleteMultipartUploadOutput{}, nil
}

func (f *fakeS3Client) AbortMultipartUpload(_ context.Context, _ *s3.AbortMultipartUploadInput, _ ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.abortCount++
	return &s3.AbortMultipartUploadOutput{}, nil
}

func (f *fakeS3Client) ListObjectsV2(_ context.Context, _ *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	return &s3.ListObjectsV2Output{Contents: f.listedObjects}, nil
}

func (f *fakeS3Client) DeleteObjects(_ context.Context, input *s3.DeleteObjectsInput, _ ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	f.batchCalls++
	for _, object := range input.Delete.Objects {
		f.deletedKeys = append(f.deletedKeys, aws.ToString(object.Key))
	}
	return &s3.DeleteObjectsOutput{}, nil
}

func (f *fakeS3Client) DeleteObject(_ context.Context, input *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	f.singleCalls++
	f.deletedKeys = append(f.deletedKeys, aws.ToString(input.Key))
	return &s3.DeleteObjectOutput{}, nil
}

func (f *fakeS3Client) completedObject(name string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	object, ok := f.completed[name]
	return object, ok
}

func (f *fakeS3Client) mustCompletedObject(t *testing.T, name string) []byte {
	t.Helper()
	object, ok := f.completedObject(name)
	if !ok {
		t.Fatalf("object %q was not completed", name)
	}
	return object
}

func (f *fakeS3Client) completedNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	names := make([]string, 0, len(f.completed))
	for name := range f.completed {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (f *fakeS3Client) contentType(name string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.contentTypes[name]
}

func (f *fakeS3Client) multipartBodiesAreFiles() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.diskBacked
}
