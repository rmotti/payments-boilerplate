package errsanitize

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSanitizeRedactsPostgresDSN(t *testing.T) {
	t.Parallel()
	in := "dial tcp: lookup failed for postgres://payments:s3cr3t-pass@db.internal:5432/payments?sslmode=disable"
	out := Sanitize(in)
	if strings.Contains(out, "s3cr3t-pass") {
		t.Fatalf("Sanitize(%q) = %q, still contains the password", in, out)
	}
	if strings.Contains(out, "payments:s3cr3t-pass@") {
		t.Fatalf("Sanitize(%q) = %q, still contains userinfo", in, out)
	}
	if !strings.Contains(out, Redacted) {
		t.Fatalf("Sanitize(%q) = %q, want the stable marker %q", in, out, Redacted)
	}
	if !strings.Contains(out, "db.internal:5432/payments") {
		t.Fatalf("Sanitize(%q) = %q, want the operationally useful host and path kept", in, out)
	}
}

func TestSanitizeRedactsAMQPCredentials(t *testing.T) {
	t.Parallel()
	in := "dial amqp: connect amqp://relay:hunter2@rabbitmq.internal:5672/ refused"
	out := Sanitize(in)
	if strings.Contains(out, "hunter2") || strings.Contains(out, "relay:hunter2@") {
		t.Fatalf("Sanitize(%q) = %q, still contains AMQP credentials", in, out)
	}
	if !strings.Contains(out, "rabbitmq.internal:5672") {
		t.Fatalf("Sanitize(%q) = %q, want the host kept", in, out)
	}
}

func TestSanitizeRedactsHTTPUserinfo(t *testing.T) {
	t.Parallel()
	in := "http request failed: https://svc-user:svc-pass@api.example.com/v1/charges: 502"
	out := Sanitize(in)
	if strings.Contains(out, "svc-pass") {
		t.Fatalf("Sanitize(%q) = %q, still contains the HTTP userinfo password", in, out)
	}
}

func TestSanitizeRedactsURLEncodedCredentials(t *testing.T) {
	t.Parallel()
	// A password containing an escaped '@' (%40) or ':' (%3A), as produced by
	// url.URL.String() when the credential itself has reserved characters.
	in := "postgres://payments:p%40ss%3Aword@db.internal:5432/payments failed"
	out := Sanitize(in)
	if strings.Contains(out, "p%40ss%3Aword") || strings.Contains(out, "payments:p%40ss") {
		t.Fatalf("Sanitize(%q) = %q, still contains the URL-encoded credential", in, out)
	}
}

func TestSanitizeRedactsAuthorizationHeader(t *testing.T) {
	t.Parallel()
	tests := []string{
		`request failed: Authorization: Bearer eyJhbGciOiJIUzI1NiJ9.abc.def`,
		`request failed: authorization=Bearer eyJhbGciOiJIUzI1NiJ9.abc.def`,
	}
	for _, in := range tests {
		out := Sanitize(in)
		if strings.Contains(out, "eyJhbGciOiJIUzI1NiJ9") {
			t.Fatalf("Sanitize(%q) = %q, still contains the bearer token", in, out)
		}
	}
}

func TestSanitizeRedactsXAPIKeyHeader(t *testing.T) {
	t.Parallel()
	in := "unexpected header X-API-Key: ik_live_abcdef0123456789 in upstream request"
	out := Sanitize(in)
	if strings.Contains(out, "ik_live_abcdef0123456789") {
		t.Fatalf("Sanitize(%q) = %q, still contains the API key value", in, out)
	}
}

func TestSanitizeRedactsCookieHeader(t *testing.T) {
	t.Parallel()
	in := "response included Set-Cookie: session=abcd1234; Path=/"
	out := Sanitize(in)
	if strings.Contains(out, "abcd1234") {
		t.Fatalf("Sanitize(%q) = %q, still contains the cookie value", in, out)
	}
}

func TestSanitizeRedactsCredentialLikeParameters(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		in   string
	}{
		{"password", "login failed: password=s3cret123 rejected"},
		{"passwd", "auth error passwd:hunter2 invalid"},
		{"token", "refresh failed: token=abcdef0123456789"},
		{"secret", "config error: secret=topsecretvalue not found"},
		{"api_key", "provider rejected api_key=abcdef0123456789"},
		{"api-key", "provider rejected api-key=abcdef0123456789"},
		{"access_key", "aws error: access_key=AKIAABCDEFGH1234"},
		{"client_secret", "oauth failed: client_secret=abcdef0123456789"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			out := Sanitize(tt.in)
			if !strings.Contains(out, Redacted) {
				t.Fatalf("Sanitize(%q) = %q, want redaction of the %s parameter", tt.in, out, tt.name)
			}
		})
	}
}

func TestSanitizeRedactsStripeSecretKey(t *testing.T) {
	t.Parallel()
	// The key-shaped fixtures are assembled at runtime rather than written as
	// literals: a literal sk_live_ / sk_test_ / rk_live_ string in a tracked
	// file trips GitHub push protection even when the body is obviously fake.
	const body = "51AbCdEfGhIjKlMnOpQrStUv"
	tests := []string{
		"stripe error using " + "sk" + "_live_" + body + " for request",
		"stripe error using " + "sk" + "_test_" + body + " for request",
		"stripe error using " + "rk" + "_live_" + body + " for request",
	}
	for _, in := range tests {
		out := Sanitize(in)
		if strings.Contains(out, body) {
			t.Fatalf("Sanitize(%q) = %q, still contains the Stripe secret key", in, out)
		}
	}
}

