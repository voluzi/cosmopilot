package nodeutils

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestStateSyncChunkMetricsExist(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		want    bool
		wantErr bool
	}{
		{
			name: "chunk request sent",
			body: `cometbft_p2p_message_send_bytes_total{message_type="statesync_ChunkRequest"} 42` + "\n",
			want: true,
		},
		{
			name: "chunk response received",
			body: `cometbft_p2p_message_receive_bytes_total{message_type="statesync_ChunkResponse"} 7` + "\n",
			want: true,
		},
		{
			name: "only unrelated message types",
			body: `cometbft_p2p_message_send_bytes_total{message_type="consensus_Vote"} 3` + "\n" +
				`cometbft_p2p_message_receive_bytes_total{message_type="mempool_Txs"} 9` + "\n",
		},
		{
			name: "only unrelated metric families",
			body: "# TYPE go_goroutines gauge\ngo_goroutines 12\n",
		},
		{
			name:    "malformed exposition",
			body:    "cometbft_p2p_message_send_bytes_total{message_type=\n",
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			var got bool
			var err error
			require.NotPanics(t, func() {
				got, err = StateSyncChunkMetricsExist(context.Background(), srv.URL)
			})
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestStateSyncChunkMetricsExistReportsHTTPErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	_, err := StateSyncChunkMetricsExist(context.Background(), srv.URL)
	require.ErrorContains(t, err, "503")
}
