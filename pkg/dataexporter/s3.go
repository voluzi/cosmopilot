package dataexporter

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/c2h5oh/datasize"
	log "github.com/sirupsen/logrus"
)

const (
	S3 Provider = "s3"

	s3MinimumChunkSize  = 5 * 1024 * 1024
	s3MaximumChunkSize  = 5 * 1024 * 1024 * 1024
	s3MaximumObjectSize = 5 * datasize.TB
	s3MaximumParts      = 10000
	s3DeleteBatchSize   = 1000
)

// S3Config configures Amazon S3 and S3-compatible endpoint behavior.
type S3Config struct {
	Region         string
	Endpoint       string
	ForcePathStyle bool
}

type s3API interface {
	CreateMultipartUpload(context.Context, *s3.CreateMultipartUploadInput, ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error)
	UploadPart(context.Context, *s3.UploadPartInput, ...func(*s3.Options)) (*s3.UploadPartOutput, error)
	CompleteMultipartUpload(context.Context, *s3.CompleteMultipartUploadInput, ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error)
	AbortMultipartUpload(context.Context, *s3.AbortMultipartUploadInput, ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error)
	ListObjectsV2(context.Context, *s3.ListObjectsV2Input, ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	DeleteObjects(context.Context, *s3.DeleteObjectsInput, ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error)
}

// S3Exporter implements Exporter for Amazon S3 and compatible object stores.
type S3Exporter struct {
	client s3API
}

// NewS3Exporter creates an exporter using the AWS SDK default credential chain.
func NewS3Exporter(ctx context.Context, cfg S3Config) (*S3Exporter, error) {
	if cfg.Endpoint != "" {
		endpoint, err := url.ParseRequestURI(cfg.Endpoint)
		if err != nil || endpoint.Host == "" {
			return nil, fmt.Errorf("invalid S3 endpoint %q", cfg.Endpoint)
		}
		if endpoint.Scheme != "http" && endpoint.Scheme != "https" {
			return nil, fmt.Errorf("S3 endpoint must use http or https")
		}
	}
	loadOptions := make([]func(*awsconfig.LoadOptions) error, 0, 1)
	if cfg.Region != "" {
		loadOptions = append(loadOptions, awsconfig.WithRegion(cfg.Region))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, loadOptions...)
	if err != nil {
		return nil, fmt.Errorf("load AWS configuration: %w", err)
	}
	if cfg.Endpoint != "" {
		awsCfg.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		awsCfg.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
	}

	client := s3.NewFromConfig(awsCfg, func(options *s3.Options) {
		options.UsePathStyle = cfg.ForcePathStyle
		if cfg.Endpoint != "" {
			options.BaseEndpoint = aws.String(cfg.Endpoint)
		}
	})
	return newS3Exporter(client), nil
}

func newS3Exporter(client s3API) *S3Exporter {
	return &S3Exporter{client: client}
}

func (exporter *S3Exporter) Provider() Provider {
	return S3
}

func (exporter *S3Exporter) Upload(dir, bucket, name string, opts ...UploadOption) error {
	options := defaultUploadOptions()
	for _, opt := range opts {
		opt(options)
	}
	if err := validateS3UploadOptions(options); err != nil {
		return err
	}

	info, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("cannot stat directory %q: %w", dir, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("%q is not a directory", dir)
	}
	totalSize, err := GetDirSize(dir)
	if err != nil {
		return fmt.Errorf("calculate directory size: %w", err)
	}
	splitArchive, err := s3ArchiveRequiresSplit(totalSize, options)
	if err != nil {
		return err
	}

	extension := options.Compression.Extension()
	log.WithFields(log.Fields{
		"size":        totalSize.HumanReadable(),
		"source":      dir,
		"target":      fmt.Sprintf("s3://%s/%s%s", bucket, name, extension),
		"compression": options.Compression,
	}).Info("start archiving and uploading")

	reader, writer := io.Pipe()
	defer reader.Close()
	go func() {
		if err := writeTarball(dir, writer, options.Compression); err != nil {
			_ = writer.CloseWithError(err)
			return
		}
		_ = writer.Close()
	}()

	ctx := context.Background()
	if !splitArchive {
		_, err := exporter.uploadObject(ctx, reader, bucket, name+extension, options, totalSize)
		return err
	}

	splitReader := bufio.NewReaderSize(reader, int(options.BufferSize.Bytes()))
	for index := 0; ; index++ {
		if _, err := splitReader.Peek(1); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("read archive: %w", err)
		}
		partName := fmt.Sprintf("%s-part-%08d%s", name, index, extension)
		partReader := &io.LimitedReader{R: splitReader, N: int64(options.PartSize.Bytes())}
		read, err := exporter.uploadObject(ctx, partReader, bucket, partName, options, totalSize)
		if err != nil {
			return err
		}
		if read == 0 {
			return nil
		}
		if read < int64(options.PartSize.Bytes()) {
			return nil
		}
	}
}

