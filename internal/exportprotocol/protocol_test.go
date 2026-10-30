package exportprotocol

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/voluzi/cosmopilot/v5/pkg/dataexporter"
)

func TestStrictResultEnvelope(t *testing.T) {
	valid := `{"version":1,"ownerUID":"owner","exportID":"export","attemptID":"attempt","attemptSequence":1,"recipeSHA256":"` + strings.Repeat("a", 64) + `","jobUID":"job","podUID":"pod","scratchPVCUID":"pvc","scratchPVUID":"pv","result":{"version":1,"layout":"single","objectCount":1,"compression":"gzip","objectKey":"archive.tar.gz","sha256":"` + strings.Repeat("b", 64) + `","sizeBytes":1,"cleanupRequired":false}}`
	for _, tc := range []struct{ name, input string }{
		{"case variant", strings.Replace(valid, `"version":1`, `"version":1,"Version":1`, 1)},
		{"nested case variant", strings.Replace(valid, `"cleanupRequired":false`, `"cleanupRequired":false,"CleanupRequired":false`, 1)},
		{"duplicate", strings.Replace(valid, `"version":1`, `"version":1,"version":1`, 1)},
		{"escaped duplicate", strings.Replace(valid, `"version":1`, `"version":1,"\u0076ersion":1`, 1)},
		{"unknown nested", strings.Replace(valid, `"layout":"single"`, `"unexpected":1,"layout":"single"`, 1)},
		{"null required", strings.Replace(valid, `"cleanupRequired":false`, `"cleanupRequired":null`, 1)},
		{"missing required", strings.Replace(valid, `,"cleanupRequired":false`, "", 1)},
		{"trailing", valid + ` {}`}, {"truncated", valid[:len(valid)-1]},
		{"oversized", valid + strings.Repeat(" ", 3072)},
		{"invalid UTF8", valid + string([]byte{255})},
	} {
		t.Run(tc.name, func(t *testing.T) { _, err := DecodeEnvelope([]byte(tc.input)); require.Error(t, err) })
	}
	reordered := strings.Replace(valid, `"version":1,`, "", 1)
	reordered = strings.Replace(reordered, `"result":`, `"version":1,"result":`, 1)
	_, err := DecodeEnvelope([]byte(reordered))
	require.NoError(t, err)
	got, err := DecodeEnvelope([]byte(valid))
	require.NoError(t, err)
	require.Equal(t, "archive.tar.gz", got.Result.ObjectKey)
	require.NoError(t, got.Result.Validate())
	got.Result = dataexporter.UploadResult{Version: 1, Layout: "split", ObjectCount: 2, Compression: dataexporter.CompressionGzip}
	encoded, err := EncodeEnvelope(got)
	require.NoError(t, err)
	split, err := DecodeEnvelope(encoded)
	require.NoError(t, err)
	require.Empty(t, split.Result.ObjectKey)
}
