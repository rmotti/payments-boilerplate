//go:build e2e

package e2e

import (
	"compress/gzip"
	"context"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rmotti/payments-boilerplate/internal/platform/config"
	"github.com/rmotti/payments-boilerplate/internal/platform/metrics"
	metricspb "go.opentelemetry.io/proto/otlp/collector/metrics/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	otlpmetrics "go.opentelemetry.io/proto/otlp/metrics/v1"
	"google.golang.org/protobuf/proto"
)

// otlpCollector is a minimal OTLP/HTTP metrics receiver. The e2e suite points
// the composition's real exporter at it, so what the test asserts is what a
// collector would actually receive, not what an in-process reader saw.
type otlpCollector struct {
	server   *http.Server
	endpoint string

	mu      sync.Mutex
	metrics map[string][]*otlpmetrics.Metric
}

func startOTLPCollector(t *testing.T) *otlpCollector {
	t.Helper()

	listener, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen OTLP collector: %v", err)
	}
	collector := &otlpCollector{
		endpoint: "http://" + listener.Addr().String(),
		metrics:  make(map[string][]*otlpmetrics.Metric),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/metrics", collector.receive)
	// The composition also exports traces; accepting and discarding them keeps
	// the exporter from logging failures that would only be noise here.
	mux.HandleFunc("POST /v1/traces", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	collector.server = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}

	go func() { _ = collector.server.Serve(listener) }()
	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = collector.server.Shutdown(shutdownCtx)
	})
	return collector
}

func (c *otlpCollector) receive(w http.ResponseWriter, r *http.Request) {
	body := io.Reader(r.Body)
	if r.Header.Get("Content-Encoding") == "gzip" {
		reader, err := gzip.NewReader(r.Body)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		defer func() { _ = reader.Close() }()
		body = reader
	}
	data, err := io.ReadAll(body)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	var request metricspb.ExportMetricsServiceRequest
	if err := proto.Unmarshal(data, &request); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	c.mu.Lock()
	for _, resource := range request.GetResourceMetrics() {
		for _, scope := range resource.GetScopeMetrics() {
			for _, metric := range scope.GetMetrics() {
				c.metrics[metric.GetName()] = append(c.metrics[metric.GetName()], metric)
			}
		}
	}
	c.mu.Unlock()

	response, err := proto.Marshal(&metricspb.ExportMetricsServiceResponse{})
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/x-protobuf")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(response)
}

// series reports the attribute sets exported for one instrument, each rendered
// as a sorted "key=value" list so a test can assert on them directly.
func (c *otlpCollector) series(name string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	seen := make(map[string]struct{})
	for _, metric := range c.metrics[name] {
		for _, attributes := range attributeSetsOf(metric) {
			seen[attributes] = struct{}{}
		}
	}
	rendered := make([]string, 0, len(seen))
	for attributes := range seen {
		rendered = append(rendered, attributes)
	}
	sort.Strings(rendered)
	return rendered
}

// names reports every instrument the collector has received.
func (c *otlpCollector) names() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	listed := make([]string, 0, len(c.metrics))
	for name := range c.metrics {
		listed = append(listed, name)
	}
	sort.Strings(listed)
	return listed
}

func attributeSetsOf(metric *otlpmetrics.Metric) []string {
	var sets []string
	switch data := metric.GetData().(type) {
	case *otlpmetrics.Metric_Sum:
		for _, point := range data.Sum.GetDataPoints() {
			sets = append(sets, renderAttributes(point.GetAttributes()))
		}
	case *otlpmetrics.Metric_Gauge:
		for _, point := range data.Gauge.GetDataPoints() {
			sets = append(sets, renderAttributes(point.GetAttributes()))
		}
	case *otlpmetrics.Metric_Histogram:
		for _, point := range data.Histogram.GetDataPoints() {
			sets = append(sets, renderAttributes(point.GetAttributes()))
		}
	}
	return sets
}

func renderAttributes(attributes []*commonpb.KeyValue) string {
	rendered := make([]string, 0, len(attributes))
	for _, attribute := range attributes {
		rendered = append(rendered, attribute.GetKey()+"="+attribute.GetValue().GetStringValue())
	}
	sort.Strings(rendered)
	return strings.Join(rendered, ",")
}

