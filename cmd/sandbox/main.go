// Command sandbox drives local, reproducible Stripe test-mode flows.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/rmotti/payments-boilerplate/internal/platform/logging"
	"github.com/rmotti/payments-boilerplate/internal/sandbox"
)

const cleanupConfirmation = "delete-local-sandbox-order"

func main() {
	if err := execute(); err != nil {
		_ = logging.WriteSanitizedError(os.Stderr, err)
		os.Exit(1)
	}
}

func execute() error {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return run(ctx, os.Args[1:])
}

func run(ctx context.Context, arguments []string) error {
	if len(arguments) == 0 {
		return usageError()
	}
	cfg, err := sandbox.LoadConfig()
	if err != nil {
		return err
	}

	switch arguments[0] {
	case "doctor":
		return runDoctor(ctx, cfg, arguments[1:])
	case "checkout":
		return runCheckout(ctx, cfg, arguments[1:])
	case "emit":
		return runEmit(ctx, cfg, arguments[1:])
	case "status":
		return runStatus(ctx, cfg, arguments[1:])
	case "wait":
		return runWait(ctx, cfg, arguments[1:])
	case "cleanup":
		return runCleanup(ctx, cfg, arguments[1:])
	case "help", "-h", "--help":
		printUsage(os.Stdout)
		return nil
	default:
		return fmt.Errorf("unknown sandbox command %q: %w", arguments[0], usageError())
	}
}

func runDoctor(ctx context.Context, cfg sandbox.Config, arguments []string) error {
	flags := newFlagSet("doctor")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("doctor does not accept positional arguments")
	}
	if err := cfg.Validate(sandbox.Requirements{
		API: true, Database: true, Stripe: true, WebhookSecret: true,
	}); err != nil {
		return err
	}
	if _, err := exec.LookPath("stripe"); err != nil {
		return errors.New("stripe CLI was not found in PATH")
	}

	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	client := sandbox.NewClient(cfg)
	if err := client.Health(checkCtx); err != nil {
		return fmt.Errorf("API health check: %w", err)
	}
	if err := client.CheckAuthentication(checkCtx); err != nil {
		return fmt.Errorf("API authentication check: %w", err)
	}
	store, err := sandbox.OpenStore(checkCtx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	return writeJSON(map[string]any{
		"api": "ready", "database": "ready", "environment": cfg.Environment,
		"stripeCLI": "available", "stripeMode": "test", "webhookSecret": "configured",
	})
}

func runCheckout(ctx context.Context, cfg sandbox.Config, arguments []string) error {
	flags := newFlagSet("checkout")
	quantity := flags.Int("quantity", 1, "demo product quantity")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("checkout does not accept positional arguments")
	}
	if err := cfg.Validate(sandbox.Requirements{API: true, Stripe: true}); err != nil {
		return err
	}
	requestCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	result, err := sandbox.NewClient(cfg).CreateCheckout(requestCtx, *quantity)
	if err != nil {
		return err
	}
	return writeJSON(result)
}

func runEmit(ctx context.Context, cfg sandbox.Config, arguments []string) error {
	flags := newFlagSet("emit")
	orderID := flags.String("order", "", "sandbox order id")
	scenarioName := flags.String("scenario", "", "fixture scenario: "+strings.Join(sandbox.ScenarioNames(), ", "))
	eventID := flags.String("event-id", "", "provider event id; generated when omitted")
	wait := flags.Duration("wait", 20*time.Second, "maximum time to wait for the asynchronous result; zero disables waiting")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || *orderID == "" || *scenarioName == "" {
		return errors.New("emit requires --order and --scenario and accepts no positional arguments")
	}
	if *wait < 0 {
		return errors.New("emit --wait must not be negative")
	}
	if err := cfg.Validate(sandbox.Requirements{API: true, Database: true, WebhookSecret: true}); err != nil {
		return err
	}

	store, err := openStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	reference, err := store.Reference(ctx, *orderID)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	payload, scenario, resolvedEventID, err := sandbox.RenderEvent(*scenarioName, *eventID, reference, now)
	if err != nil {
		return err
	}
	signature, err := sandbox.StripeSignature(payload, cfg.WebhookSecret, now)
	if err != nil {
		return err
	}
	if err := sandbox.NewClient(cfg).PostEvent(ctx, payload, signature); err != nil {
		return err
	}

	result := struct {
		EventID  string                  `json:"providerEventId"`
		Scenario string                  `json:"scenario"`
		State    sandbox.AggregateStatus `json:"state,omitempty"`
	}{EventID: resolvedEventID, Scenario: scenario.Name}
	if *wait > 0 {
		waitCtx, cancel := context.WithTimeout(ctx, *wait)
		defer cancel()
		result.State, err = store.WaitForScenario(waitCtx, *orderID, resolvedEventID, scenario)
		if err != nil {
			_ = writeJSON(result)
			return err
		}
	}
	return writeJSON(result)
}

