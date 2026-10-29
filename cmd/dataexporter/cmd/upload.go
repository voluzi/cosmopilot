package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/voluzi/cosmopilot/v5/pkg/dataexporter"
	"github.com/voluzi/cosmopilot/v5/pkg/environ"
)

type resultUploader interface {
	UploadWithResult(context.Context, string, string, string, ...dataexporter.UploadOption) (dataexporter.UploadResult, error)
}

func newUploadCmd(defaultChunkSize string) *cobra.Command {
	var partSize string
	var chunkSize string
	var sizeLimit string
	var reportPeriod time.Duration
	var concurrentUploadJobs int
	var bufferSize string
	var compressionName string
	var height string
	var resultFile string

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
			options := []dataexporter.UploadOption{
				dataexporter.WithCompression(compression),
				dataexporter.WithHeight(height),
				dataexporter.WithChunkSize(chunkSize),
				dataexporter.WithSizeLimit(sizeLimit),
				dataexporter.WithPartSize(partSize),
				dataexporter.WithReportPeriod(reportPeriod),
				dataexporter.WithConcurrentUploadJobs(concurrentUploadJobs),
				dataexporter.WithBufferSize(bufferSize),
			}
			if cmd.Flags().Changed("result-file") {
				if resultFile == "" {
					return fmt.Errorf("result file path must not be empty")
				}
				uploader, ok := exporter.(resultUploader)
				if !ok {
					return fmt.Errorf("exporter does not support upload results")
				}
				result, err := uploader.UploadWithResult(cmd.Context(), dir, bucket, name, options...)
				if err != nil {
					return err
				}
				if err := result.Validate(); err != nil {
					return fmt.Errorf("invalid upload result: %w", err)
				}
				data, err := json.Marshal(result)
				if err != nil {
					return fmt.Errorf("encode upload result: %w", err)
				}
				if len(data) > 3072 {
					return fmt.Errorf("upload result exceeds 3072 bytes")
				}
				if err := os.WriteFile(resultFile, data, 0600); err != nil {
					return fmt.Errorf("write upload result: %w", err)
				}
			} else if err := exporter.Upload(dir, bucket, name, options...); err != nil {
				return err
			}
			log.WithField("time-elapsed", time.Since(start)).Info("upload successful")
			return nil
		},
	}

	command.Flags().StringVar(&resultFile, "result-file", "", "Write acknowledged upload result JSON to this path (maximum 3072 bytes)")
	command.Flags().StringVar(&height, "height", environ.GetString("DATA_HEIGHT", ""), "Archived dataset block height")
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
