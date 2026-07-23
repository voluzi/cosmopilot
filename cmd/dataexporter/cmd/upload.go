package cmd

import (
	"fmt"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/voluzi/cosmopilot/v2/pkg/dataexporter"
	"github.com/voluzi/cosmopilot/v2/pkg/environ"
)

func newUploadCmd(defaultChunkSize string) *cobra.Command {
	var partSize string
	var chunkSize string
	var sizeLimit string
	var reportPeriod time.Duration
	var concurrentUploadJobs int
	var bufferSize string
	var compressionName string

	command := &cobra.Command{
		Use:   "upload <dir> <bucket> <name>",
		Short: "Uploads a tar archive to external storage",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			compression, err := dataexporter.ParseCompression(compressionName)
			if err != nil {
				return fmt.Errorf("invalid compression: %w", err)
			}
			dir, bucket, name := args[0], args[1], args[2]
			start := time.Now()
			if err := exporter.Upload(dir, bucket, name,
				dataexporter.WithCompression(compression),
				dataexporter.WithChunkSize(chunkSize),
				dataexporter.WithSizeLimit(sizeLimit),
				dataexporter.WithPartSize(partSize),
				dataexporter.WithReportPeriod(reportPeriod),
				dataexporter.WithConcurrentUploadJobs(concurrentUploadJobs),
				dataexporter.WithBufferSize(bufferSize),
			); err != nil {
				return err
			}
			log.WithField("time-elapsed", time.Since(start)).Info("upload successful")
			return nil
		},
	}

	command.Flags().StringVar(&compressionName, "compression",
		environ.GetString("COMPRESSION", string(dataexporter.CompressionGzip)),
		"Archive compression: none, gzip, zstd, or lz4",
	)
	command.Flags().StringVar(&chunkSize, "chunk-size",
		environ.GetString("CHUNK_SIZE", defaultChunkSize),
		"Chunk size for multi-part uploads",
	)
	command.Flags().StringVar(&partSize, "part-size",
		environ.GetString("PART_SIZE", dataexporter.DefaultPartSize),
		"Part size on multi-part uploads (when size limit is crossed)",
	)
	command.Flags().StringVar(&sizeLimit, "size-limit",
		environ.GetString("SIZE_LIMIT", dataexporter.DefaultSizeLimit),
		"Size limit for single file",
	)
	command.Flags().DurationVar(&reportPeriod, "report-period",
		environ.GetDuration("REPORT_PERIOD", dataexporter.DefaultReportPeriod),
		"Period for progress reporting",
	)
	command.Flags().IntVar(&concurrentUploadJobs, "concurrent-jobs",
		environ.GetInt("CONCURRENT_JOBS", dataexporter.DefaultConcurrentJobs),
		"Number of concurrent jobs",
	)
	command.Flags().StringVar(&bufferSize, "buffer-size",
		environ.GetString("BUFFER_SIZE", dataexporter.DefaultBufferSize),
		"Buffer size on upload",
	)
	return command
}