func TestSanitizeRedactsStripeWebhookSecret(t *testing.T) {
	t.Parallel()
	const body = "AbCdEfGhIjKlMnOpQrStUv1234"
	in := "signature verification failed with secret " + "whsec" + "_" + body
	out := Sanitize(in)
	if strings.Contains(out, body) {
		t.Fatalf("Sanitize(%q) = %q, still contains the webhook secret", in, out)
	}
}

func TestSanitizeRedactsStripeSignatureHeader(t *testing.T) {
	t.Parallel()
	in := "invalid header Stripe-Signature: t=1690000000,v1=abcdef0123456789abcdef0123456789,v0=abcdef0123456789"
	out := Sanitize(in)
	if strings.Contains(out, "abcdef0123456789abcdef0123456789") {
		t.Fatalf("Sanitize(%q) = %q, still contains the signature value", in, out)
	}
}

func TestSanitizeRedactsMalformedStripeSignatureHeader(t *testing.T) {
	t.Parallel()
	const sentinel = "malformed-signature-sentinel"
	in := "webhook rejected Stripe-Signature: " + sentinel
	out := Sanitize(in)
	if strings.Contains(out, sentinel) {
		t.Fatalf("Sanitize(%q) = %q, still contains the malformed signature", in, out)
	}
}

func TestSanitizeRedactsQuotedCredentialValues(t *testing.T) {
	t.Parallel()
	for _, in := range []string{
		`payload={"password":"secret value with spaces"}`,
		`config client_secret='quoted secret value'`,
	} {
		out := Sanitize(in)
		if strings.Contains(out, "secret value") {
			t.Fatalf("Sanitize(%q) = %q, still contains a quoted credential", in, out)
		}
		if !strings.Contains(out, Redacted) {
			t.Fatalf("Sanitize(%q) = %q, want %q", in, out, Redacted)
		}
	}
}

func TestSanitizePreservesUsefulOperationalText(t *testing.T) {
	t.Parallel()
	in := "context deadline exceeded while dialing db.internal:5432"
	out := Sanitize(in)
	if out != in {
		t.Fatalf("Sanitize(%q) = %q, want the text unchanged: it carries no credential", in, out)
	}
}

func TestSanitizeTruncatesAfterRedacting(t *testing.T) {
	t.Parallel()
	// The DSN starts the message, so truncating first (without sanitizing)
	// would still leave the password inside the first 500 bytes. Sanitizing
	// first removes it regardless of where in the string it falls.
	long := "postgres://payments:s3cr3t-pass@db.internal:5432/payments failed: " + strings.Repeat("x", 600)
	out := Sanitize(long)
	if strings.Contains(out, "s3cr3t-pass") {
		t.Fatalf("Sanitize(long DSN + padding) still contains the password")
	}
	if len(out) > MaxLength {
		t.Fatalf("Sanitize() length = %d, want at most %d", len(out), MaxLength)
	}
}

func TestSanitizeTruncationRespectsUTF8Boundaries(t *testing.T) {
	t.Parallel()
	// Build a string whose 500th byte would land inside a multi-byte rune if
	// cut naively. "é" is 2 bytes (0xC3 0xA9); repeating it crosses the
	// MaxLength boundary mid-rune for many possible prefix lengths.
	prefix := strings.Repeat("a", MaxLength-1)
	in := prefix + "é" + strings.Repeat("b", 50)
	out := Sanitize(in)
	if !utf8.ValidString(out) {
		t.Fatalf("Sanitize() produced invalid UTF-8: %q", out)
	}
	if len(out) > MaxLength {
		t.Fatalf("Sanitize() length = %d, want at most %d", len(out), MaxLength)
	}
}

func TestSanitizeRepairsInvalidUTF8AndNUL(t *testing.T) {
	t.Parallel()
	in := "driver failed: " + string([]byte{0xff, 0xfe}) + "\x00tail"
	out := Sanitize(in)
	if !utf8.ValidString(out) {
		t.Fatalf("Sanitize() produced invalid UTF-8: %q", out)
	}
	if strings.ContainsRune(out, '\x00') {
		t.Fatalf("Sanitize() kept a NUL byte: %q", out)
	}
	if !strings.Contains(out, Redacted) {
		t.Fatalf("Sanitize() = %q, want the replacement marker", out)
	}
}

func TestTruncateWithNonPositiveLimitReturnsEmpty(t *testing.T) {
	t.Parallel()
	for _, limit := range []int{0, -1} {
		if out := Truncate("text", limit); out != "" {
			t.Fatalf("Truncate(text, %d) = %q, want empty", limit, out)
		}
	}
}

func TestSanitizeHandlesEmptyString(t *testing.T) {
	t.Parallel()
	if out := Sanitize(""); out != "" {
		t.Fatalf("Sanitize(\"\") = %q, want empty", out)
	}
}

func TestSanitizeRedactsMultipleSecretsInOneMessage(t *testing.T) {
	t.Parallel()
	in := "publish failed: amqp://relay:hunter2@rabbitmq.internal:5672/ then retried with Authorization: Bearer abcd1234 and secret=topsecret"
	out := Sanitize(in)
	for _, leaked := range []string{"hunter2", "abcd1234", "topsecret"} {
		if strings.Contains(out, leaked) {
			t.Fatalf("Sanitize(%q) = %q, still contains %q", in, out, leaked)
		}
	}
}
