package httpserver

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

const forwardedForHeader = "X-Forwarded-For"

type clientAddressKey struct{}

// ClientAddressResolver decides which address identifies the client of a
// request. It exists so that every consumer of that identity, the rate limiter
// first among them, agrees on one answer and none of them reads
// X-Forwarded-For on its own.
//
// The header is believed only when the TCP peer is one of the trusted proxies.
// A request that arrives straight from the internet carrying a forged header
// is attributed to its real peer, so a caller cannot choose its own identity.
// With no trusted proxies configured the peer is always the client. Every
// address returned is canonical: IPv4-mapped IPv6 is unmapped and zones are
// dropped, so the same client never produces two spellings.
type ClientAddressResolver struct {
	trusted []netip.Prefix
}

// NewClientAddressResolver trusts X-Forwarded-For from peers inside trusted.
func NewClientAddressResolver(trusted []netip.Prefix) *ClientAddressResolver {
	return &ClientAddressResolver{trusted: trusted}
}

// Resolve returns the client address and whether one could be determined. The
// second value is false only when the peer itself has no IP address, such as
// a connection over a Unix socket; no header is ever consulted in that case.
func (r *ClientAddressResolver) Resolve(request *http.Request) (netip.Addr, bool) {
	peer, ok := peerAddress(request.RemoteAddr)
	if !ok {
		return netip.Addr{}, false
	}
	if r == nil || !r.trustedProxy(peer) {
		return peer, true
	}

	// Each proxy appends the peer it saw, so the chain reads oldest to newest
	// and the rightmost entry is what the closest trusted proxy observed.
	// Walking right to left and skipping our own proxies finds the first hop
	// nobody we trust is responsible for: the client. Anything further left
	// was written by that client or by proxies it controls, and is ignored.
	hops := forwardedHops(request.Header.Values(forwardedForHeader))
	for index := len(hops) - 1; index >= 0; index-- {
		hop, ok := parseHop(hops[index])
		if !ok {
			// A trusted proxy never writes an unparsable hop, so this entry
			// was authored by whoever sits behind the proxies. Attribute the
			// request to the proxy instead of to a value the client chose.
			return peer, true
		}
		if !r.trustedProxy(hop) {
			return hop, true
		}
		if index == 0 {
			// Every hop is one of our proxies: the request originated inside
			// the trusted network, and the leftmost proxy is its source.
			return hop, true
		}
	}
	return peer, true
}

func (r *ClientAddressResolver) trustedProxy(address netip.Addr) bool {
	for _, prefix := range r.trusted {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

// ClientBucket collapses an address to the granularity a limiter should count
// on: the whole address for IPv4 and the /64 for IPv6, where a single host
// routinely holds the entire prefix. Counting IPv6 per address would let one
// machine spread its requests across billions of keys.
func ClientBucket(address netip.Addr) netip.Prefix {
	if address.Is4() {
		return netip.PrefixFrom(address, 32)
	}
	return netip.PrefixFrom(address, 64).Masked()
}

// ClientAddressFromContext returns the address resolved for the request, or
// false when none was, which happens only for peers without an IP address.
func ClientAddressFromContext(ctx context.Context) (netip.Addr, bool) {
	address, ok := ctx.Value(clientAddressKey{}).(netip.Addr)
	return address, ok && address.IsValid()
}

// clientAddressMiddleware resolves the client once, before anything that could
// need it, so a rate limiter and a handler never disagree about who called.
func clientAddressMiddleware(resolver *ClientAddressResolver, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		address, ok := resolver.Resolve(r)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), clientAddressKey{}, address)))
	})
}

func peerAddress(remoteAddr string) (netip.Addr, bool) {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	return parseHop(host)
}

// forwardedHops splits every header value on commas. A client may send the
// header more than once, and Go keeps repeated headers as separate values, so
// all of them are read in order rather than only the first.
func forwardedHops(values []string) []string {
	var hops []string
	for _, value := range values {
		for _, hop := range strings.Split(value, ",") {
			hops = append(hops, strings.TrimSpace(hop))
		}
	}
	return hops
}

// parseHop accepts a bare address, an address with a port and a bracketed
// IPv6 address, which are the spellings proxies actually produce. The result
// is canonical, so the same host never appears under two names.
func parseHop(value string) (netip.Addr, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return netip.Addr{}, false
	}
	address, err := netip.ParseAddr(value)
	if err != nil {
		addressPort, portErr := netip.ParseAddrPort(value)
		if portErr != nil {
			return netip.Addr{}, false
		}
		address = addressPort.Addr()
	}
	return address.Unmap().WithZone(""), true
}
