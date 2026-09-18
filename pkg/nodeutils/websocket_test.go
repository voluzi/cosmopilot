package nodeutils

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewBlockWatcherReconnectsCoalescesAndCancels(t *testing.T) {
	var connections atomic.Int32
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)
		defer conn.Close()
		connections.Add(1)
		_, _, err = conn.ReadMessage()
		require.NoError(t, err)
		require.NoError(t, conn.WriteJSON(map[string]any{"jsonrpc": "2.0", "result": map[string]any{}}))
	}))
	t.Cleanup(server.Close)

	ctx, cancel := context.WithCancel(t.Context())
	wake := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		runNewBlockWatcher(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/websocket", wake)
		close(done)
	}()

	require.Eventually(t, func() bool { return connections.Load() >= 2 }, 3*time.Second, 10*time.Millisecond)
	assert.Len(t, wake, 1)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("websocket watcher did not stop after cancellation")
	}
}

func TestNewBlockWatcherBacksOffAfterDisconnect(t *testing.T) {
	connected := make(chan time.Time, 2)
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		require.NoError(t, err)
		connected <- time.Now()
		_ = conn.Close()
	}))
	t.Cleanup(server.Close)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		runNewBlockWatcher(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/websocket", make(chan struct{}, 1))
		close(done)
	}()

	first := <-connected
	second := <-connected
	assert.GreaterOrEqual(t, second.Sub(first), 200*time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("websocket watcher did not stop after cancellation")
	}
}
