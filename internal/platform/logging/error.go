package logging

import (
	"fmt"
	"io"

	"github.com/rmotti/payments-boilerplate/internal/platform/errsanitize"
	"go.uber.org/zap"
)

// SanitizedError renders err as a log field the same way zap.Error does,
// except the message is passed through errsanitize.Sanitize first.
//
// A database driver, an AMQP client or an HTTP client error can carry a DSN,
// a header value or a Stripe key verbatim in its Error() text. zap.Error
// keeps that text unmodified; this is the field to use instead whenever the
// cause can plausibly have come from one of those clients, so a person with
// only log access does not see more than last_error would already show them.
//
// It does not replace error handling: callers still use errors.Is and
// errors.As on the original error for control flow. Only the string this
// field renders is sanitized.
func SanitizedError(err error) zap.Field {
	if err == nil {
		return zap.Skip()
	}
	return zap.String("error", SanitizedText(err))
}

// SanitizedText is the plain-text equivalent of SanitizedError. Process entry
// points use it before writing a fatal startup error to stderr, which container
// runtimes collect as a log even though it did not pass through zap.
func SanitizedText(err error) string {
	if err == nil {
		return ""
	}
	return errsanitize.Sanitize(err.Error())
}

// WriteSanitizedError writes one sanitized error line. Command entry points use
// it for startup failures because stderr is normally collected as a log by the
// process supervisor, outside zap's field sanitization.
func WriteSanitizedError(w io.Writer, err error) error {
	_, writeErr := fmt.Fprintln(w, SanitizedText(err))
	return writeErr
}
