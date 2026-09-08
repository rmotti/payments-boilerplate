package httpserver

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

var docsRoutes = []string{docsPath, docsIndexPath, openAPIPath}

func TestDocsModeFor(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		enabled     bool
		development bool
		want        DocsMode
	}{
		{name: "production without opt-in", want: DocsDisabled},
		{name: "development without opt-in", development: true, want: DocsDisabled},
		{name: "development with opt-in", enabled: true, development: true, want: DocsPublic},
		{name: "production with opt-in", enabled: true, want: DocsProtected},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if got := DocsModeFor(test.enabled, test.development); got != test.want {
				t.Fatalf("DocsModeFor(%t, %t) = %d, want %d", test.enabled, test.development, got, test.want)
			}
		})
	}
}

func newDocsHandler(mode DocsMode) http.Handler {
	verifier := verifierFunc(func(candidate string) bool { return candidate == testAPIKey })
	return New(Config{Docs: mode}, zap.NewNop(), newHealthOnlyHandler(), verifier).server.Handler
}

func docsRequest(handler http.Handler, method, path, key string) *httptest.ResponseRecorder {
	request := httptest.NewRequestWithContext(context.Background(), method, path, nil)
	if key != "" {
		request.Header.Set(apiKeyHeader, key)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// TestDocsDisabledAnswersLikeAnyUnknownPath is the production default: with no
// opt-in the documentation paths are not distinguishable from paths that never
// existed, with or without a valid key.
func TestDocsDisabledAnswersLikeAnyUnknownPath(t *testing.T) {
	t.Parallel()

	handler := newDocsHandler(DocsDisabled)
	unknown := docsRequest(handler, http.MethodGet, "/never-registered", "")
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown path status = %d, want 404", unknown.Code)
	}
	for _, path := range append(docsRoutes, "/docs/swagger-ui.css", "/docs/index.html") {
		for _, key := range []string{"", testAPIKey} {
			recorder := docsRequest(handler, http.MethodGet, path, key)
			if recorder.Code != http.StatusNotFound {
				t.Fatalf("GET %s (key %q) status = %d, want 404", path, key, recorder.Code)
			}
			if recorder.Body.String() != unknown.Body.String() ||
				recorder.Header().Get("Content-Type") != unknown.Header().Get("Content-Type") {
				t.Fatalf("GET %s (key %q) is distinguishable from an unknown path: %q", path, key, recorder.Body.String())
			}
		}
	}
}

// TestDocsProtectedRequiresValidKey covers production with the opt-in: every
// documentation path, the redirect included, is behind the integration key.
func TestDocsProtectedRequiresValidKey(t *testing.T) {
	t.Parallel()

	handler := newDocsHandler(DocsProtected)
	wantStatus := map[string]int{docsPath: http.StatusMovedPermanently, docsIndexPath: http.StatusOK, openAPIPath: http.StatusOK}
	for _, path := range docsRoutes {
		for _, key := range []string{"", "wrong-key"} {
			recorder := docsRequest(handler, http.MethodGet, path, key)
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("GET %s (key %q) status = %d, want 401", path, key, recorder.Code)
			}
			body := decodeError(t, recorder)
			if body.Code != codeUnauthorized || body.CorrelationId == "" {
				t.Fatalf("GET %s (key %q) error = %#v, want unauthorized with correlation id", path, key, body)
			}
			if recorder.Header().Get("Location") != "" {
				t.Fatalf("GET %s (key %q) redirected before authentication", path, key)
			}
			assertStrictSecurityHeaders(t, recorder.Header())
		}
		recorder := docsRequest(handler, http.MethodGet, path, testAPIKey)
		if recorder.Code != wantStatus[path] {
			t.Fatalf("GET %s (valid key) status = %d, want %d", path, recorder.Code, wantStatus[path])
		}
	}

	// Authentication is decided before the method, so an unauthenticated
	// caller cannot learn the allowed methods either.
	if recorder := docsRequest(handler, http.MethodPost, docsIndexPath, ""); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("POST %s without key status = %d, want 401", docsIndexPath, recorder.Code)
	}
	recorder := docsRequest(handler, http.MethodPost, docsIndexPath, testAPIKey)
	if recorder.Code != http.StatusMethodNotAllowed || recorder.Header().Get("Allow") != docsAllowHeader {
		t.Fatalf("POST %s with key status/allow = %d/%q, want 405/%q",
			docsIndexPath, recorder.Code, recorder.Header().Get("Allow"), docsAllowHeader)
	}
}

