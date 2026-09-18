package nodeutils

import (
	"context"
	"time"

	"github.com/gorilla/websocket"
	log "github.com/sirupsen/logrus"
)

const (
	websocketReconnectMin = 250 * time.Millisecond
	websocketReconnectMax = 5 * time.Second
)

func runNewBlockWatcher(ctx context.Context, url string, wake chan<- struct{}) {
	backoff := websocketReconnectMin
	for {
		if ctx.Err() != nil {
			return
		}
		conn, _, err := websocket.DefaultDialer.DialContext(ctx, url, nil)
		if err != nil {
			if !waitForReconnect(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, websocketReconnectMax)
			continue
		}

		notifyReconcile(wake)
		if err := conn.WriteJSON(map[string]any{
			"jsonrpc": "2.0",
			"id":      1,
			"method":  "subscribe",
			"params":  map[string]string{"query": "tm.event='NewBlockHeader'"},
		}); err != nil {
			_ = conn.Close()
			if !waitForReconnect(ctx, backoff) {
				return
			}
			backoff = min(backoff*2, websocketReconnectMax)
			continue
		}

		closed := make(chan struct{})
		go func() {
			select {
			case <-ctx.Done():
				_ = conn.Close()
			case <-closed:
			}
		}()
		receivedMessage := false
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				break
			}
			receivedMessage = true
			notifyReconcile(wake)
		}
		close(closed)
		_ = conn.Close()
		if ctx.Err() == nil {
			log.Debug("new-block websocket disconnected; reconnecting")
		}
		if !waitForReconnect(ctx, backoff) {
			return
		}
		if receivedMessage {
			backoff = websocketReconnectMin
		} else {
			backoff = min(backoff*2, websocketReconnectMax)
		}
	}
}

func notifyReconcile(wake chan<- struct{}) {
	select {
	case wake <- struct{}{}:
	default:
	}
}

func waitForReconnect(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
