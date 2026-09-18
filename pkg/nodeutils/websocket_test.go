package nodeutils

import (
	"context"
	"fmt"
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
	ctx, cancel := context.WithCancel(t.Context())
	handlerErrors := make(chan error, 8)
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			reportWebsocketHandlerError(handlerErrors, fmt.Errorf("upgrade connection: %w", err))
			return
		}
		defer conn.Close()
		_, _, err = conn.ReadMessage()
		if err != nil {
			if ctx.Err() == nil {
				reportWebsocketHandlerError(handlerErrors, fmt.Errorf("read subscription: %w", err))
			}
			return
		}
		if err := conn.WriteJSON(map[string]any{"jsonrpc": "2.0", "result": map[string]any{}}); err != nil {
			if ctx.Err() == nil {
				reportWebsocketHandlerError(handlerErrors, fmt.Errorf("write notification: %w", err))
			}
			return
		}
		connections.Add(1)
	}))
	t.Cleanup(server.Close)

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
	assertNoWebsocketHandlerError(t, handlerErrors)
}

func TestNewBlockWatcherBacksOffAfterDisconnect(t *testing.T) {
	connected := make(chan time.Time, 2)
	handlerErrors := make(chan error, 8)
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			reportWebsocketHandlerError(handlerErrors, fmt.Errorf("upgrade connection: %w", err))
			return
		}
		select {
		case connected <- time.Now():
		default:
		}
		_ = conn.Close()
	}))
	t.Cleanup(server.Close)

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		runNewBlockWatcher(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/websocket", make(chan struct{}, 1))
		close(done)
	}()

	first := waitForWebsocketConnection(t, connected)
	second := waitForWebsocketConnection(t, connected)
	assert.GreaterOrEqual(t, second.Sub(first), 200*time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("websocket watcher did not stop after cancellation")
	}
	assertNoWebsocketHandlerError(t, handlerErrors)
}

func reportWebsocketHandlerError(errors chan<- error, err error) {
	select {
	case errors <- err:
	default:
	}
}

func waitForWebsocketConnection(t *testing.T, connected <-chan time.Time) time.Time {
	t.Helper()
	select {
	case at := <-connected:
		return at
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for websocket connection")
		return time.Time{}
	}
}

func assertNoWebsocketHandlerError(t *testing.T, errors <-chan error) {
	t.Helper()
	select {
	case err := <-errors:
		require.NoError(t, err)
	default:
	}
}
