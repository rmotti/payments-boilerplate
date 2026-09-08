//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestAPIBinaryRejectsIncompleteProviderConfiguration(t *testing.T) {
	root := repositoryRoot(t)
	binary := filepath.Join(t.TempDir(), "api-e2e")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "./cmd/api")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build API binary: %v\n%s", err, output)
	}

	command := exec.CommandContext(ctx, binary)
	command.Dir = root
	command.Env = replaceEnvironment(os.Environ(), map[string]string{
		"DATABASE_URL":         "postgres://payments:payments_local@127.0.0.1:55432/payments?sslmode=disable",
		"INTEGRATION_API_KEYS": apiKeyA,
		"STRIPE_SECRET_KEY":    "", "STRIPE_WEBHOOK_SECRET": "",
		"STRIPE_SUCCESS_URL": "", "STRIPE_CANCEL_URL": "",
	})
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("API binary accepted incomplete Stripe configuration\n%s", output)
	}
	if !strings.Contains(string(output), "STRIPE_SECRET_KEY is required") {
		t.Fatalf("API binary reported the wrong configuration failure\n%s", output)
	}
}

// TestWorkerBinaryConfigurationAndSignalShutdown is intentionally black-box.
// The in-process tests prove composition; this one proves that the actual
// executable loads environment configuration, installs signal handling and
// exits cleanly when SIGTERM arrives.
func TestWorkerBinaryConfigurationAndSignalShutdown(t *testing.T) {
	h := newHarness(t, false)
	root := repositoryRoot(t)
	binary := filepath.Join(t.TempDir(), "worker-e2e")
	buildCtx, cancelBuild := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancelBuild()
	build := exec.CommandContext(buildCtx, "go", "build", "-o", binary, "./cmd/worker")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build worker binary: %v\n%s", err, output)
	}

	probe, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve worker port: %v", err)
	}
	port := probe.Addr().(*net.TCPAddr).Port
	if err := probe.Close(); err != nil {
		t.Fatalf("release worker port: %v", err)
	}

	var output bytes.Buffer
	processCtx, cancelProcess := context.WithCancel(context.Background())
	defer cancelProcess()
	command := exec.CommandContext(processCtx, binary)
	command.Dir = root
	command.Stdout, command.Stderr = &output, &output
	command.Env = replaceEnvironment(os.Environ(), map[string]string{
		"APP_ENV": "test", "PORT": strconv.Itoa(port), "DATABASE_URL": h.databaseURL,
		"RABBITMQ_URL": h.rabbitURL, "LOG_FORMAT": "json", "LOG_LEVEL": "debug",
		"OTEL_ENABLED": "false", "STARTUP_TIMEOUT": "10s", "SHUTDOWN_TIMEOUT": "3s",
		"OUTBOX_BATCH_SIZE": "1", "OUTBOX_INTERVAL": "25ms", "OUTBOX_LEASE_DURATION": "10s",
		"CONSUMER_CONCURRENCY": "2", "CONSUMER_PREFETCH": "2", "CONSUMER_MAX_ATTEMPTS": "2",
		"CONSUMER_RETRY_DELAYS": "100ms,250ms", "DATABASE_MAX_OPEN_CONNECTIONS": "8",
	})
	if err := command.Start(); err != nil {
		t.Fatalf("start worker binary: %v", err)
	}
	defer func() {
		if command.ProcessState == nil {
			_ = command.Process.Kill()
			_, _ = command.Process.Wait()
		}
	}()

	endpoint := fmt.Sprintf("http://127.0.0.1:%d/health", port)
	client := &http.Client{Timeout: 500 * time.Millisecond}
	h.poll("black-box worker readiness", func() (bool, string, error) {
		request, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, endpoint, nil)
		response, err := client.Do(request)
		if err != nil {
			return false, err.Error(), nil
		}
		defer func() { _ = response.Body.Close() }()
		return response.StatusCode == http.StatusOK, response.Status, nil
	})
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal worker: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("worker exited after SIGTERM: %v\n%s", err, output.String())
		}
	case <-time.After(8 * time.Second):
		_ = command.Process.Kill()
		t.Fatalf("worker did not exit after SIGTERM\n%s", output.String())
	}
	if !strings.Contains(output.String(), "worker starting") {
		t.Fatalf("worker never reached configured runtime\n%s", output.String())
	}
}

func replaceEnvironment(current []string, replacements map[string]string) []string {
	result := make([]string, 0, len(current)+len(replacements))
	for _, item := range current {
		name, _, _ := strings.Cut(item, "=")
		if _, replaced := replacements[name]; !replaced {
			result = append(result, item)
		}
	}
	for name, value := range replacements {
		result = append(result, name+"="+value)
	}
	return result
}
