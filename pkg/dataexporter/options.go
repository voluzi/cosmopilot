package dataexporter

import (
	"errors"
	"fmt"
	"time"

	"github.com/c2h5oh/datasize"
)

const (
	DefaultSizeLimit      = "5TB"
	DefaultPartSize       = "500GB"
	DefaultChunkSize      = "250MB"
	DefaultS3ChunkSize    = "64MB"
	DefaultBufferSize     = "32MB"
	DefaultReportPeriod   = time.Second
	DefaultConcurrentJobs = 10
)

// UploadOptions configures the behavior of data uploads to cloud storage.
type UploadOptions struct {
	Compression    Compression
	PartSize       datasize.ByteSize
	ChunkSize      datasize.ByteSize
	SizeLimit      datasize.ByteSize
	ReportPeriod   time.Duration
	ConcurrentJobs int
	BufferSize     datasize.ByteSize
	validationErrs []error
}

func defaultUploadOptions() *UploadOptions {
	return &UploadOptions{
		Compression:    CompressionGzip,
		PartSize:       datasize.MustParseString(DefaultPartSize),
		ChunkSize:      datasize.MustParseString(DefaultChunkSize),
		SizeLimit:      datasize.MustParseString(DefaultSizeLimit),
		BufferSize:     datasize.MustParseString(DefaultBufferSize),
		ReportPeriod:   DefaultReportPeriod,
		ConcurrentJobs: DefaultConcurrentJobs,
	}
}

// UploadOption is a functional option for configuring uploads.
type UploadOption func(*UploadOptions)

// WithCompression selects the archive compression format.
func WithCompression(compression Compression) UploadOption {
	return func(o *UploadOptions) {
		o.Compression = compression
	}
}

// WithChunkSize sets the chunk size for uploads.
func WithChunkSize(size string) UploadOption {
	return func(o *UploadOptions) {
		parsed, err := datasize.ParseString(size)
		if err != nil {
			o.validationErrs = append(o.validationErrs, fmt.Errorf("invalid chunk size %q: %w", size, err))
			return
		}
		o.ChunkSize = parsed
	}
}

// WithPartSize sets the part size for multi-part uploads.
func WithPartSize(size string) UploadOption {
	return func(o *UploadOptions) {
		parsed, err := datasize.ParseString(size)
		if err != nil {
			o.validationErrs = append(o.validationErrs, fmt.Errorf("invalid part size %q: %w", size, err))
			return
		}
		o.PartSize = parsed
	}
}

// WithSizeLimit sets the maximum size before splitting into parts.
func WithSizeLimit(size string) UploadOption {
	return func(o *UploadOptions) {
		parsed, err := datasize.ParseString(size)
		if err != nil {
			o.validationErrs = append(o.validationErrs, fmt.Errorf("invalid size limit %q: %w", size, err))
			return
		}
		o.SizeLimit = parsed
	}
}

// WithBufferSize sets the buffer size for upload operations.
func WithBufferSize(size string) UploadOption {
	return func(o *UploadOptions) {
		parsed, err := datasize.ParseString(size)
		if err != nil {
			o.validationErrs = append(o.validationErrs, fmt.Errorf("invalid buffer size %q: %w", size, err))
			return
		}
		o.BufferSize = parsed
	}
}

// WithReportPeriod sets how often progress is reported.
func WithReportPeriod(period time.Duration) UploadOption {
	return func(o *UploadOptions) {
		o.ReportPeriod = period
	}
}

// WithConcurrentUploadJobs sets the number of concurrent upload workers.
func WithConcurrentUploadJobs(concurrentJobs int) UploadOption {
	return func(o *UploadOptions) {
		o.ConcurrentJobs = concurrentJobs
	}
}

func validateUploadOptions(options *UploadOptions) error {
	if err := errors.Join(options.validationErrs...); err != nil {
		return err
	}
	if _, err := ParseCompression(string(options.Compression)); err != nil {
		return err
	}
	switch {
	case options.ChunkSize == 0:
		return fmt.Errorf("chunk size must be greater than zero")
	case options.PartSize == 0:
		return fmt.Errorf("part size must be greater than zero")
	case options.SizeLimit == 0:
		return fmt.Errorf("size limit must be greater than zero")
	case options.BufferSize == 0:
		return fmt.Errorf("buffer size must be greater than zero")
	case options.ReportPeriod <= 0:
		return fmt.Errorf("report period must be greater than zero")
	case options.ConcurrentJobs < 1:
		return fmt.Errorf("concurrent jobs must be greater than zero")
	}
	return nil
}

// DeleteOptions configures the behavior of data deletion from cloud storage.
type DeleteOptions struct {
	ConcurrentJobs int
}

func defaultDeleteOptions() *DeleteOptions {
	return &DeleteOptions{
		ConcurrentJobs: DefaultConcurrentJobs,
	}
}

// DeleteOption is a functional option for configuring deletions.
type DeleteOption func(*DeleteOptions)

// WithConcurrentDeleteJobs sets the number of concurrent delete workers.
func WithConcurrentDeleteJobs(concurrentJobs int) DeleteOption {
	return func(o *DeleteOptions) {
		o.ConcurrentJobs = concurrentJobs
	}
}
