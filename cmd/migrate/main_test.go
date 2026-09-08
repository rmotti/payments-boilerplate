package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/rmotti/payments-boilerplate/internal/platform/errsanitize"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestGooseLoggerSanitizesFormattedMessages(t *testing.T) {
	t.Parallel()
	const sentinel = "migration-secret-sentinel"

	tests := []struct {
		name string
		log  func(gooseLogger)
	}{
		{
			name: "fatal",
			log: func(logger gooseLogger) {
				logger.Fatalf("migration failed: %s", "postgres://payments:"+sentinel+"@db.internal/payments")
			},
		},
		{
			name: "informational",
			log: func(logger gooseLogger) {
				logger.Printf("migration source: %s", "amqp://payments:"+sentinel+"@rabbitmq:5672")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.InfoLevel)
			tt.log(gooseLogger{logger: zap.New(core)})

			entries := logs.All()
			if len(entries) != 1 {
				t.Fatalf("log entries = %d, want 1", len(entries))
			}
			message := fmt.Sprint(entries[0].ContextMap()["message"])
			if strings.Contains(message, sentinel) {
				t.Fatalf("logged message %q leaked the migration credential", message)
			}
			if !strings.Contains(message, errsanitize.Redacted) {
				t.Fatalf("logged message = %q, want %q", message, errsanitize.Redacted)
			}
		})
	}
}
