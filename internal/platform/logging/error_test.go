package logging

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/rmotti/payments-boilerplate/internal/platform/errsanitize"
)

func TestSanitizedTextProtectsPlainProcessOutput(t *testing.T) {
	t.Parallel()
	const sentinel = "startup-secret-sentinel"
	err := errors.New("open postgres://payments:" + sentinel + "@db.internal/payments")

	got := SanitizedText(err)
	if strings.Contains(got, sentinel) {
		t.Fatalf("SanitizedText() = %q, leaked the startup credential", got)
	}
	if !strings.Contains(got, errsanitize.Redacted) {
		t.Fatalf("SanitizedText() = %q, want %q", got, errsanitize.Redacted)
	}
}

func TestSanitizedTextHandlesNil(t *testing.T) {
	t.Parallel()
	if got := SanitizedText(nil); got != "" {
		t.Fatalf("SanitizedText(nil) = %q, want empty", got)
	}
}

func TestWriteSanitizedErrorProtectsStderrOutput(t *testing.T) {
	t.Parallel()
	const sentinel = "stderr-secret-sentinel"
	var output bytes.Buffer

	if err := WriteSanitizedError(&output, errors.New("connect amqp://payments:"+sentinel+"@rabbitmq:5672")); err != nil {
		t.Fatalf("WriteSanitizedError() error = %v", err)
	}
	if got := output.String(); strings.Contains(got, sentinel) {
		t.Fatalf("WriteSanitizedError() = %q, leaked the startup credential", got)
	} else if !strings.Contains(got, errsanitize.Redacted) {
		t.Fatalf("WriteSanitizedError() = %q, want %q", got, errsanitize.Redacted)
	}
}
