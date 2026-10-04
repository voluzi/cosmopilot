package cmd

import (
	"encoding/json"
	"errors"
	"os"

	"github.com/spf13/cobra"

	"github.com/voluzi/cosmopilot/v5/pkg/dataexporter"
)

func newRestoreCmd() *cobra.Command {
	var digest string
	command := &cobra.Command{
		Use:   "restore <dir> <bucket> <object-key>",
		Short: "Install an exported snapshot in a fresh data directory",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			return dataexporter.Restore(cmd.Context(), exporter, args[0], args[1], args[2], digest)
		},
	}
	command.Flags().StringVar(&digest, "sha256", "", "Optional SHA-256 of the complete stored object")
	return command
}

func writeRestoreTermination(err error) {
	result := dataexporter.RestoreResult{Stage: "download", Message: err.Error()}
	var restoreErr *dataexporter.RestoreError
	if errors.As(err, &restoreErr) {
		result.Stage = restoreErr.Stage
		result.Message = restoreErr.Err.Error()
	}
	// Leave space for Kubernetes' termination-message limit, even for long provider errors.
	if len(result.Message) > 1800 {
		result.Message = result.Message[:1800]
	}
	data, marshalErr := json.Marshal(result)
	if marshalErr == nil {
		_ = os.WriteFile("/dev/termination-log", data, 0644)
	}
}
