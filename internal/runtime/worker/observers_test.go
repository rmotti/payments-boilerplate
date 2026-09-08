package worker

import (
	"errors"
	"strings"
	"testing"

	"github.com/rmotti/payments-boilerplate/internal/platform/errsanitize"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// TestObserversNeverLogCauseVerbatim is a negative test with unique sentinels
// covering every observer that logs a cause coming from the broker or the
// database. A relay or consumer failure can carry a RabbitMQ or PostgreSQL
// connection string verbatim in err.Error(); this pins that none of the
// captured log entries or their fields ever contain the sentinel, and that
// the sanitized field carries the stable redaction marker instead.
func TestObserversNeverLogCauseVerbatim(t *testing.T) {
	t.Parallel()

	const sentinel = "sentinel-observer-3E9F7C"
	cause := errors.New("dial amqp://relay:" + sentinel + "@rabbitmq.internal:5672/ refused")

	core, logs := observer.New(zapcore.InfoLevel)
	logger := zap.New(core)

	relay := relayObserver{logger: logger}
	relay.Stuck("msg-1", 3, cause)
	relay.Abandoned("msg-2", 5, cause)
	relay.LeaseLost("msg-3", cause)

	consumer := consumerObserver{logger: logger}
	consumer.Retrying("evt-1", 2, cause)
	consumer.DeadLettered("evt-2", 5, cause)

	broker := brokerObserver{logger: logger}
	broker.Rejected("msg-4", cause)
	broker.Failed("msg-5", cause)

	entries := logs.All()
	if len(entries) != 7 {
		t.Fatalf("captured %d log entries, want 7", len(entries))
	}
	for _, entry := range entries {
		if strings.Contains(entry.Message, sentinel) {
			t.Fatalf("log message = %q, leaked the sentinel credential", entry.Message)
		}
		errorField, ok := entry.ContextMap()["error"].(string)
		if !ok {
			t.Fatalf("entry %q has no string error field: %#v", entry.Message, entry.ContextMap())
		}
		if strings.Contains(errorField, sentinel) {
			t.Fatalf("entry %q error field = %q, leaked the sentinel credential", entry.Message, errorField)
		}
		if !strings.Contains(errorField, errsanitize.Redacted) {
			t.Fatalf("entry %q error field = %q, want the stable redaction marker", entry.Message, errorField)
		}
	}
}
