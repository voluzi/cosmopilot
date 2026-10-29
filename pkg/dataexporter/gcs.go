package dataexporter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"

	"cloud.google.com/go/storage"
	"github.com/c2h5oh/datasize"
	log "github.com/sirupsen/logrus"
	"google.golang.org/api/iterator"
)

const (
	GCS                   Provider = "gcs"
	CompositionBatchLimit int      = 32
)

// GcsExporter implements Exporter for Google Cloud Storage.
type GcsExporter struct {
	client *storage.Client
}

// NewGcsExporter creates a new GCS exporter using application default credentials.
func NewGcsExporter() (*GcsExporter, error) {
	ctx := context.Background()
	client, err := storage.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to create client: %v", err)
	}
	return &GcsExporter{
		client: client,
	}, nil
}

func (gcs *GcsExporter) Provider() Provider {
	return GCS
}

func (gcs *GcsExporter) Upload(dir, bucket, name string, opts ...UploadOption) error {
	result, err := gcs.UploadWithResult(context.Background(), dir, bucket, name, opts...)
	if err != nil {
		return err
	}
	if result.CleanupRequired {
		return errors.New("upload completed; temporary object cleanup required")
	}
	return nil
}

// UploadWithResult distinguishes acknowledged output from temporary cleanup still required.
func (gcs *GcsExporter) UploadWithResult(ctx context.Context, dir, bucket, name string, opts ...UploadOption) (UploadResult, error) {
	if err := ctx.Err(); err != nil {
		return UploadResult{}, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	options := defaultUploadOptions()
	for _, opt := range opts {
		opt(options)
	}
	if err := validateUploadOptions(options); err != nil {
		return UploadResult{}, err
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return UploadResult{}, fmt.Errorf("cannot stat directory %q: %w", dir, err)
	}
	if !fi.IsDir() {
		return UploadResult{}, fmt.Errorf("%q is not a directory", dir)
	}
	totalSize, err := GetDirSize(dir)
	if err != nil {
		return UploadResult{}, err
	}
	estimatedArchiveSize, err := estimateArchiveUpperBound(dir, totalSize, options.Compression)
	if err != nil {
		return UploadResult{}, err
	}
	if estimatedArchiveSize > options.SizeLimit && options.ChunkSize > options.PartSize {
		return UploadResult{}, errors.New("on multi-part, chunk size cannot be greater than part size")
	}
	log.WithFields(log.Fields{
		"size":        totalSize.HumanReadable(),
		"source":      dir,
		"target":      fmt.Sprintf("gs://%s/%s%s", bucket, name, options.Compression.Extension()),
		"compression": options.Compression,
	}).Info("start archiving and uploading")
	reader, closeStream := newArchiveStream(ctx, dir, options.Compression)
	defer closeStream()
	return gcs.uploadChunks(ctx, cancel, reader, bucket, name, totalSize, estimatedArchiveSize, options)
}

func (gcs *GcsExporter) uploadChunks(ctx context.Context, cancelUploads context.CancelFunc, reader io.Reader, bucket, objectName string, totalSize, estimatedArchiveSize datasize.ByteSize, opts *UploadOptions) (UploadResult, error) {
	var bytesArchived, bytesUploaded atomic.Uint64
	stopProgress := monitorUploadProgress(ctx, opts.ReportPeriod, totalSize, &bytesArchived, &bytesUploaded)
	defer stopProgress()
	digest := newUploadDigest()
	reader = io.TeeReader(reader, digest)
	var wg sync.WaitGroup
	semaphore := make(chan struct{}, opts.ConcurrentJobs)
	buf := make([]byte, opts.ChunkSize)
	var errMu sync.Mutex
	var uploadErr error
	setError := func(err error) {
		errMu.Lock()
		defer errMu.Unlock()
		if uploadErr == nil {
			uploadErr = err
			cancelUploads()
		}
	}
	firstError := func() error {
		errMu.Lock()
		defer errMu.Unlock()
		return uploadErr
	}
	var partNames []string
readChunks:
	for {
		select {
		case semaphore <- struct{}{}:
		case <-ctx.Done():
			setError(ctx.Err())
			break readChunks
		}
		if firstError() != nil {
			<-semaphore
			break
		}
		n, err := readArchiveChunk(reader, buf)
		if err != nil && !errors.Is(err, io.EOF) {
			<-semaphore
			setError(fmt.Errorf("read archive chunk: %w", err))
			break
		}
		if firstError() != nil {
			<-semaphore
			break
		}
		bytesArchived.Add(uint64(n))
		if n == 0 {
			<-semaphore
			break
		}
		chunk := append([]byte(nil), buf[:n]...)
		partName := fmt.Sprintf("%s-part-%08d", objectName, len(partNames))
		partNames = append(partNames, partName)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-semaphore }()
			if err := gcs.uploadToGCS(ctx, bucket, partName, newReaderWithBytesCounter(bytes.NewReader(chunk), &bytesUploaded), opts.BufferSize.Bytes()); err != nil {
				setError(fmt.Errorf("upload failed for %q: %w", partName, err))
			}
		}()
		if errors.Is(err, io.EOF) {
			break
		}
	}
	wg.Wait()
	if err := firstError(); err != nil {
		return UploadResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return UploadResult{}, err
	}
	count, cleanup, err := gcs.composeParts(ctx, bucket, partNames, objectName, estimatedArchiveSize, opts)
	if err != nil {
		return UploadResult{}, err
	}
	layout := "single"
	if estimatedArchiveSize > opts.SizeLimit {
		layout = "split"
	}
	result := digest.result(opts.Compression, layout, count, objectName+opts.Compression.Extension())
	result.CleanupRequired = cleanup
	return result, nil
}

