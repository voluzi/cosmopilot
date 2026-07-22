package cmd

import (
	"github.com/spf13/cobra"

	"github.com/voluzi/cosmopilot/v2/pkg/dataexporter"
)

var gcsCmd = &cobra.Command{
	Use:   "gcs",
	Short: "Google Cloud Storage (GCS) operations",
	Long:  "Manage uploads and deletions in Google Cloud Storage (GCS).",
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		if err := rootCmd.PersistentPreRunE(cmd, args); err != nil {
			return err
		}
		gcsExporter, err := dataexporter.FromProvider(dataexporter.GCS)
		exporter = gcsExporter
		return err
	},
}

func init() {
	rootCmd.AddCommand(gcsCmd)
	gcsCmd.AddCommand(newUploadCmd(dataexporter.DefaultChunkSize))
	gcsCmd.AddCommand(newDeleteCmd())
}