// A full run of the pipeline has to produce the series an operator will build
// alerts on. This is the end-to-end half of the metrics work: the observers
// are unit-tested against an in-memory reader, and here the real composition
// exports through the real OTLP exporter.
func TestFullFlowExportsTheExpectedSeries(t *testing.T) {
	collector := startOTLPCollector(t)
	h := newMetricsHarness(t, collector)

	order := h.createOrder(apiKeyA, "metrics-order", map[string]any{"productId": "product_demo", "quantity": 1})
	// A repeated request with the same key is an idempotent replay, which has
	// its own counter and must not reach the provider a second time.
	h.createOrder(apiKeyA, "metrics-order", map[string]any{"productId": "product_demo", "quantity": 1})
	h.createCheckout(order.ID, "metrics-checkout")
	paymentID, attemptID, sessionID, amount, currency := h.paymentReferences(order.ID)

	paid := stripeEventPayload(t, stripeEvent{
		ProviderEventID: "evt_e2e_metrics_paid", Type: "checkout.session.completed", SessionID: sessionID,
		OrderID: order.ID, PaymentID: paymentID, AttemptID: attemptID,
		Amount: amount, Currency: currency, PaymentStatus: "paid",
	})
	response, body := h.postWebhook(paid, true)
	requireStatus(t, response, body, http.StatusAccepted)

	// An invalid signature is the other half of the receiving path, and the
	// counter behind the alert an operator actually wants.
	response, body = h.postWebhook(paid, false)
	requireStatus(t, response, body, http.StatusBadRequest)

	// A body rejected by the transport still has to cross the application
	// observer boundary as a classified failure. No partial bytes reach
	// verification, but the operational signal must not disappear.
	oversized := []byte(`{"padding":"` + strings.Repeat("a", 513<<10) + `"}`)
	response, body = h.postWebhook(oversized, true)
	requireStatus(t, response, body, http.StatusInternalServerError)

	h.waitAggregate("metrics flow settled", order.ID, "evt_e2e_metrics_paid", func(state aggregateState) bool {
		return state.Order == "paid" && state.Payment == "succeeded" &&
			state.Inbox == "processed" && state.Outbox == "published"
	})
	h.waitQueues("metrics pipeline drained", 0, 0)

	// Stopping the processes flushes their exporters, so the collector holds
	// everything the run produced rather than only the last export interval.
	h.stop()

	if len(collector.names()) == 0 {
		t.Fatal("the collector received no metrics at all; OTLP export was not exercised")
	}
	t.Logf("collector received %d instruments", len(collector.names()))

	wantSeries := map[string][]string{
		metrics.WebhookReceived: {
			"event.kind=checkout.completed,outcome=accepted,provider=stripe",
		},
		metrics.WebhookInvalidSignature: {"provider=stripe"},
		metrics.WebhookReceiveFailures:  {"provider=stripe,reason=storage"},
		metrics.IdempotencyReplays:      {"operation=create_order"},
		metrics.ProviderRequestDuration: {
			"operation=create_checkout,outcome=success,provider=stripe",
		},
		metrics.StateTransitions: {
			"entity=attempt,from=pending,to=succeeded",
			"entity=order,from=pending,to=paid",
			"entity=payment,from=pending,to=succeeded",
		},
		metrics.OutboxPublishDuration:  {"outcome=published"},
		metrics.ConsumerHandleDuration: {"disposition=done"},
		metrics.OutboxRelayCycleTime:   {"outcome=empty", "outcome=published"},
	}
	for name, want := range wantSeries {
		got := collector.series(name)
		for _, series := range want {
			if !containsSeries(got, series) {
				t.Errorf("%s did not export %q; exported %v", name, series, got)
			}
		}
	}

	// The gauges come from the background samplers, so their presence proves
	// the samplers ran and published inside a real process.
	for _, name := range []string{
		metrics.OutboxPending, metrics.OutboxOldestAge, metrics.InboxPending,
		metrics.InboxFailed, metrics.InboxOldestAge, metrics.PoolConnections,
		metrics.PoolMaxOpen, metrics.SamplerAge,
	} {
		if len(collector.series(name)) == 0 {
			t.Errorf("gauge %s was never exported; exported instruments: %v", name, collector.names())
		}
	}
	for _, queue := range []string{"payments.webhooks", "payments.webhooks.dlq"} {
		if !containsSeries(collector.series(metrics.QueueDepth), "queue="+queue) {
			t.Errorf("queue depth was not exported for %s; got %v", queue, collector.series(metrics.QueueDepth))
		}
	}
	if !containsSeries(collector.series(metrics.PoolConnections), "state=in_use") ||
		!containsSeries(collector.series(metrics.PoolConnections), "state=idle") {
		t.Errorf("pool usage was not exported by state; got %v", collector.series(metrics.PoolConnections))
	}

	// Nothing exported may carry an identifier of the run, which the whole
	// design of the label allowlist exists to guarantee.
	forbidden := []string{order.ID, paymentID, attemptID, sessionID, "evt_e2e_metrics_paid", webhookSecret}
	for _, name := range collector.names() {
		for _, series := range collector.series(name) {
			for _, needle := range forbidden {
				if needle != "" && strings.Contains(series, needle) {
					t.Errorf("%s exported %q, which carries the identifier %q", name, series, needle)
				}
			}
		}
	}
}

func containsSeries(got []string, want string) bool {
	for _, series := range got {
		if series == want {
			return true
		}
		// A gauge or counter may carry more labels than the caller named.
		if strings.Contains(series, want) {
			return true
		}
	}
	return false
}

// newMetricsHarness starts the same composition the other scenarios use, with
// OTLP export enabled and pointed at the in-process collector. Sampling runs
// fast so a scenario measured in seconds still sees several rounds.
func newMetricsHarness(t *testing.T, collector *otlpCollector) *harness {
	t.Helper()
	previous := metricsRuntimeOverrides
	metricsRuntimeOverrides = func(cfg *config.Config) {
		cfg.OTelEnabled = true
		cfg.OTelExporterEndpoint = collector.endpoint
		cfg.OTelExportInterval = 500 * time.Millisecond
		cfg.MetricsSampleInterval = 500 * time.Millisecond
		cfg.MetricsSampleTimeout = 250 * time.Millisecond
	}
	t.Cleanup(func() { metricsRuntimeOverrides = previous })
	return newHarness(t, true)
}
