package config

import (
	"testing"
	"time"
)

func TestLoadUsesRailwayPort(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://example")
	t.Setenv("PORT", "9090")
	t.Setenv("HTTP_ADDRESS", ":8080")

	cfg, err := Load("test-service", ":8000", false)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.HTTPAddress != ":9090" {
		t.Fatalf("HTTPAddress = %q, want %q", cfg.HTTPAddress, ":9090")
	}
	if cfg.OTelServiceName != "test-service" {
		t.Fatalf("OTelServiceName = %q, want %q", cfg.OTelServiceName, "test-service")
	}
	if cfg.ShutdownTimeout != 15*time.Second {
		t.Fatalf("ShutdownTimeout = %s, want 15s", cfg.ShutdownTimeout)
	}
}

func TestLoadRequiresRabbitMQWhenRequested(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://example")
	t.Setenv("RABBITMQ_URL", "")

	if _, err := Load("test-worker", ":8001", true); err == nil {
		t.Fatal("Load() error = nil, want an error")
	}
}