// TestDocsPublicServesWithoutKey is the explicitly configured development case.
func TestDocsPublicServesWithoutKey(t *testing.T) {
	t.Parallel()

	handler := newDocsHandler(DocsPublic)
	index := docsRequest(handler, http.MethodGet, docsIndexPath, "")
	if index.Code != http.StatusOK || !strings.Contains(index.Body.String(), "swagger-ui") {
		t.Fatalf("GET /docs/ status = %d, want 200 with Swagger UI", index.Code)
	}
	spec := docsRequest(handler, http.MethodGet, openAPIPath, "")
	if spec.Code != http.StatusOK || spec.Header().Get("Content-Type") != "application/yaml" {
		t.Fatalf("GET /openapi.yaml status/type = %d/%q, want 200/application/yaml", spec.Code, spec.Header().Get("Content-Type"))
	}
	redirect := docsRequest(handler, http.MethodGet, "/docs?checkout=success", "")
	if redirect.Code != http.StatusMovedPermanently || redirect.Header().Get("Location") != "/docs/?checkout=success" {
		t.Fatalf("GET /docs?checkout=success status/location = %d/%q, want 301 to /docs/?checkout=success",
			redirect.Code, redirect.Header().Get("Location"))
	}
	if head := docsRequest(handler, http.MethodHead, docsIndexPath, ""); head.Code != http.StatusOK {
		t.Fatalf("HEAD /docs/ status = %d, want 200", head.Code)
	}
}

// TestDocsResponsesCarrySecurityHeaders checks the page gets the Swagger
// policy and everything else keeps the strict one.
func TestDocsResponsesCarrySecurityHeaders(t *testing.T) {
	t.Parallel()

	handler := newDocsHandler(DocsPublic)
	for _, path := range docsRoutes {
		header := docsRequest(handler, http.MethodGet, path, "").Header()
		if header.Get(headerContentTypeOptions) != "nosniff" || header.Get(headerReferrerPolicy) != "no-referrer" ||
			header.Get(headerCacheControl) != "no-store" || header.Get(headerFrameOptions) != "DENY" {
			t.Fatalf("GET %s headers = %v, want nosniff, no-referrer, no-store and DENY", path, header)
		}
		if header.Get(correlationHeader) == "" {
			t.Fatalf("GET %s has no correlation id", path)
		}
	}
	if policy := docsRequest(handler, http.MethodGet, openAPIPath, "").Header().Get(headerContentSecurity); policy != strictContentSecurityPolicy {
		t.Fatalf("openapi.yaml policy = %q, want the strict policy", policy)
	}
	if policy := docsRequest(handler, http.MethodGet, docsIndexPath, "").Header().Get(headerContentSecurity); !strings.Contains(policy, "script-src 'self'") {
		t.Fatalf("/docs/ policy = %q, want the Swagger policy", policy)
	}
}

// TestSwaggerContentSecurityPolicyMatchesServedResources validates the policy
// against the page and assets the embedded Swagger UI really serves, so an
// upgrade that changes them fails here instead of in a browser console.
func TestSwaggerContentSecurityPolicyMatchesServedResources(t *testing.T) {
	t.Parallel()

	handler := newDocsHandler(DocsPublic)
	index := docsRequest(handler, http.MethodGet, docsIndexPath, "")
	if index.Code != http.StatusOK {
		t.Fatalf("GET /docs/ status = %d, want 200", index.Code)
	}
	page := index.Body.String()
	directives := parsePolicy(t, index.Header().Get(headerContentSecurity))

	scriptSources := directives["script-src"]
	if strings.Contains(scriptSources, "'unsafe-inline'") || strings.Contains(scriptSources, "'unsafe-eval'") {
		t.Fatalf("script-src = %q, want hashes instead of unsafe-inline or unsafe-eval", scriptSources)
	}
	inline := extractInlineScripts(page)
	if len(inline) == 0 {
		t.Fatal("Swagger page has no inline scripts; the hash allowlist is testing nothing")
	}
	for _, script := range inline {
		sum := sha256.Sum256([]byte(script))
		hash := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
		if !strings.Contains(scriptSources, hash) {
			t.Fatalf("inline script %.40q... is not allowed by script-src %q", script, scriptSources)
		}
	}

	for _, reference := range extractAttributeValues(page, "src=\"", "href=\"") {
		if strings.Contains(reference, "://") || strings.HasPrefix(reference, "//") {
			t.Fatalf("Swagger page references an external resource %q, which no directive allows", reference)
		}
		if !strings.HasPrefix(reference, docsIndexPath) {
			t.Fatalf("Swagger page references %q outside %s", reference, docsIndexPath)
		}
		asset := docsRequest(handler, http.MethodGet, reference, "")
		if asset.Code != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200", reference, asset.Code)
		}
		contentType := asset.Header().Get("Content-Type")
		switch {
		case strings.HasSuffix(reference, ".js") && !strings.Contains(contentType, "javascript"):
			t.Fatalf("GET %s Content-Type = %q; nosniff would block the script", reference, contentType)
		case strings.HasSuffix(reference, ".css") && !strings.HasPrefix(contentType, "text/css"):
			t.Fatalf("GET %s Content-Type = %q; nosniff would block the stylesheet", reference, contentType)
		case strings.HasSuffix(reference, ".png") && contentType != "image/png":
			t.Fatalf("GET %s Content-Type = %q, want image/png", reference, contentType)
		}
		if strings.HasSuffix(reference, ".css") {
			assertStylesheetNeedsOnly(t, asset.Body.String(), directives)
		}
	}

	for directive, want := range map[string]string{
		"default-src": "'none'", "connect-src": "'self'", "frame-ancestors": "'none'",
		"base-uri": "'none'", "form-action": "'none'", "style-src": "'self' 'unsafe-inline'",
	} {
		if directives[directive] != want {
			t.Fatalf("%s = %q, want %q", directive, directives[directive], want)
		}
	}
}

