package middleware

import (
	"fmt"
	"log/slog"
	"net/netip"
	"sync/atomic"
	"time"

	"github.com/17xande-dev/ratelimit"
	"github.com/17xande-dev/ratelimit/clientip"
	"golang.org/x/time/rate"
)

// fullLogEvery is the least time between "rate limiter full" lines for one limiter, so a flood is one line per
// ten minutes rather than one per refused request.
const fullLogEvery = 10 * time.Minute

// idleTTL is how long an idle, refilled client's bucket is kept before it can be evicted.
const idleTTL = 30 * time.Minute

// ClientIPSource turns TRUSTED_PROXIES (CIDRs) into the policy that decides which address a request came from.
// With no CIDRs it is the socket peer only. With CIDRs, X-Forwarded-For is read from the right, and only when
// the immediate peer is in one of them; the ingress (kamal-proxy with forward_headers) must append the peer.
// CF-Connecting-IP is not read. An invalid CIDR is an error.
func ClientIPSource(cidrs []string) (clientip.Source, error) {
	var prefixes []netip.Prefix
	for _, c := range cidrs {
		p, err := netip.ParsePrefix(c)
		if err != nil {
			return clientip.Source{}, fmt.Errorf("TRUSTED_PROXIES: %w", err)
		}
		prefixes = append(prefixes, p)
	}
	if len(prefixes) == 0 {
		return clientip.Source{}, nil
	}
	return clientip.New(clientip.Config{TrustedProxies: prefixes})
}

// NewLimiter builds a fail-closed keyed token-bucket limiter: one request's worth of allowance accrues every
// interval, up to burst. When its key table is full, new keys are refused and one warning per fullLogEvery names
// the limiter, so the surface under attack can be told apart. It panics on an invalid rate or burst: every caller
// passes constants.
func NewLimiter(name string, every time.Duration, burst int) *ratelimit.Limiter {
	var l *ratelimit.Limiter
	var last atomic.Int64 // unix nanoseconds of the last warning
	l, err := ratelimit.New(ratelimit.Config{
		Rate: rate.Every(every), Burst: burst, IdleTTL: idleTTL,
		OnFull: func() {
			now := time.Now().UnixNano()
			prev := last.Load()
			if prev != 0 && now-prev < int64(fullLogEvery) {
				return
			}
			if last.CompareAndSwap(prev, now) {
				slog.Warn("rate limiter full; refusing new keys", "limiter", name, "buckets", l.Len())
			}
		},
	})
	if err != nil {
		panic("middleware: limiter " + name + ": " + err.Error())
	}
	return l
}