func s3ArchiveRequiresSplit(totalSize datasize.ByteSize, options *UploadOptions) (bool, error) {
	maximumMultipartSize := options.ChunkSize * datasize.ByteSize(s3MaximumParts)
	splitArchive := totalSize > options.SizeLimit || totalSize > maximumMultipartSize
	if !splitArchive || options.PartSize <= maximumMultipartSize {
		return splitArchive, nil
	}
	requiredParts := (options.PartSize.Bytes() + options.ChunkSize.Bytes() - 1) / options.ChunkSize.Bytes()
	return false, fmt.Errorf(
		"S3 archive part size %s would require %d multipart chunks; increase chunk size or lower part size",
		options.PartSize.HumanReadable(),
		requiredParts,
	)
}

func validateS3UploadOptions(options *UploadOptions) error {
	if err := validateUploadOptions(options); err != nil {
		return err
	}
	chunkSize := options.ChunkSize.Bytes()
	switch {
	case chunkSize < s3MinimumChunkSize:
		return fmt.Errorf("S3 chunk size must be at least 5MiB")
	case chunkSize > s3MaximumChunkSize:
		return fmt.Errorf("S3 chunk size must not exceed 5GiB")
	case options.PartSize < options.ChunkSize:
		return fmt.Errorf("S3 archive part size cannot be smaller than chunk size")
	case options.PartSize > s3MaximumObjectSize:
		return fmt.Errorf("S3 archive part size must not exceed 5TB")
	case options.SizeLimit > s3MaximumObjectSize:
		return fmt.Errorf("S3 size limit must not exceed 5TB")
	}
	return nil
}

func (exporter *S3Exporter) uploadObject(
	ctx context.Context,
	reader io.Reader,
	bucket string,
	objectName string,
	options *UploadOptions,
	totalSize datasize.ByteSize,
) (int64, error) {
	created, err := exporter.client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket:      aws.String(bucket),
		Key:         aws.String(objectName),
		ContentType: aws.String(options.Compression.ContentType()),
		Metadata: map[string]string{
			"cosmopilot-compression": string(options.Compression),
		},
	})
	if err != nil {
		return 0, fmt.Errorf("create S3 multipart upload for %q: %w", objectName, err)
	}
	uploadID := aws.ToString(created.UploadId)
	completed := false
	defer func() {
		if completed {
			return
		}
		abortCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, abortErr := exporter.client.AbortMultipartUpload(abortCtx, &s3.AbortMultipartUploadInput{
			Bucket:   aws.String(bucket),
			Key:      aws.String(objectName),
			UploadId: aws.String(uploadID),
		}); abortErr != nil {
			log.WithError(abortErr).Warnf("failed to abort S3 multipart upload %s", uploadID)
		}
	}()

	uploadCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var archivedBytes atomic.Uint64
	var uploadedBytes atomic.Uint64
	stopProgress := monitorS3Progress(uploadCtx, options.ReportPeriod, totalSize, &archivedBytes, &uploadedBytes)
	defer stopProgress()

	semaphore := make(chan struct{}, options.ConcurrentJobs)
	results := make(chan types.CompletedPart, s3MaximumParts)
	var wg sync.WaitGroup
	var firstErr error
	var errMu sync.Mutex
	setError := func(uploadErr error) {
		errMu.Lock()
		defer errMu.Unlock()
		if firstErr == nil {
			firstErr = uploadErr
			cancel()
		}
	}

	var totalRead int64
	bufferedReader := bufio.NewReaderSize(reader, int(options.BufferSize.Bytes()))
	for partNumber := int32(1); ; partNumber++ {
		buffer := make([]byte, int(options.ChunkSize.Bytes()))
		n, readErr := io.ReadFull(bufferedReader, buffer)
		if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
			setError(fmt.Errorf("read archive for %q: %w", objectName, readErr))
			break
		}
		if n == 0 {
			break
		}
		if partNumber > s3MaximumParts {
			setError(fmt.Errorf("S3 object %q exceeds the %d-part multipart limit", objectName, s3MaximumParts))
			break
		}
		buffer = buffer[:n]
		totalRead += int64(n)
		archivedBytes.Add(uint64(n))

		semaphore <- struct{}{}
		errMu.Lock()
		hasError := firstErr != nil
		errMu.Unlock()
		if hasError {
			<-semaphore
			break
		}

		wg.Add(1)
		go func(number int32, data []byte) {
			defer wg.Done()
			defer func() { <-semaphore }()
			output, uploadErr := exporter.client.UploadPart(uploadCtx, &s3.UploadPartInput{
				Bucket:        aws.String(bucket),
				Key:           aws.String(objectName),
				UploadId:      aws.String(uploadID),
				PartNumber:    aws.Int32(number),
				Body:          bytes.NewReader(data),
				ContentLength: aws.Int64(int64(len(data))),
			})
			if uploadErr != nil {
				setError(fmt.Errorf("upload S3 part %d for %q: %w", number, objectName, uploadErr))
				return
			}
			uploadedBytes.Add(uint64(len(data)))
			results <- types.CompletedPart{ETag: output.ETag, PartNumber: aws.Int32(number)}
		}(partNumber, buffer)

		if errors.Is(readErr, io.ErrUnexpectedEOF) {
			break
		}
	}

	wg.Wait()
	close(results)
	errMu.Lock()
	uploadErr := firstErr
	errMu.Unlock()
	if uploadErr != nil {
		return totalRead, uploadErr
	}
	if totalRead == 0 {
		return 0, nil
	}

	parts := make([]types.CompletedPart, 0, len(results))
	for part := range results {
		parts = append(parts, part)
	}
	sort.Slice(parts, func(i, j int) bool {
		return aws.ToInt32(parts[i].PartNumber) < aws.ToInt32(parts[j].PartNumber)
	})
	_, err = exporter.client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:   aws.String(bucket),
		Key:      aws.String(objectName),
		UploadId: aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{
			Parts: parts,
		},
	})
	if err != nil {
		return totalRead, fmt.Errorf("complete S3 multipart upload for %q: %w", objectName, err)
	}
	completed = true
	return totalRead, nil
}

