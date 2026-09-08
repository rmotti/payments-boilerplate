// Command worker starts background message processing.
//
// It owns only the concerns of a process: reading configuration, turning
// signals into a cancelled context and mapping a failure to an exit code. The
// composition it runs lives in internal/runtime/worker, where tests can start
// the same wiring this binary starts.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/rmotti/payments-boilerplate/internal/platform/config"
	"github.com/rmotti/payments-boilerplate/internal/platform/logging"
	runtimeworker "github.com/rmotti/payments-boilerplate/internal/runtime/worker"
)

func main() {
	if err := run(); err != nil {
		_ = logging.WriteSanitizedError(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(runtimeworker.ServiceName, runtimeworker.DefaultAddress, true)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return runtimeworker.Run(ctx, cfg, runtimeworker.Options{})
}
