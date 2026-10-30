package middleware

import (
	"testing"
	"time"
)

func TestLimiterPerKey(t *testing.T) {
	l := NewLimiter(time.Hour, 2)
	if !l.Allow("a") || !l.Allow("a") {
		t.Fatal("burst of 2 refused")
	}
	if l.Allow("a") {
		t.Fatal("third attempt allowed inside the burst window")
	}
	// Another key has its own bucket: one person being guessed at must not
	// lock everyone else out of signing in.
	if !l.Allow("b") {
		t.Fatal("independent key refused")
	}
}

func TestIPKey(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"203.0.113.7", "203.0.113.7"},
		{"2001:db8:1:2:aaaa::1", "2001:db8:1:2::/64"},
		{"2001:db8:1:2:bbbb::9", "2001:db8:1:2::/64"},
		{"2001:db8:1:3::1", "2001:db8:1:3::/64"},
		{"::ffff:203.0.113.7", "203.0.113.7"},
		{"fe80::1%eth0", "fe80::/64"},
		{"not an ip", "not an ip"},
	} {
		if got := IPKey(c.in); got != c.want {
			t.Errorf("IPKey(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestAllowIPSharesOneBucketPerPrefix(t *testing.T) {
	l := NewLimiter(time.Hour, 2)
	if !l.AllowIP("2001:db8::1") || !l.AllowIP("2001:db8::2") {
		t.Fatal("burst of 2 refused")
	}
	if l.AllowIP("2001:db8::ffff") {
		t.Fatal("a third address in the same /64 got a fresh bucket")
	}
	if !l.AllowIP("2001:db8:0:1::1") {
		t.Fatal("a different /64 was refused")
	}
	if !l.AllowIP("203.0.113.7") || !l.AllowIP("::ffff:203.0.113.7") || l.AllowIP("203.0.113.7") {
		t.Fatal("IPv4 and its mapped form must share a bucket of 2")
	}
}

func TestLimiterCapRefusesNewKeys(t *testing.T) {
	l := NewLimiter(time.Hour, 1)
	l.max = 3
	for _, k := range []string{"a", "b", "c"} {
		if !l.Allow(k) {
			t.Fatalf("%s refused below the cap", k)
		}
	}
	if l.Allow("d") {
		t.Fatal("new key admitted at the cap (limiter failed open)")
	}
	if l.size() != 3 {
		t.Fatalf("size = %d, want 3", l.size())
	}
	// Existing keys are unaffected by a full map: still limited, not refused as new.
	if l.Allow("a") {
		t.Fatal("existing key got a second token from a burst of 1")
	}
}

func TestLimiterCapHealsAfterSweep(t *testing.T) {
	l := NewLimiter(time.Hour, 1)
	l.max = 2
	l.Allow("a")
	l.Allow("b")
	// Age every bucket past the idle window and allow the cap sweep to run.
	old := time.Now().Add(-time.Hour)
	for _, b := range l.buckets {
		b.seen = old
	}
	if !l.Allow("c") {
		t.Fatal("idle buckets were not swept to admit a new key")
	}
	// A refused newcomer must not rescan the map on every request.
	l.max = 1
	before := l.capSwept
	l.Allow("d")
	if !l.capSwept.Equal(before) {
		t.Fatal("cap sweep ran again inside capSweepEvery")
	}
}