func monitorS3Progress(
	ctx context.Context,
	period time.Duration,
	totalSize datasize.ByteSize,
	archivedBytes *atomic.Uint64,
	uploadedBytes *atomic.Uint64,
) func() {
	progressCtx, cancel := context.WithCancel(ctx)
	go func() {
		ticker := time.NewTicker(period)
		defer ticker.Stop()
		for {
			select {
			case <-progressCtx.Done():
				return
			case <-ticker.C:
				log.WithFields(log.Fields{
					"archived": datasize.ByteSize(archivedBytes.Load()).HumanReadable(),
					"uploaded": datasize.ByteSize(uploadedBytes.Load()).HumanReadable(),
					"dir-size": totalSize.HumanReadable(),
				}).Info("archiving and uploading")
			}
		}
	}()
	return cancel
}

func (exporter *S3Exporter) Delete(bucket, name string, opts ...DeleteOption) error {
	options := defaultDeleteOptions()
	for _, opt := range opts {
		opt(options)
	}
	if options.ConcurrentJobs < 1 {
		return fmt.Errorf("concurrent jobs must be greater than zero")
	}
	ctx := context.Background()
	var continuationToken *string
	objectIDs := make([]types.ObjectIdentifier, 0)
	for {
		output, err := exporter.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(bucket),
			Prefix:            aws.String(name),
			ContinuationToken: continuationToken,
		})
		if err != nil {
			return fmt.Errorf("list S3 objects with prefix %q: %w", name, err)
		}
		for _, object := range output.Contents {
			objectIDs = append(objectIDs, types.ObjectIdentifier{Key: object.Key})
		}
		if !aws.ToBool(output.IsTruncated) {
			break
		}
		continuationToken = output.NextContinuationToken
	}
	if len(objectIDs) == 0 {
		log.Warnf("no objects found with prefix: %s", name)
		return nil
	}

	semaphore := make(chan struct{}, options.ConcurrentJobs)
	errCh := make(chan error, (len(objectIDs)+s3DeleteBatchSize-1)/s3DeleteBatchSize)
	var wg sync.WaitGroup
	for start := 0; start < len(objectIDs); start += s3DeleteBatchSize {
		end := min(start+s3DeleteBatchSize, len(objectIDs))
		batch := append([]types.ObjectIdentifier(nil), objectIDs[start:end]...)
		semaphore <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-semaphore }()
			output, err := exporter.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
				Bucket: aws.String(bucket),
				Delete: &types.Delete{Objects: batch, Quiet: aws.Bool(true)},
			})
			if err != nil {
				errCh <- fmt.Errorf("delete S3 objects with prefix %q: %w", name, err)
				return
			}
			if len(output.Errors) > 0 {
				errCh <- fmt.Errorf("delete S3 objects with prefix %q: %s", name, formatS3DeleteErrors(output.Errors))
			}
		}()
	}
	wg.Wait()
	close(errCh)
	var deleteErrors []error
	for err := range errCh {
		deleteErrors = append(deleteErrors, err)
	}
	return errors.Join(deleteErrors...)
}

func formatS3DeleteErrors(deleteErrors []types.Error) string {
	messages := make([]string, 0, len(deleteErrors))
	for _, deleteErr := range deleteErrors {
		messages = append(messages, fmt.Sprintf("%s: %s", aws.ToString(deleteErr.Key), aws.ToString(deleteErr.Message)))
	}
	return fmt.Sprint(messages)
}