func runStatus(ctx context.Context, cfg sandbox.Config, arguments []string) error {
	flags := newFlagSet("status")
	orderID := flags.String("order", "", "sandbox order id")
	eventID := flags.String("event-id", "", "optional provider event id")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || *orderID == "" {
		return errors.New("status requires --order and accepts no positional arguments")
	}
	if err := cfg.Validate(sandbox.Requirements{Database: true}); err != nil {
		return err
	}
	store, err := openStore(ctx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	status, err := store.Status(ctx, *orderID, *eventID)
	if err != nil {
		return err
	}
	return writeJSON(status)
}

func runWait(ctx context.Context, cfg sandbox.Config, arguments []string) error {
	flags := newFlagSet("wait")
	orderID := flags.String("order", "", "sandbox order id")
	expected := flags.String("status", "paid", "expected public order status")
	timeout := flags.Duration("timeout", 2*time.Minute, "maximum wait")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || *orderID == "" {
		return errors.New("wait requires --order and accepts no positional arguments")
	}
	if *timeout <= 0 {
		return errors.New("wait --timeout must be positive")
	}
	if err := cfg.Validate(sandbox.Requirements{API: true}); err != nil {
		return err
	}
	waitCtx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	order, err := sandbox.WaitForOrder(waitCtx, sandbox.NewClient(cfg), *orderID, *expected)
	if err != nil {
		return err
	}
	return writeJSON(order)
}

func runCleanup(ctx context.Context, cfg sandbox.Config, arguments []string) error {
	flags := newFlagSet("cleanup")
	orderID := flags.String("order", "", "sandbox order id")
	confirmation := flags.String("confirm", "", "required local deletion confirmation")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 || *orderID == "" {
		return errors.New("cleanup requires --order and accepts no positional arguments")
	}
	if *confirmation != cleanupConfirmation {
		return fmt.Errorf("cleanup requires --confirm=%s", cleanupConfirmation)
	}
	if err := cfg.Validate(sandbox.Requirements{Database: true}); err != nil {
		return err
	}
	cleanupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	store, err := openStore(cleanupCtx, cfg)
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	result, err := store.Cleanup(cleanupCtx, *orderID)
	if err != nil {
		return err
	}
	return writeJSON(result)
}

func openStore(ctx context.Context, cfg sandbox.Config) (*sandbox.Store, error) {
	openCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return sandbox.OpenStore(openCtx, cfg.DatabaseURL)
}

func newFlagSet(name string) *flag.FlagSet {
	flags := flag.NewFlagSet(name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	return flags
}

func writeJSON(value any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		return fmt.Errorf("write sandbox output: %w", err)
	}
	return nil
}

func usageError() error {
	return errors.New("usage: sandbox <doctor|checkout|emit|status|wait|cleanup>; run sandbox help for details")
}

func printUsage(output io.Writer) {
	_, _ = fmt.Fprintln(output, `Usage: sandbox <command> [options]

Commands:
  doctor    validate local dependencies and test-mode credentials
  checkout  create a product_demo order and hosted Stripe Checkout
  emit      send a signed local fixture through the webhook pipeline
  status    show persisted order, payment, attempt and event state
  wait      wait for a hosted Checkout to change the public order state
  cleanup   delete one terminal local order created by this tool`)
}
