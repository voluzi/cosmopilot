package proxy

import (
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func TestNewTCPProxy(t *testing.T) {
	tests := []struct {
		name        string
		localAddr   string
		remoteAddr  string
		failOnClose bool
		wantErr     bool
	}{
		{
			name:        "valid addresses",
			localAddr:   ":0",
			remoteAddr:  "127.0.0.1:8080",
			failOnClose: false,
			wantErr:     false,
		},
		{
			name:        "with failOnClose",
			localAddr:   ":0",
			remoteAddr:  "127.0.0.1:9090",
			failOnClose: true,
			wantErr:     false,
		},
		{
			name:        "invalid local address",
			localAddr:   "invalid:address:format",
			remoteAddr:  "127.0.0.1:8080",
			failOnClose: false,
			wantErr:     true,
		},
		{
			name:        "invalid remote address",
			localAddr:   ":0",
			remoteAddr:  "invalid:address:format",
			failOnClose: false,
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			proxy, err := NewTCPProxy(tt.localAddr, tt.remoteAddr, tt.failOnClose)

			if (err != nil) != tt.wantErr {
				t.Errorf("NewTCPProxy() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr && proxy == nil {
				t.Error("NewTCPProxy() returned nil proxy without error")
			}

			if !tt.wantErr && proxy.runOnce != tt.failOnClose {
				t.Errorf("NewTCPProxy() runOnce = %v, want %v", proxy.runOnce, tt.failOnClose)
			}
		})
	}
}

func TestNewTCPProxyNilCallbackDoesNotRetryUpstream(t *testing.T) {
	proxy, err := NewTCPProxy(":0", "127.0.0.1:8080", false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if proxy.onAccept != nil {
		t.Fatal("NewTCPProxy() stored a nil callback")
	}
	if proxy.retryUpstream {
		t.Fatal("NewTCPProxy() enabled upstream retries for a nil callback")
	}
}

func TestNewTCPProxyStoresAcceptCallback(t *testing.T) {
	called := false
	proxy, err := NewTCPProxy(":0", "127.0.0.1:8080", false, func() { called = true })
	if err != nil {
		t.Fatal(err)
	}
	if proxy.onAccept == nil {
		t.Fatal("NewTCPProxy() did not store the accept callback")
	}

	proxy.onAccept()
	if !called {
		t.Fatal("stored accept callback was not invoked")
	}
}

func TestTCPProxy_RetainsAcceptedConnectionUntilUpstreamStarts(t *testing.T) {
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	upstreamAddr := reserved.Addr().String()
	if err := reserved.Close(); err != nil {
		t.Fatal(err)
	}

	accepted := make(chan struct{})
	proxy, err := NewTCPProxy(":0", upstreamAddr, false, func() { close(accepted) })
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.ListenTCP("tcp", proxy.laddr)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()

	handleResult := make(chan error, 1)
	go func() {
		conn, acceptErr := listener.AcceptTCP()
		if acceptErr != nil {
			handleResult <- acceptErr
			return
		}
		proxy.onAccept()
		handleResult <- proxy.handle(conn)
	}()

	client, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("accept callback was not invoked")
	}

	upstream, err := net.Listen("tcp", upstreamAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	go func() {
		conn, acceptErr := upstream.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(conn, conn)
	}()

	payload := []byte("signer handshake")
	if _, err := client.Write(payload); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, len(payload))
	_ = client.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(client, response); err != nil {
		t.Fatalf("accepted signer connection was not retained: %v", err)
	}
	if string(response) != string(payload) {
		t.Fatalf("response = %q, want %q", response, payload)
	}
	_ = client.Close()
	select {
	case err := <-handleResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("proxy handler did not finish after connection close")
	}
}

func TestTCPProxy_StartAcceptsConnectionsWhileUpstreamUnavailable(t *testing.T) {
	reservedUpstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	upstreamAddr := reservedUpstream.Addr().String()
	if err := reservedUpstream.Close(); err != nil {
		t.Fatal(err)
	}

	reservedProxy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxyAddr := reservedProxy.Addr().String()
	if err := reservedProxy.Close(); err != nil {
		t.Fatal(err)
	}

	accepted := make(chan struct{}, 2)
	proxy, err := NewTCPProxy(proxyAddr, upstreamAddr, false, func() { accepted <- struct{}{} })
	if err != nil {
		t.Fatal(err)
	}
	startResult := make(chan error, 1)
	go func() { startResult <- proxy.Start() }()
	t.Cleanup(func() {
		if proxy.listener != nil {
			_ = proxy.Stop()
		}
	})

	dialProxy := func() net.Conn {
		deadline := time.Now().Add(2 * time.Second)
		for {
			conn, dialErr := net.Dial("tcp", proxyAddr)
			if dialErr == nil {
				return conn
			}
			if time.Now().After(deadline) {
				t.Fatalf("proxy did not start: %v", dialErr)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	first := dialProxy()
	defer first.Close()
	second := dialProxy()
	defer second.Close()

	for i := 0; i < 2; i++ {
		select {
		case <-accepted:
		case err := <-startResult:
			t.Fatalf("proxy stopped before accepting both connections: %v", err)
		case <-time.After(time.Second):
			t.Fatal("accept loop blocked while the first connection retried its upstream")
		}
	}

	upstream, err := net.Listen("tcp", upstreamAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer upstream.Close()
	go func() {
		for i := 0; i < 2; i++ {
			conn, acceptErr := upstream.Accept()
			if acceptErr != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()

	payload := []byte("second signer")
	if _, err := second.Write(payload); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, len(payload))
	_ = second.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(second, response); err != nil {
		t.Fatalf("second accepted connection was not retained: %v", err)
	}
	if string(response) != string(payload) {
		t.Fatalf("response = %q, want %q", response, payload)
	}
}

func TestTCPProxy_DataForwarding(t *testing.T) {
	// Start a simple echo server as the upstream
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start upstream server: %v", err)
	}
	defer upstream.Close()

	upstreamAddr := upstream.Addr().String()

	// Echo server goroutine
	go func() {
		conn, err := upstream.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(conn, conn) // Echo back whatever is received
	}()

	// Create and start proxy
	proxy, err := NewTCPProxy(":0", upstreamAddr, true)
	if err != nil {
		t.Fatalf("failed to create proxy: %v", err)
	}

	// Start proxy in background
	proxyStarted := make(chan string)
	go func() {
		// We need to start the listener first to get the port
		listener, err := net.ListenTCP("tcp", proxy.laddr)
		if err != nil {
			t.Errorf("failed to start listener: %v", err)
			return
		}
		proxy.listener = listener
		proxyStarted <- listener.Addr().String()

		// Accept one connection and handle it
		lconn, err := listener.AcceptTCP()
		if err != nil {
			return
		}
		_ = proxy.handle(lconn)
		listener.Close()
	}()

	// Wait for proxy to start
	proxyAddr := <-proxyStarted

	// Connect to proxy
	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("failed to connect to proxy: %v", err)
	}
	defer conn.Close()

	// Send test data
	testData := "hello proxy"
	_, err = conn.Write([]byte(testData))
	if err != nil {
		t.Fatalf("failed to write to proxy: %v", err)
	}

	// Read response
	buf := make([]byte, len(testData))
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("failed to read from proxy: %v", err)
	}

	if string(buf[:n]) != testData {
		t.Errorf("expected %q, got %q", testData, string(buf[:n]))
	}
}

func TestTCPProxy_Stop(t *testing.T) {
	proxy, err := NewTCPProxy(":0", "127.0.0.1:8080", false)
	if err != nil {
		t.Fatalf("failed to create proxy: %v", err)
	}

	// Start listener
	listener, err := net.ListenTCP("tcp", proxy.laddr)
	if err != nil {
		t.Fatalf("failed to start listener: %v", err)
	}
	proxy.listener = listener

	// Stop should close the listener
	err = proxy.Stop()
	if err != nil {
		t.Errorf("Stop() returned error: %v", err)
	}

	// Trying to accept should fail after stop
	_, err = listener.Accept()
	if err == nil {
		t.Error("expected error after stopping listener, got nil")
	}
}

func TestTCPProxy_HandleUpstreamUnavailable(t *testing.T) {
	// Create proxy pointing to non-existent upstream
	proxy, err := NewTCPProxy(":0", "127.0.0.1:59999", false)
	if err != nil {
		t.Fatalf("failed to create proxy: %v", err)
	}

	// Create a mock local connection
	server, client := net.Pipe()
	defer server.Close()
	defer client.Close()

	// Wrap in TCPConn-like behavior (we can't actually get a TCPConn from Pipe)
	// Instead, test the error path by checking that handle returns an error
	// when upstream is unavailable

	// Start listener
	listener, err := net.ListenTCP("tcp", proxy.laddr)
	if err != nil {
		t.Fatalf("failed to start listener: %v", err)
	}
	proxy.listener = listener
	defer listener.Close()

	proxyAddr := listener.Addr().String()

	// Connect and expect the connection to be closed (upstream unavailable)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		conn, err := listener.AcceptTCP()
		if err != nil {
			return
		}
		// This should fail because upstream is not available
		err = proxy.handle(conn)
		if err == nil {
			t.Error("expected error when upstream unavailable, got nil")
		}
	}()

	// Connect to trigger the handler
	conn, err := net.Dial("tcp", proxyAddr)
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	conn.Close()

	wg.Wait()
}

func TestTCPProxy_ConcurrentConnections(t *testing.T) {
	// Start a simple echo server
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to start upstream: %v", err)
	}
	defer upstream.Close()

	upstreamAddr := upstream.Addr().String()

	// Handle multiple connections on upstream
	go func() {
		for {
			conn, err := upstream.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()

	// Create proxy
	proxy, err := NewTCPProxy(":0", upstreamAddr, false)
	if err != nil {
		t.Fatalf("failed to create proxy: %v", err)
	}

	listener, err := net.ListenTCP("tcp", proxy.laddr)
	if err != nil {
		t.Fatalf("failed to start listener: %v", err)
	}
	proxy.listener = listener
	defer listener.Close()

	proxyAddr := listener.Addr().String()

	// Accept and handle connections
	go func() {
		for {
			conn, err := listener.AcceptTCP()
			if err != nil {
				return
			}
			go func(c *net.TCPConn) {
				_ = proxy.handle(c)
			}(conn)
		}
	}()

	// Make concurrent connections
	numConns := 5
	var wg sync.WaitGroup
	errors := make(chan error, numConns)

	for i := 0; i < numConns; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()

			conn, err := net.Dial("tcp", proxyAddr)
			if err != nil {
				errors <- err
				return
			}
			defer conn.Close()

			testData := []byte("test")
			_, err = conn.Write(testData)
			if err != nil {
				errors <- err
				return
			}

			buf := make([]byte, len(testData))
			_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			_, err = conn.Read(buf)
			if err != nil {
				errors <- err
				return
			}

			if string(buf) != string(testData) {
				errors <- err
			}
		}(i)
	}

	wg.Wait()
	close(errors)

	for err := range errors {
		if err != nil {
			t.Errorf("concurrent connection error: %v", err)
		}
	}
}