func readArchiveChunk(reader io.Reader, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := reader.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func (gcs *GcsExporter) uploadToGCS(ctx context.Context, bucket, objName string, r io.Reader, bufferSize uint64) error {
	writer := gcs.client.Bucket(bucket).Object(objName).NewWriter(ctx)
	buf := make([]byte, bufferSize)
	if _, err := io.CopyBuffer(writer, r, buf); err != nil {
		_ = writer.Close()
		return err
	}
	return writer.Close()
}

func (gcs *GcsExporter) composeParts(ctx context.Context, bucket string, objects []string, objectName string, estimatedArchiveSize datasize.ByteSize, opts *UploadOptions) (int, bool, error) {
	extension := opts.Compression.Extension()
	if estimatedArchiveSize <= opts.SizeLimit {
		cleanup, err := gcs.composeIntoSingleObject(ctx, bucket, objects, objectName+extension, opts)
		return 1, cleanup, err
	}
	if opts.ChunkSize == opts.PartSize {
		cleanup, err := gcs.renameToFinalNames(ctx, objects, bucket, objectName, opts)
		return len(objects), cleanup, err
	}
	partSize := opts.PartSize
	if partSize%opts.ChunkSize != 0 {
		partSize += opts.ChunkSize - partSize%opts.ChunkSize
	}
	chunksPerPart := int(partSize / opts.ChunkSize)
	digits := getDigitCount(len(objects) / chunksPerPart)
	format := fmt.Sprintf("%%s-part-%%0%dd%s", digits, extension)
	count := 0
	cleanupRequired := false
	for i := 0; i < len(objects); i += chunksPerPart {
		partName := fmt.Sprintf(format, objectName, i/chunksPerPart)
		cleanup, err := gcs.composeIntoSingleObject(ctx, bucket, objects[i:min(i+chunksPerPart, len(objects))], partName, opts)
		if err != nil {
			return 0, false, err
		}
		cleanupRequired = cleanupRequired || cleanup
		count++
	}
	return count, cleanupRequired, nil
}

func (gcs *GcsExporter) renameToFinalNames(ctx context.Context, objects []string, bucket, objectName string, opts *UploadOptions) (bool, error) {
	cleanupRequired := false
	digits := getDigitCount(len(objects) - 1)
	for i, object := range objects {
		finalName := fmt.Sprintf("%s-part-%0*d%s", objectName, digits, i, opts.Compression.Extension())
		if len(objects) == 1 {
			finalName = objectName + opts.Compression.Extension()
		}
		cleanup, err := gcs.renameObject(ctx, bucket, object, finalName, opts)
		if err != nil {
			return false, err
		}
		cleanupRequired = cleanupRequired || cleanup
	}
	return cleanupRequired, nil
}

func (gcs *GcsExporter) renameObject(ctx context.Context, bucket, oldName, newName string, opts *UploadOptions) (bool, error) {
	src := gcs.client.Bucket(bucket).Object(oldName)
	copier := gcs.client.Bucket(bucket).Object(newName).CopierFrom(src)
	if opts.Height != "" {
		copier.Metadata = map[string]string{HeightMetadataKey: opts.Height}
	}
	if _, err := copier.Run(ctx); err != nil {
		return false, fmt.Errorf("rename object %q to %q: %w", oldName, newName, err)
	}
	return src.Delete(ctx) != nil, nil
}

func (gcs *GcsExporter) composeIntoSingleObject(ctx context.Context, bucket string, objects []string, objectName string, opts *UploadOptions) (bool, error) {
	var temporary []string
	for round := 0; len(objects) > CompositionBatchLimit; round++ {
		newParts := make([]string, (len(objects)+CompositionBatchLimit-1)/CompositionBatchLimit)
		roundCtx, cancel := context.WithCancel(ctx)
		var wg sync.WaitGroup
		semaphore := make(chan struct{}, opts.ConcurrentJobs)
		var firstErr error
		var errMu sync.Mutex
	composeBatches:
		for i := 0; i < len(objects); i += CompositionBatchLimit {
			select {
			case semaphore <- struct{}{}:
			case <-roundCtx.Done():
				break composeBatches
			}
			if roundCtx.Err() != nil {
				<-semaphore
				break
			}
			index := i / CompositionBatchLimit
			batch := objects[i:min(i+CompositionBatchLimit, len(objects))]
			tempName := fmt.Sprintf("%s-temp-%d-%d", objectName, round, index)
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-semaphore }()
				composer := gcs.client.Bucket(bucket).Object(tempName).ComposerFrom(objectsToHandles(gcs.client, bucket, batch)...)
				if _, err := composer.Run(roundCtx); err != nil {
					errMu.Lock()
					if firstErr == nil {
						firstErr = fmt.Errorf("compose intermediate object %q: %w", tempName, err)
						cancel()
					}
					errMu.Unlock()
					return
				}
				newParts[index] = tempName
			}()
		}
		wg.Wait()
		cancel()
		if firstErr != nil {
			return false, firstErr
		}
		if err := ctx.Err(); err != nil {
			return false, err
		}
		temporary = append(temporary, objects...)
		objects = newParts
	}
	composer := gcs.client.Bucket(bucket).Object(objectName).ComposerFrom(objectsToHandles(gcs.client, bucket, objects)...)
	if opts.Height != "" {
		composer.Metadata = map[string]string{HeightMetadataKey: opts.Height}
	}
	if _, err := composer.Run(ctx); err != nil {
		return false, fmt.Errorf("compose final object %q: %w", objectName, err)
	}
	temporary = append(temporary, objects...)
	return gcs.batchDelete(ctx, bucket, temporary, opts.ConcurrentJobs*10) != nil, nil
}

