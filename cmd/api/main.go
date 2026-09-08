// Command api starts the public HTTP API.
//
// It owns only the concerns of a process: reading configuration, turning
// signals into a cancelled context and mapping a failure to an exit code. The
// composition it runs lives in internal/runtime/api, where tests can start the
// same wiring this binary starts.
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/rmotti/payments-boilerplate/internal/platform/config"
	"github.com/rmotti/payments-boilerplate/internal/platform/logging"
	runtimeapi "github.com/rmotti/payments-boilerplate/internal/runtime/api"
)

func main() {
	if err := run(); err != nil {
		_ = logging.WriteSanitizedError(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load(runtimeapi.ServiceName, runtimeapi.DefaultAddress, false)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	return runtimeapi.Run(ctx, cfg, runtimeapi.Options{})
}
