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
