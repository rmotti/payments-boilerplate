package httpserver

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"

	apispec "github.com/rmotti/payments-boilerplate/api"
	"github.com/swaggest/swgui/v5emb"
)

// Documentation routes. They live outside the OpenAPI contract, so the
// operation-level authentication middleware never sees them and the policy
// below is what protects them.
const (
	docsPath        = "/docs"
	docsIndexPath   = "/docs/"
	openAPIPath     = "/openapi.yaml"
	docsTitle       = "Payments Boilerplate API"
	docsAllowHeader = "GET, HEAD"
)

// DocsMode is the policy applied to Swagger UI and the OpenAPI document.
type DocsMode int

const (
	// DocsDisabled registers no documentation route at all. The paths answer
	// exactly like any other unknown path, so nothing reveals they exist.
	DocsDisabled DocsMode = iota
	// DocsPublic serves the documentation without credentials. It is only
	// reachable in development.
	DocsPublic
	// DocsProtected serves the documentation to callers presenting a valid
	// X-API-Key, and answers 401 to everyone else. It is the only way to
	// enable documentation outside development.
	DocsProtected
)

// DocsModeFor turns the opt-in flag and the environment into a policy. The
// flag alone is never enough outside development: enabling docs there means
// enabling them behind the same key that protects the business routes.
func DocsModeFor(enabled, development bool) DocsMode {
	switch {
	case !enabled:
		return DocsDisabled
	case development:
		return DocsPublic
	default:
		return DocsProtected
	}
}

// docsHandler guards the three documentation paths with one policy. Every
// path, the redirect included, passes through the same check so no route
// can be used to learn whether the others exist.
type docsHandler struct {
	ui       http.Handler
	verifier APIKeyVerifier
	require  bool
	policy   string
}

func registerDocs(mux *http.ServeMux, mode DocsMode, verifier APIKeyVerifier) {
	if mode == DocsDisabled {
		return
	}
	ui := v5emb.New(docsTitle, openAPIPath, docsIndexPath)
	handler := &docsHandler{
		ui:       ui,
		verifier: verifier,
		require:  mode == DocsProtected,
		policy:   swaggerContentSecurityPolicy(ui),
	}
	// Registered without a method so that every verb reaches the guard.
	// A method-qualified pattern would let the mux answer 405, or redirect
	// /docs to /docs/, before the policy ran.
	mux.Handle(docsPath, handler)
	mux.Handle(docsIndexPath, handler)
	mux.Handle(openAPIPath, handler)
}

func (h *docsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.require && (h.verifier == nil || !h.verifier.Valid(r.Header.Get(apiKeyHeader))) {
		writeError(w, r, http.StatusUnauthorized, codeUnauthorized, ErrUnauthorized.Error())
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", docsAllowHeader)
		writeError(w, r, http.StatusMethodNotAllowed, codeInvalidRequest, "method not allowed")
		return
	}

	switch r.URL.Path {
	case docsPath:
		target := docsIndexPath
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, target, http.StatusMovedPermanently)
	case openAPIPath:
		w.Header().Set("Content-Type", "application/yaml")
		_, _ = w.Write(apispec.OpenAPI)
	default:
		// The UI page and its assets share the Swagger policy. Assets are not
		// documents, so the header is inert on them, but one policy for the
		// whole subtree is simpler to reason about than a per-file choice.
		w.Header().Set(headerContentSecurity, h.policy)
		h.ui.ServeHTTP(w, r)
	}
}

var inlineScriptPattern = regexp.MustCompile(`(?s)<script(\s[^>]*)?>(.*?)</script>`)

// swaggerContentSecurityPolicy builds the policy for the Swagger UI page from
// what the page actually contains, rather than from a guess about it.
//
// The embedded index carries two inline scripts and no other way to run
// them, so their hashes are allowed instead of 'unsafe-inline'. The bundle
// styles elements through attributes, which only 'unsafe-inline' permits
// for styles. Its stylesheet embeds images as data: URIs, it fetches the
// document and "Try it out" targets from this origin only, and it loads no
// fonts, frames or workers. A test exercises the real handler and checks
// every one of these claims against the served files.
func swaggerContentSecurityPolicy(ui http.Handler) string {
	scriptSources := []string{"'self'"}
	hashes, ok := inlineScriptHashes(ui)
	if !ok {
		// The page could not be rendered at startup. Serving a page whose
		// scripts are blocked is worse than serving one that allows inline
		// scripts, so the fallback is the permissive form.
		scriptSources = append(scriptSources, "'unsafe-inline'")
	}
	scriptSources = append(scriptSources, hashes...)
	return strings.Join([]string{
		"default-src 'none'",
		"script-src " + strings.Join(scriptSources, " "),
		"style-src 'self' 'unsafe-inline'",
		"img-src 'self' data:",
		"font-src 'self'",
		"connect-src 'self'",
		"base-uri 'none'",
		"form-action 'none'",
		"frame-ancestors 'none'",
	}, "; ")
}

// inlineScriptHashes renders the index page once and hashes each inline
// script. The template depends only on the handler configuration, so what
// is rendered here is byte for byte what a browser receives.
func inlineScriptHashes(ui http.Handler) ([]string, bool) {
	recorder := httptest.NewRecorder()
	ui.ServeHTTP(recorder, httptest.NewRequestWithContext(context.Background(), http.MethodGet, docsIndexPath, nil))
	if recorder.Code != http.StatusOK {
		return nil, false
	}
	return hashInlineScripts(recorder.Body.String()), true
}

func hashInlineScripts(page string) []string {
	var hashes []string
	for _, match := range inlineScriptPattern.FindAllStringSubmatch(page, -1) {
		if strings.Contains(match[1], "src=") {
			continue
		}
		sum := sha256.Sum256([]byte(match[2]))
		hashes = append(hashes, "'sha256-"+base64.StdEncoding.EncodeToString(sum[:])+"'")
	}
	return hashes
}
