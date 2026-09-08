// Package errsanitize redacts credentials and personal data out of error
// messages before they are persisted or logged.
//
// An error produced by a database driver, an AMQP client or an HTTP client
// can carry a connection string, a header value or a fragment of a request
// body verbatim. Nothing in the Go standard library or the drivers this
// project uses stops that text from reaching err.Error(), so redaction has to
// happen here, once, before the text goes anywhere else — a log, a trace, or
// the last_error column that the operational API returns to an integrator.
package errsanitize

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// Redacted replaces whatever this package removes. It is a stable marker so
// a caller can distinguish "the error said nothing here" from "the error
// said something, but it looked like a secret."
const Redacted = "[REDACTED]"

// MaxLength is the byte budget error text is held to after sanitizing. It
// matches the column width last_error has occupied since the outbox and inbox
// schemas were introduced; changing it is a schema decision, not a call site
// decision.
const MaxLength = 500

// patterns run in order. Order matters: URL credentials must be stripped
// before the generic key=value pass, otherwise a password containing "&" or
// "=" could leave a fragment behind once the URL structure is gone.
var patterns = []*regexp.Regexp{
	// scheme://user:password@host — postgres, postgresql, amqp, amqps, http,
	// https, mysql, redis, mongodb. The credential is URL-encoded per RFC 3986,
	// so a password containing "%40" or similar survives unescaped by this
	// pattern (it does not need "@" itself to end the match).
	regexp.MustCompile(`(?i)\b(postgres(?:ql)?|amqps?|https?|mysql|redis|mongodb(?:\+srv)?)://[^/@\s]+@`),

	// Authorization and API-key-shaped headers rendered inside an error, e.g.
	// "Authorization: Bearer abcd" or "X-Api-Key=abcd" produced by an HTTP
	// client's error formatting. The value runs to the next comma, semicolon
	// or line break rather than stopping at the first space, because schemes
	// like "Bearer <token>" and "Basic <token>" carry the credential after a
	// space.
	regexp.MustCompile(`(?i)\b(authorization|x-api-key|proxy-authorization|cookie|set-cookie)\s*[:=]\s*[^,;\n]+`),

	// Stripe-Signature is its own header pattern because commas are part of its
	// value. Matching from the header name also redacts malformed or future
	// formats rather than protecting only the canonical t=...,v1=... spelling.
	regexp.MustCompile(`(?i)\bstripe-signature\s*[:=]\s*[^;\n]+`),

	// key=value or key: value pairs naming a credential, in querystrings,
	// form bodies or free-form driver errors. \S+ stops at whitespace, "&"
	// stays inside because query separators are handled by consuming the
	// whole run up to the next actual space.
	regexp.MustCompile(`(?i)\b(password|passwd|pwd|token|secret|api[_-]?key|access[_-]?key|client[_-]?secret)\b["']?\s*[:=]\s*("[^"]*"|'[^']*'|[^\s&,;}]+)`),

	// Stripe secret and restricted keys: sk_live_*, sk_test_*, rk_live_*,
	// rk_test_*. These authenticate as the platform account and must never
	// appear even partially.
	regexp.MustCompile(`\b[sr]k_(live|test)_[A-Za-z0-9]+\b`),

	// Stripe webhook signing secret.
	regexp.MustCompile(`\bwhsec_[A-Za-z0-9]+\b`),

	// Stripe-Signature header value, e.g. "t=169..,v1=abcd..,v0=abcd..". The
	// timestamp alone is not sensitive, but the whole header is redacted as a
	// unit to avoid parsing a provider-controlled format inside an error path.
	regexp.MustCompile(`\bt=\d+(?:,v\d+=[A-Za-z0-9]+)+\b`),
}

// Sanitize redacts credentials and structured secrets from text, then
// truncates the result to MaxLength bytes without splitting a UTF-8 rune.
//
// It does not attempt general PII detection: free-form names, emails or
// addresses that reach an error message are not caught by pattern matching.
// Callers that hand user-supplied or provider-supplied payloads to error
// values are responsible for keeping that data out in the first place; this
// function is the second line of defense for the shapes of secret that
// actually appear in driver, broker and HTTP client errors.
func Sanitize(text string) string {
	text = validText(text)
	for _, pattern := range patterns {
		text = pattern.ReplaceAllString(text, Redacted)
	}
	return Truncate(text, MaxLength)
}

// Truncate cuts s to at most maxBytes bytes, stepping back to the nearest
// UTF-8 rune boundary rather than splitting one. A truncated multi-byte rune
// would leave invalid UTF-8 in a column that later has to be read back as
// text. It is exported so callers that truncate trusted, non-error text
// (values that do not need Sanitize's redaction) still share this rule
// instead of re-implementing byte-slicing themselves.
func Truncate(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	s = validText(s)
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// validText makes arbitrary error strings safe for PostgreSQL TEXT and JSON
// logs. Go permits invalid UTF-8 and NUL bytes inside a string; PostgreSQL does
// not accept either in text values, and emitting them also breaks downstream
// log processors. Consecutive invalid bytes and every NUL become the same stable
// marker used for redaction.
func validText(s string) string {
	s = strings.ToValidUTF8(s, Redacted)
	return strings.ReplaceAll(s, "\x00", Redacted)
}
