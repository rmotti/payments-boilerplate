// Package logging builds structured process loggers.
package logging

import (
	"fmt"
	"strings"

	"github.com/rmotti/payments-boilerplate/internal/platform/config"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// New returns a process logger configured for development or production use.
func New(cfg config.Config) (*zap.Logger, error) {
	level := zapcore.InfoLevel
	if err := level.UnmarshalText([]byte(strings.ToLower(cfg.LogLevel))); err != nil {
		return nil, fmt.Errorf("parse LOG_LEVEL: %w", err)
	}

	var zapCfg zap.Config
	switch cfg.LogFormat {
	case "console":
		zapCfg = zap.NewDevelopmentConfig()
	case "json":
		zapCfg = zap.NewProductionConfig()
	default:
		return nil, fmt.Errorf("unsupported LOG_FORMAT %q", cfg.LogFormat)
	}

	zapCfg.Level = zap.NewAtomicLevelAt(level)
	zapCfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	logger, err := zapCfg.Build()
	if err != nil {
		return nil, fmt.Errorf("build logger: %w", err)
	}

	return logger.With(
		zap.String("service", cfg.ServiceName),
		zap.String("environment", cfg.Environment),
	), nil
}
