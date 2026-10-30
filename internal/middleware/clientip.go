package middleware

import (
	"net"
	"net/http"
	"slices"
	"strings"
)

// ParseTrustedProxies parses a list of CIDRs (e.g. from config.TrustedProxies)
// into the form ClientIP expects. Invalid entries are skipped.
func ParseTrustedProxies(cidrs []string) []*net.IPNet {
	var out []*net.IPNet
	for _, c := range cidrs {
		if _, n, err := net.ParseCIDR(c); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// ClientIP returns the best-effort real client address for r. The origin is
// directly reachable (not only via a proxy), so CF-Connecting-IP and
// X-Forwarded-For are honoured only when the immediate TCP peer is one of the
// trusted proxies -- otherwise those headers could be set by anyone and are
// ignored. An empty trusted list means trust nothing, i.e. always use the
// peer address.
func ClientIP(r *http.Request, trusted []*net.IPNet) string {
	peer := peerHost(r.RemoteAddr)

	if !isTrusted(peer, trusted) {
		return peer
	}

	if cf := bareIP(strings.TrimSpace(r.Header.Get("CF-Connecting-IP"))); net.ParseIP(cf) != nil {
		return cf
	}

	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts := strings.Split(xff, ",")
		for _, raw := range slices.Backward(parts) {
			candidate := bareIP(strings.TrimSpace(raw))
			if net.ParseIP(candidate) == nil {
				continue
			}
			if !isTrusted(candidate, trusted) {
				return candidate
			}
		}
	}

	return peer
}

func peerHost(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return host
}

func isTrusted(ip string, trusted []*net.IPNet) bool {
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return false
	}
	for _, n := range trusted {
		if n.Contains(parsed) {
			return true
		}
	}
	return false
}

// bareIP reduces a header value to the address alone. Some load balancers write
// "ip:port" or "[v6]:port"; left alone that is not an address, so it would be
// skipped for the peer's address, or key a rate limiter per source port. A value
// that is neither an address nor host:port is returned unchanged.
func bareIP(s string) string {
	if net.ParseIP(unbracket(s)) != nil {
		return unbracket(s)
	}
	if host, _, err := net.SplitHostPort(s); err == nil {
		return host
	}
	return s
}

// unbracket strips the brackets an IPv6 address may arrive wrapped in.
func unbracket(ip string) string {
	return strings.TrimSuffix(strings.TrimPrefix(ip, "["), "]")
}
