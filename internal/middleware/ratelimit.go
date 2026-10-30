package middleware

import (
	"log/slog"
	"net/netip"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Limiter is a keyed set of token buckets with a lazy eviction sweep.
//
// The login form uses two: one keyed on the submitted username, so guessing
// one person's password is slow however many addresses it comes from, and a
// looser one keyed on the client IP, so spraying many usernames from one
// place is slow too. Neither key is something the attacker can discard for
// free, unlike a cookie. Every attempt also costs a bind against the DC, and
// AD's own lockout policy is behind this -- which is exactly why this limit
// must be tighter than AD's: a guesser should hit this wall, not lock a
// colleague out of their Windows account.
type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*limitBucket
	every   rate.Limit
	burst   int
	lastGC  time.Time
	// capSwept is when a sweep forced by the cap last ran; see capSweepEvery.
	capSwept time.Time
	// max caps len(buckets); zero means maxBuckets. A field so a test can lower
	// it instead of creating ten thousand entries.
	max int
	// capLogged is when a full map last logged, so a flood is one line per sweep
	// interval rather than one per request.
	capLogged time.Time
	name      string
	log       *slog.Logger
}

// maxBuckets is the most buckets one limiter holds. A key can be attacker
// chosen (the submitted username, an address in a header), and the lazy sweep
// only runs every ten minutes, so without a cap the map's size is a hope, not a
// bound.
const maxBuckets = 10_000

// capSweepEvery is the least time between sweeps forced by hitting the cap. A
// sweep scans every bucket under the lock; at the cap every refused newcomer
// would otherwise run one.
const capSweepEvery = 5 * time.Second

// IPKey is the identity a client address is throttled under. IPv4 is keyed by
// the full address; IPv6 by its /64, the smallest block an ISP gives one
// subscriber, because keying on the full address hands anyone with IPv6 a fresh
// bucket per address. An IPv4-mapped address (::ffff:a.b.c.d) is unmapped so it
// shares a bucket with the plain form. Anything unparseable keeps its raw string.
func IPKey(ip string) string {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return ip
	}
	addr = addr.Unmap()
	if addr.Is4() {
		return addr.String()
	}
	// Drop any zone: fe80::1%eth0 and fe80::1%eth1 are one client to us.
	return netip.PrefixFrom(addr.WithZone(""), 64).Masked().String()
}

type limitBucket struct {
	l    *rate.Limiter
	seen time.Time
}

// NewLimiter's name appears in the log line written when the limiter is full,
// so the surface under attack can be told apart.
func NewLimiter(name string, every time.Duration, burst int) *Limiter {
	return &Limiter{buckets: map[string]*limitBucket{}, every: rate.Every(every), burst: burst, lastGC: time.Now(),
		name: name, log: slog.Default()}
}

// AllowIP is Allow for a client address, keyed by IPKey.
func (l *Limiter) AllowIP(ip string) bool { return l.Allow(IPKey(ip)) }

// Allow reports whether key may proceed now, spending a token if so. A key not
// yet in the map is refused when the map is full of live buckets: failing open
// would let a flood of fresh keys switch the limiter off, and existing keys are
// unaffected.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	// Sweep on use rather than from a goroutine: nothing to leak or stop, and
	// an idle limiter costs nothing.
	if now.Sub(l.lastGC) > 10*time.Minute {
		l.sweep(now)
		l.lastGC = now
	}
	b, ok := l.buckets[key]
	if !ok {
		limit := l.max
		if limit <= 0 {
			limit = maxBuckets
		}
		if len(l.buckets) >= limit && (l.capSwept.IsZero() || now.Sub(l.capSwept) >= capSweepEvery) {
			l.sweep(now)
			l.lastGC = now
			l.capSwept = now
		}
		if len(l.buckets) >= limit {
			if l.log != nil && (l.capLogged.IsZero() || now.Sub(l.capLogged) >= 10*time.Minute) {
				l.capLogged = now
				l.log.Warn("rate limiter full; refusing new keys", "limiter", l.name, "buckets", len(l.buckets))
			}
			return false
		}
		b = &limitBucket{l: rate.NewLimiter(l.every, l.burst)}
		l.buckets[key] = b
	}
	b.seen = now
	return b.l.AllowN(now, 1)
}

func (l *Limiter) sweep(now time.Time) {
	for k, b := range l.buckets {
		if now.Sub(b.seen) > 30*time.Minute {
			delete(l.buckets, k)
		}
	}
}

// Len reports how many buckets are held.
func (l *Limiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}