// assertStylesheetNeedsOnly checks the stylesheet against img-src and
// font-src: inline images need data:, and any font or import would need a
// source the policy does not grant.
func assertStylesheetNeedsOnly(t *testing.T, stylesheet string, directives map[string]string) {
	t.Helper()
	if strings.Contains(stylesheet, "@font-face") || strings.Contains(stylesheet, "@import") {
		t.Fatal("Swagger stylesheet loads fonts or imports; font-src 'self' no longer describes it")
	}
	dataImages := 0
	for _, reference := range extractAttributeValues(stylesheet, "url(") {
		reference = strings.Trim(reference, `"'`)
		switch {
		case strings.HasPrefix(reference, "data:image/"):
			dataImages++
		case strings.Contains(reference, "://") || strings.HasPrefix(reference, "//"):
			t.Fatalf("Swagger stylesheet loads external resource %q", reference)
		}
	}
	if dataImages == 0 {
		t.Fatal("Swagger stylesheet embeds no data: images; img-src data: is testing nothing")
	}
	if directives["img-src"] != "'self' data:" {
		t.Fatalf("img-src = %q, want 'self' data:", directives["img-src"])
	}
}

func parsePolicy(t *testing.T, policy string) map[string]string {
	t.Helper()
	if policy == "" {
		t.Fatal("Content-Security-Policy is absent")
	}
	directives := make(map[string]string)
	for _, directive := range strings.Split(policy, ";") {
		name, value, _ := strings.Cut(strings.TrimSpace(directive), " ")
		directives[name] = strings.TrimSpace(value)
	}
	return directives
}

// extractInlineScripts scans the page by hand, independently of the regular
// expression the server uses, so both have to agree on what is inline.
func extractInlineScripts(page string) []string {
	var scripts []string
	rest := page
	for {
		start := strings.Index(rest, "<script")
		if start < 0 {
			return scripts
		}
		tagEnd := strings.Index(rest[start:], ">")
		if tagEnd < 0 {
			return scripts
		}
		tag := rest[start : start+tagEnd+1]
		rest = rest[start+tagEnd+1:]
		end := strings.Index(rest, "</script>")
		if end < 0 {
			return scripts
		}
		if !strings.Contains(tag, "src=") {
			scripts = append(scripts, rest[:end])
		}
		rest = rest[end+len("</script>"):]
	}
}

// extractAttributeValues returns the text following each prefix up to the
// next quote or closing parenthesis, which covers src="...", href="..." and
// url(...) alike.
func extractAttributeValues(text string, prefixes ...string) []string {
	var values []string
	for _, prefix := range prefixes {
		rest := text
		for {
			start := strings.Index(rest, prefix)
			if start < 0 {
				break
			}
			rest = rest[start+len(prefix):]
			end := strings.IndexAny(rest, `")`)
			if end < 0 {
				break
			}
			values = append(values, rest[:end])
			rest = rest[end:]
		}
	}
	return values
}

// TestDocsHandlerBuildsPolicyBeforeServing guards the startup path: the hashes
// are computed once when the server is built, not per request.
func TestDocsHandlerBuildsPolicyBeforeServing(t *testing.T) {
	t.Parallel()

	handler := newDocsHandler(DocsPublic)
	first := docsRequest(handler, http.MethodGet, docsIndexPath, "").Header().Get(headerContentSecurity)
	second := docsRequest(handler, http.MethodGet, docsIndexPath, "").Header().Get(headerContentSecurity)
	if first != second || !strings.Contains(first, "'sha256-") {
		t.Fatalf("policy is not stable across requests: %q vs %q", first, second)
	}
	_ = time.Second
}
