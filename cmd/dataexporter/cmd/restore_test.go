package cmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/voluzi/cosmopilot/v5/pkg/dataexporter"
)

func TestRestoreTerminationResult(t *testing.T) {
	for _, result := range []dataexporter.RestoreResult{
		{Stage: "complete", Height: "150"},
		{Stage: "complete"},
		{Stage: "verification", Message: strings.Repeat("\"é", 5000)},
	} {
		destination := filepath.Join(t.TempDir(), "termination-log")
		writeRestoreResult(destination, result)
		data, err := os.ReadFile(destination)
		require.NoError(t, err)
		require.LessOrEqual(t, len(data), 4096)
		var reported dataexporter.RestoreResult
		require.NoError(t, json.Unmarshal(data, &reported))
		require.Equal(t, result.Stage, reported.Stage)
		require.Equal(t, result.Height, reported.Height)
		if result.Stage == "complete" {
			if result.Height == "" {
				require.JSONEq(t, `{"stage":"complete","message":""}`, string(data))
			} else {
				require.JSONEq(t, `{"stage":"complete","message":"","height":"150"}`, string(data))
			}
		} else {
			require.NotEmpty(t, reported.Message)
		}
	}
}