func objectsToHandles(client *storage.Client, bucket string, names []string) []*storage.ObjectHandle {
	handles := make([]*storage.ObjectHandle, len(names))
	for i, name := range names {
		handles[i] = client.Bucket(bucket).Object(name)
	}
	return handles
}

func (gcs *GcsExporter) batchDelete(ctx context.Context, bucket string, objectNames []string, batchSize int) error {
	var wg sync.WaitGroup
	errChan := make(chan error, len(objectNames))
	semaphore := make(chan struct{}, batchSize)

	for _, objName := range objectNames {
		wg.Add(1)
		semaphore <- struct{}{}

		go func(obj string) {
			defer wg.Done()
			defer func() { <-semaphore }()

			log.WithFields(map[string]interface{}{
				"object": obj,
				"bucket": bucket,
			}).Debug("deleting object")
			err := gcs.client.Bucket(bucket).Object(obj).Delete(ctx)
			if err != nil {
				errChan <- fmt.Errorf("failed to delete %s: %v", obj, err)
			}
		}(objName)
	}

	wg.Wait()
	close(errChan)

	// Collect errors
	var errList []error
	for err := range errChan {
		errList = append(errList, err)
	}
	if len(errList) > 0 {
		return fmt.Errorf("some deletions failed: %v", errList)
	}

	return nil
}

func (gcs *GcsExporter) Delete(bucket, name string, opts ...DeleteOption) error {
	options := defaultDeleteOptions()
	for _, opt := range opts {
		opt(options)
	}

	ctx := context.Background()
	// List objects that match the prefix
	var objectNames []string
	it := gcs.client.Bucket(bucket).Objects(ctx, &storage.Query{Prefix: name})

	for {
		objAttrs, err := it.Next()
		if errors.Is(err, iterator.Done) {
			break
		}
		if err != nil {
			return fmt.Errorf("failed to list objects: %v", err)
		}
		if isArchiveObjectName(name, objAttrs.Name) {
			objectNames = append(objectNames, objAttrs.Name)
		}
	}

	if len(objectNames) == 0 {
		log.Warnf("no objects found with prefix: %s", name)
		return nil
	}

	log.WithFields(map[string]interface{}{
		"bucket":  bucket,
		"name":    name,
		"objects": len(objectNames),
	}).Infof("deleting object(s) with name(prefix): %s", name)
	return gcs.batchDelete(ctx, bucket, objectNames, options.ConcurrentJobs)
}

func (gcs *GcsExporter) Read(ctx context.Context, bucket, name string) (io.ReadCloser, map[string]string, error) {
	object := gcs.client.Bucket(bucket).Object(name)
	attrs, err := object.Attrs(ctx)
	if err != nil {
		return nil, nil, err
	}
	reader, err := object.Generation(attrs.Generation).NewReader(ctx)
	if err != nil {
		return nil, nil, err
	}
	return reader, attrs.Metadata, nil
}
