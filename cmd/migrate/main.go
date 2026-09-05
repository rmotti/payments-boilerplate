// Command migrate manages PostgreSQL schema migrations.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/rmotti/payments-boilerplate/internal/platform/config"
	"github.com/rmotti/payments-boilerplate/internal/platform/logging"
	"go.uber.org/zap"
)

func main() {
	if err := run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) != 2 {
		return fmt.Errorf("usage: migrate <up|down|status>")
	}

	cfg, err := config.Load("payments-migrate", ":0", false)
	if err != nil {
		return err
	}
	logger, err := logging.New(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = logger.Sync() }()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	db, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("open postgres: %w", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			logger.Error("postgres shutdown failed", zap.Error(err))
		}
	}()
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("ping postgres: %w", err)
	}

	goose.SetLogger(gooseLogger{logger: logger})
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set migration dialect: %w", err)
	}

	switch os.Args[1] {
	case "up":
		err = goose.UpContext(ctx, db, cfg.MigrationsDir)
	case "down":
		err = goose.DownContext(ctx, db, cfg.MigrationsDir)
	case "status":
		err = goose.StatusContext(ctx, db, cfg.MigrationsDir)
	default:
		return fmt.Errorf("unknown migration command %q", os.Args[1])
	}
	if err != nil {
		return fmt.Errorf("migrate %s: %w", os.Args[1], err)
	}
	return nil
}

type gooseLogger struct {
	logger *zap.Logger
}

func (l gooseLogger) Fatalf(format string, args ...any) {
	l.logger.Error("migration fatal error", zap.String("message", fmt.Sprintf(format, args...)))
}

func (l gooseLogger) Printf(format string, args ...any) {
	l.logger.Info("migration", zap.String("message", fmt.Sprintf(format, args...)))
}
