package cmd

import (
	"time"

	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"

	"github.com/voluzi/cosmopilot/v2/pkg/dataexporter"
	"github.com/voluzi/cosmopilot/v2/pkg/environ"
)

func newDeleteCmd() *cobra.Command {
	var concurrentDeleteJobs int
	command := &cobra.Command{
		Use:   "delete <bucket> <name>",
		Short: "Deletes objects from external storage",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			bucket, name := args[0], args[1]
			start := time.Now()
			if err := exporter.Delete(bucket, name,
				dataexporter.WithConcurrentDeleteJobs(concurrentDeleteJobs),
			); err != nil {
				return err
			}
			log.WithField("time-elapsed", time.Since(start)).Info("delete successful")
			return nil
		},
	}
	command.Flags().IntVar(&concurrentDeleteJobs, "concurrent-jobs",
		environ.GetInt("CONCURRENT_JOBS", dataexporter.DefaultConcurrentJobs),
		"Number of concurrent jobs",
	)
	return command
}
