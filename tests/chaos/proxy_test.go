package chaos

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

func TestTCPProxyControlsConnectionFailuresAndRecovery(t *testing.T) {
	t.Parallel()

	echo := startEchoServer(t)
	proxy, err := NewTCPProxy(context.Background(), echo.Addr().String())
	if err != nil {
		t.Fatalf("NewTCPProxy() error = %v", err)
	}
	t.Cleanup(func() {
		if err := proxy.Close(); err != nil {
			t.Errorf("close proxy: %v", err)
		}
	})

	forwarded := dialProxy(t, proxy)
	assertEcho(t, forwarded, "forwarded")

	proxy.CloseExisting()
	waitCtx, cancelWait := context.WithTimeout(context.Background(), time.Second)
	defer cancelWait()
	if err := proxy.WaitForConnections(waitCtx, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := forwarded.Write([]byte("closed")); err == nil {
		buffer := make([]byte, len("closed"))
		_ = forwarded.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
		if _, readErr := io.ReadFull(forwarded, buffer); readErr == nil {
			t.Fatal("connection remained usable after CloseExisting")
		}
	}
	_ = forwarded.Close()

	proxy.RejectNew()
	rejected := dialProxy(t, proxy)
	_ = rejected.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := rejected.Read(make([]byte, 1)); err == nil {
		t.Fatal("rejected connection remained open")
	}
	_ = rejected.Close()

	proxy.Silence()
	silent := dialProxy(t, proxy)
	if _, err := silent.Write([]byte("restored")); err != nil {
		t.Fatalf("write to silent connection: %v", err)
	}
	_ = silent.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	if _, err := silent.Read(make([]byte, 1)); err == nil {
		t.Fatal("silent connection unexpectedly answered")
	} else if !errors.Is(err, context.DeadlineExceeded) {
		var networkError net.Error
		if !errors.As(err, &networkError) || !networkError.Timeout() {
			t.Fatalf("silent read error = %v, want timeout", err)
		}
	}

	if err := silent.SetReadDeadline(time.Time{}); err != nil {
		t.Fatalf("clear read deadline: %v", err)
	}
	proxy.Restore()
	buffer := make([]byte, len("restored"))
	if _, err := io.ReadFull(silent, buffer); err != nil {
		t.Fatalf("read after Restore: %v", err)
	}
	if string(buffer) != "restored" {
		t.Fatalf("echo = %q, want restored", buffer)
	}
	_ = silent.Close()
}

func startEchoServer(t *testing.T) net.Listener {
	t.Helper()

	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen echo server: %v", err)
	}
	var connectionsMu sync.Mutex
	var connections []net.Conn
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			connectionsMu.Lock()
			connections = append(connections, connection)
			connectionsMu.Unlock()
			go func() {
				_, _ = io.Copy(connection, connection)
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		connectionsMu.Lock()
		for _, connection := range connections {
			_ = connection.Close()
		}
		connectionsMu.Unlock()
		<-done
	})
	return listener
}

func dialProxy(t *testing.T, proxy *TCPProxy) net.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	connection, err := (&net.Dialer{}).DialContext(ctx, "tcp", proxy.Addr())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	return connection
}

func assertEcho(t *testing.T, connection net.Conn, text string) {
	t.Helper()
	if _, err := connection.Write([]byte(text)); err != nil {
		t.Fatalf("write echo: %v", err)
	}
	buffer := make([]byte, len(text))
	if _, err := io.ReadFull(connection, buffer); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(buffer) != text {
		t.Fatalf("echo = %q, want %q", buffer, text)
	}
}
