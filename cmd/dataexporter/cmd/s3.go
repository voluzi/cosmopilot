package cmd

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/voluzi/cosmopilot/v2/pkg/dataexporter"
	"github.com/voluzi/cosmopilot/v2/pkg/environ"
)

var (
	s3Region         string
	s3Endpoint       string
	s3ForcePathStyle bool
)

var s3Cmd = &cobra.Command{
	Use:   "s3",
	Short: "Amazon S3 and S3-compatible storage operations",
	Long:  "Manage uploads and deletions in Amazon S3, MinIO, DigitalOcean Spaces, and compatible object stores.",
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		if err := rootCmd.PersistentPreRunE(cmd, args); err != nil {
			return err
		}
		var err error
		exporter, err = dataexporter.NewS3Exporter(context.Background(), dataexporter.S3Config{
			Region:         s3Region,
			Endpoint:       s3Endpoint,
			ForcePathStyle: s3ForcePathStyle,
		})
		return err
	},
}

func init() {
	rootCmd.AddCommand(s3Cmd)
	s3Cmd.PersistentFlags().StringVar(&s3Region, "region",
		environ.GetString("AWS_REGION", environ.GetString("AWS_DEFAULT_REGION", "")),
		"AWS region used to sign requests",
	)
	s3Cmd.PersistentFlags().StringVar(&s3Endpoint, "endpoint",
		environ.GetString("S3_ENDPOINT", ""),
		"Custom S3-compatible endpoint URL",
	)
	s3Cmd.PersistentFlags().BoolVar(&s3ForcePathStyle, "force-path-style",
		environ.GetBool("S3_FORCE_PATH_STYLE", false),
		"Use path-style bucket addressing",
	)
	s3Cmd.AddCommand(newUploadCmd(dataexporter.DefaultS3ChunkSize))
	s3Cmd.AddCommand(newDeleteCmd())
}
