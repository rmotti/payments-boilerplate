//go:build e2e

package e2e

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"testing"
)

const destructiveCleanupConsent = "E2E_ALLOW_DESTRUCTIVE_CLEANUP"

// validateCleanupTargets is deliberately narrower than the application's URL
// validation. The e2e suite truncates tables and purges queues, so knowing that
// a URL is syntactically valid is not enough: it must identify the documented,
// loopback-only test installation and the caller must explicitly opt in.
func validateCleanupTargets(consent, databaseURL, rabbitURL string) error {
	if consent != "YES" {
		return fmt.Errorf("%s must be exactly YES", destructiveCleanupConsent)
	}
	if err := validateDatabaseCleanupURL(databaseURL); err != nil {
		return err
	}
	if err := validateRabbitCleanupURL(rabbitURL); err != nil {
		return err
	}
	return nil
}

func validateDatabaseCleanupURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse E2E_DATABASE_URL: %w", err)
	}
	if parsed.Scheme != "postgres" && parsed.Scheme != "postgresql" {
		return errors.New("E2E_DATABASE_URL must use postgres or postgresql")
	}
	if !loopbackHost(parsed.Hostname()) {
		return errors.New("E2E_DATABASE_URL must point to localhost or a loopback address")
	}
	if parsed.User == nil || parsed.User.Username() != "payments" {
		return errors.New("E2E_DATABASE_URL must use the documented payments user")
	}
	password, hasPassword := parsed.User.Password()
	if !hasPassword || password != "payments_local" {
		return errors.New("E2E_DATABASE_URL must use the documented local password")
	}
	if parsed.Port() != "5432" && parsed.Port() != "55432" {
		return errors.New("E2E_DATABASE_URL must use the documented local or CI port")
	}
	if strings.TrimPrefix(parsed.EscapedPath(), "/") != "payments" {
		return errors.New("E2E_DATABASE_URL must select the documented payments database")
	}
	if parsed.Query().Get("sslmode") != "disable" {
		return errors.New("E2E_DATABASE_URL must explicitly set sslmode=disable")
	}
	return nil
}

func validateRabbitCleanupURL(raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse E2E_RABBITMQ_URL: %w", err)
	}
	if parsed.Scheme != "amqp" {
		return errors.New("E2E_RABBITMQ_URL must use amqp")
	}
	if !loopbackHost(parsed.Hostname()) {
		return errors.New("E2E_RABBITMQ_URL must point to localhost or a loopback address")
	}
	if parsed.User == nil || parsed.User.Username() != "payments" {
		return errors.New("E2E_RABBITMQ_URL must use the documented payments user")
	}
	password, hasPassword := parsed.User.Password()
	if !hasPassword || password != "payments_local" {
		return errors.New("E2E_RABBITMQ_URL must use the documented local password")
	}
	if parsed.Port() != "5672" {
		return errors.New("E2E_RABBITMQ_URL must use the documented AMQP port")
	}
	if parsed.Path != "" && parsed.Path != "/" {
		return errors.New("E2E_RABBITMQ_URL must select the documented root vhost")
	}
	return nil
}

func loopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func TestCleanupTargetValidation(t *testing.T) {
	databaseURL := "postgres://payments:payments_local@127.0.0.1:55432/payments?sslmode=disable"
	rabbitURL := "amqp://payments:payments_local@localhost:5672/"
	if err := validateCleanupTargets("YES", databaseURL, rabbitURL); err != nil {
		t.Fatalf("documented local targets rejected: %v", err)
	}

	tests := []struct {
		name, consent, databaseURL, rabbitURL string
	}{
		{"consent absent", "", databaseURL, rabbitURL},
		{"remote postgres", "YES", "postgres://payments:x@production.example/payments?sslmode=disable", rabbitURL},
		{"wrong database", "YES", "postgres://payments:x@localhost/production?sslmode=disable", rabbitURL},
		{"remote rabbitmq", "YES", databaseURL, "amqp://payments:x@broker.example/"},
		{"wrong vhost", "YES", databaseURL, "amqp://payments:x@localhost/production"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := validateCleanupTargets(test.consent, test.databaseURL, test.rabbitURL); err == nil {
				t.Fatal("validateCleanupTargets() error = nil, want refusal")
			}
		})
	}
}
