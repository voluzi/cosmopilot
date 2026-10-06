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
			result, err := dataexporter.Restore(cmd.Context(), exporter, args[0], args[1], args[2], digest)
			if err == nil {
				writeRestoreResult("/dev/termination-log", result)
			}
			return err
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
	writeRestoreResult("/dev/termination-log", result)
}

func writeRestoreResult(destination string, result dataexporter.RestoreResult) {
	// Kubernetes truncates a termination message at 4096 bytes, which would leave invalid JSON.
	data, marshalErr := json.Marshal(result)
	for marshalErr == nil && len(data) > 4096 && len(result.Message) > 0 {
		result.Message = result.Message[:len(result.Message)/2]
		data, marshalErr = json.Marshal(result)
	}
	if marshalErr == nil {
		_ = os.WriteFile(destination, data, 0644)
	}
}
