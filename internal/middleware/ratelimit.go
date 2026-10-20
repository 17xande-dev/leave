package middleware

import (
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
}

type limitBucket struct {
	l    *rate.Limiter
	seen time.Time
}

func NewLimiter(every time.Duration, burst int) *Limiter {
	return &Limiter{buckets: map[string]*limitBucket{}, every: rate.Every(every), burst: burst, lastGC: time.Now()}
}

// Allow reports whether key may proceed now, spending a token if so.
func (l *Limiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	// Sweep on use rather than from a goroutine: nothing to leak or stop, and
	// an idle limiter costs nothing.
	if now.Sub(l.lastGC) > 10*time.Minute {
		for k, b := range l.buckets {
			if now.Sub(b.seen) > 30*time.Minute {
				delete(l.buckets, k)
			}
		}
		l.lastGC = now
	}
	b, ok := l.buckets[key]
	if !ok {
		b = &limitBucket{l: rate.NewLimiter(l.every, l.burst)}
		l.buckets[key] = b
	}
	b.seen = now
	return b.l.AllowN(now, 1)
}
