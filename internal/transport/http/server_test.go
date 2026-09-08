package httpserver

import (
	"context"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/rmotti/payments-boilerplate/internal/platform/health"
	"go.uber.org/zap"
)

// TestRunServesInjectedListener proves the socket a caller already holds is
// the one served. It is what lets a test bind port zero, read the address it
// was given and reach the server there, with no window in which another
// process could take the port back.
func TestRunServesInjectedListener(t *testing.T) {
	t.Parallel()

	listener := listen(t)
	address := listener.Addr().String()

	server := New(Config{
		// Address names a port nothing serves. If it were bound instead of
		// the listener, the request below would fail.
		Address:         "127.0.0.1:0",
		ShutdownTimeout: 5 * time.Second,
		Listener:        listener,
	}, zap.NewNop(), newHealthOnlyHandler(), nil)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()

	response, err := waitForHealth(t, "http://"+address+"/health")
	if err != nil {
		t.Fatalf("GET /health error = %v", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("GET /health status = %d, want %d", response.StatusCode, http.StatusOK)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run() did not return after the context was cancelled")
	}
}

// TestRunReleasesTheListenerOnShutdown proves shutdown closes the socket
// rather than leaving it bound. A composition that ends on a cancelled context
// has to leave the port free for the next process, or the next test.
func TestRunReleasesTheListenerOnShutdown(t *testing.T) {
	t.Parallel()

	listener := listen(t)
	address := listener.Addr().String()

	server := New(Config{
		ShutdownTimeout: 5 * time.Second,
		Listener:        listener,
	}, zap.NewNop(), newHealthOnlyHandler(), nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()

	response, err := waitForHealth(t, "http://"+address+"/health")
	if err != nil {
		t.Fatalf("GET /health error = %v", err)
	}
	_ = response.Body.Close()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() error = %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run() did not return after the context was cancelled")
	}

	var config net.ListenConfig
	reopened, err := config.Listen(context.Background(), "tcp", address)
	if err != nil {
		t.Fatalf("rebinding %s after shutdown: %v", address, err)
	}
	_ = reopened.Close()
}

// TestRunForcesCloseAfterShutdownTimeout proves that a handler which has not
// drained by the graceful deadline cannot leave Serve and its listener behind.
func TestRunForcesCloseAfterShutdownTimeout(t *testing.T) {
	t.Parallel()

	listener := listen(t)
	address := listener.Addr().String()
	requestStarted := make(chan struct{})
	healthHandler := NewAPIHandler(health.New("payments-test", "test", map[string]health.Checker{
		"blocked": func(ctx context.Context) error {
			close(requestStarted)
			<-ctx.Done()
			return ctx.Err()
		},
	}), nil, nil, nil)
	server := New(Config{
		ShutdownTimeout: 25 * time.Millisecond,
		Listener:        listener,
	}, zap.NewNop(), healthHandler, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- server.Run(ctx) }()

	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		response, err := http.Get("http://" + address + "/health") //nolint:noctx // The server closes this request context forcibly.
		if err == nil {
			_ = response.Body.Close()
		}
	}()

	select {
	case <-requestStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("health request did not reach the blocking checker")
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Run() error = %v, want shutdown deadline exceeded", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run() did not force the server closed after the shutdown timeout")
	}

	select {
	case <-requestDone:
	case <-time.After(10 * time.Second):
		t.Fatal("active request remained blocked after forced server close")
	}

	var listenConfig net.ListenConfig
	reopened, err := listenConfig.Listen(context.Background(), "tcp", address)
	if err != nil {
		t.Fatalf("rebinding %s after forced shutdown: %v", address, err)
	}
	_ = reopened.Close()
}

// listen opens a loopback socket on a port the kernel picks, which is the
// socket the server is then handed.
func listen(t *testing.T) net.Listener {
	t.Helper()

	var config net.ListenConfig
	listener, err := config.Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	return listener
}

func newHealthOnlyHandler() *APIHandler {
	return NewAPIHandler(health.New("payments-test", "test", map[string]health.Checker{
		"postgres": func(context.Context) error { return nil },
	}), nil, nil, nil)
}

// waitForHealth retries until the server goroutine has reached Serve. The
// listener is already accepting connections when Run starts, so this only
// absorbs the scheduling gap before the handler is attached.
func waitForHealth(t *testing.T, url string) (*http.Response, error) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		response, err := http.DefaultClient.Do(request)
		if err == nil {
			return response, nil
		}
		lastErr = err
		time.Sleep(20 * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = errors.New("timed out before the first attempt")
	}
	return nil, lastErr
}
