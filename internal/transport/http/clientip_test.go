package httpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func prefixes(t *testing.T, values ...string) []netip.Prefix {
	t.Helper()
	result := make([]netip.Prefix, 0, len(values))
	for _, value := range values {
		result = append(result, netip.MustParsePrefix(value))
	}
	return result
}

func forwardedRequest(remoteAddr string, forwardedFor ...string) *http.Request {
	request := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/health", nil)
	request.RemoteAddr = remoteAddr
	for _, value := range forwardedFor {
		request.Header.Add(forwardedForHeader, value)
	}
	return request
}

func TestClientAddressResolver(t *testing.T) {
	t.Parallel()

	private := prefixes(t, "10.0.0.0/8", "fd00::/8")
	tests := []struct {
		name    string
		trusted []netip.Prefix
		request *http.Request
		want    string
	}{
		{
			name:    "no proxies configured uses the peer and ignores the header",
			request: forwardedRequest("203.0.113.10:5000", "198.51.100.7"),
			want:    "203.0.113.10",
		},
		{
			name:    "untrusted peer with forged header is attributed to the peer",
			trusted: private,
			request: forwardedRequest("203.0.113.10:5000", "10.0.0.1"),
			want:    "203.0.113.10",
		},
		{
			name:    "trusted peer reveals the client",
			trusted: private,
			request: forwardedRequest("10.1.2.3:5000", "198.51.100.7"),
			want:    "198.51.100.7",
		},
		{
			name:    "client-supplied entries left of the real one are ignored",
			trusted: private,
			request: forwardedRequest("10.1.2.3:5000", "1.1.1.1, 10.0.0.9, 198.51.100.7"),
			want:    "198.51.100.7",
		},
		{
			name:    "chain of trusted proxies is walked to the first untrusted hop",
			trusted: private,
			request: forwardedRequest("10.1.2.3:5000", "198.51.100.7, 10.9.9.9"),
			want:    "198.51.100.7",
		},
		{
			name:    "repeated headers are read in order",
			trusted: private,
			request: forwardedRequest("10.1.2.3:5000", "198.51.100.7", "10.9.9.9"),
			want:    "198.51.100.7",
		},
		{
			name:    "trusted peer without header is the client",
			trusted: private,
			request: forwardedRequest("10.1.2.3:5000"),
			want:    "10.1.2.3",
		},
		{
			name:    "unparsable header falls back to the peer",
			trusted: private,
			request: forwardedRequest("10.1.2.3:5000", "not-an-address"),
			want:    "10.1.2.3",
		},
		{
			name:    "request originating inside the trusted network",
			trusted: private,
			request: forwardedRequest("10.1.2.3:5000", "10.5.5.5"),
			want:    "10.5.5.5",
		},
		{
			name:    "IPv4-mapped peer is unmapped",
			request: forwardedRequest("[::ffff:203.0.113.10]:1234"),
			want:    "203.0.113.10",
		},
		{
			name:    "bracketed IPv6 with port and zone is canonicalised",
			trusted: private,
			request: forwardedRequest("[fd00::1]:443", "[2001:db8::1%25eth0]:8080"),
			want:    "2001:db8::1",
		},
		{
			name:    "IPv4 hop with port is accepted",
			trusted: private,
			request: forwardedRequest("10.1.2.3:5000", "198.51.100.7:61234"),
			want:    "198.51.100.7",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, ok := NewClientAddressResolver(test.trusted).Resolve(test.request)
			if !ok {
				t.Fatal("Resolve() ok = false, want an address")
			}
			if got != netip.MustParseAddr(test.want) {
				t.Fatalf("Resolve() = %s, want %s", got, test.want)
			}
			if got.Is4In6() || got.Zone() != "" {
				t.Fatalf("Resolve() = %s is not canonical", got)
			}
		})
	}
}

func TestClientAddressResolverWithoutIPPeer(t *testing.T) {
	t.Parallel()

	resolver := NewClientAddressResolver(prefixes(t, "10.0.0.0/8"))
	if _, ok := resolver.Resolve(forwardedRequest("@", "198.51.100.7")); ok {
		t.Fatal("Resolve() ok = true for a peer without an IP address")
	}
}

func TestClientBucket(t *testing.T) {
	t.Parallel()

	if got := ClientBucket(netip.MustParseAddr("203.0.113.10")); got != netip.MustParsePrefix("203.0.113.10/32") {
		t.Fatalf("ClientBucket(IPv4) = %s, want /32", got)
	}
	if got := ClientBucket(netip.MustParseAddr("2001:db8:1:2:3:4:5:6")); got != netip.MustParsePrefix("2001:db8:1:2::/64") {
		t.Fatalf("ClientBucket(IPv6) = %s, want the /64", got)
	}
}

// TestClientAddressMiddlewareExposesResolvedAddress proves a handler behind
// the full chain reads the same identity the resolver produced, which is the
// hook a rate limiter will use.
func TestClientAddressMiddlewareExposesResolvedAddress(t *testing.T) {
	t.Parallel()

	var seen netip.Addr
	var found bool
	capture := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen, found = ClientAddressFromContext(r.Context())
	})
	handler := clientAddressMiddleware(NewClientAddressResolver(prefixes(t, "10.0.0.0/8")), capture)

	handler.ServeHTTP(httptest.NewRecorder(), forwardedRequest("10.1.2.3:5000", "198.51.100.7"))
	if !found || seen != netip.MustParseAddr("198.51.100.7") {
		t.Fatalf("context address = %s/%t, want 198.51.100.7/true", seen, found)
	}

	handler.ServeHTTP(httptest.NewRecorder(), forwardedRequest("@"))
	if found {
		t.Fatal("context carries an address for a peer without one")
	}
}
