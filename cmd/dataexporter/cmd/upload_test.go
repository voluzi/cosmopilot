package cmd

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/voluzi/cosmopilot/v5/pkg/dataexporter"
)

type uploadTestExporter struct {
	dataexporter.Exporter
	height string
}

func (e *uploadTestExporter) Upload(_, _, _ string, opts ...dataexporter.UploadOption) error {
	var options dataexporter.UploadOptions
	for _, opt := range opts {
		opt(&options)
	}
	e.height = options.Height
	return nil
}

func TestUploadHeightFlagOverridesEnvironment(t *testing.T) {
	t.Setenv("DATA_HEIGHT", "150")
	original := exporter
	t.Cleanup(func() { exporter = original })
	for _, tc := range []struct {
		args   []string
		height string
	}{
		{[]string{"dir", "bucket", "snapshot"}, "150"},
		{[]string{"--height", "0", "dir", "bucket", "snapshot"}, "0"},
		{[]string{"--height=", "dir", "bucket", "snapshot"}, ""},
	} {
		recorder := &uploadTestExporter{}
		exporter = recorder
		command := newUploadCmd(dataexporter.DefaultChunkSize)
		command.SetArgs(tc.args)
		require.NoError(t, command.Execute())
		require.Equal(t, tc.height, recorder.height)
	}
}
