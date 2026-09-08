package httpserver

import "net/http"

// Response headers applied to every response, including the ones written by
// the recovery path. They are set before the handler runs so that a handler
// which fails halfway still answers with them; a handler that serves HTML
// replaces only the policy that HTML needs.
const (
	headerContentTypeOptions = "X-Content-Type-Options"
	headerReferrerPolicy     = "Referrer-Policy"
	headerCacheControl       = "Cache-Control"
	headerContentSecurity    = "Content-Security-Policy"
	headerFrameOptions       = "X-Frame-Options"

	// strictContentSecurityPolicy is what a JSON or YAML response gets. Such
	// a body never loads anything, so nothing is allowed, and it must never
	// be framed by another origin.
	strictContentSecurityPolicy = "default-src 'none'; frame-ancestors 'none'"
)

// securityHeadersMiddleware installs the strict defaults. Cache-Control is
// no-store because every body is either authenticated, an error carrying a
// correlation id, or a readiness answer that must never be served stale.
func securityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setStrictSecurityHeaders(w.Header())
		next.ServeHTTP(w, r)
	})
}

func setStrictSecurityHeaders(header http.Header) {
	header.Set(headerContentTypeOptions, "nosniff")
	header.Set(headerReferrerPolicy, "no-referrer")
	header.Set(headerCacheControl, "no-store")
	header.Set(headerContentSecurity, strictContentSecurityPolicy)
	header.Set(headerFrameOptions, "DENY")
}
