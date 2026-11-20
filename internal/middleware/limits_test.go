package middleware

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func keyFor(t *testing.T, cidrs []string, peer string, headers map[string]string) string {
	t.Helper()
	src, err := ClientIPSource(cidrs)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.RemoteAddr = peer
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return src.Addr(r).String()
}

func TestClientIPSource(t *testing.T) {
	trusted := []string{"10.0.0.0/8"}
	xff := map[string]string{"X-Forwarded-For": "1.2.3.4, 10.0.0.2"}
	cf := map[string]string{"CF-Connecting-IP": "1.2.3.4", "X-Forwarded-For": "5.6.7.8"}
	for _, tc := range []struct {
		name    string
		cidrs   []string
		peer    string
		headers map[string]string
		want    string
	}{
		{"no CIDRs: peer only", nil, "203.0.113.5:1", cf, "203.0.113.5"},
		{"untrusted peer ignores X-Forwarded-For", trusted, "203.0.113.5:1", xff, "203.0.113.5"},
		{"untrusted peer ignores CF-Connecting-IP", trusted, "203.0.113.5:1", cf, "203.0.113.5"},
		{"trusted peer: rightmost untrusted hop", trusted, "10.0.0.1:1", xff, "1.2.3.4"},
		{"trusted peer: CF-Connecting-IP not read", trusted, "10.0.0.1:1", map[string]string{"CF-Connecting-IP": "1.2.3.4"}, "10.0.0.1"},
		{"client cannot lead the chain", trusted, "10.0.0.1:1", map[string]string{"X-Forwarded-For": "9.9.9.9, 1.2.3.4, 10.0.0.2"}, "1.2.3.4"},
		{"kamal subnet peer", []string{"172.30.17.0/24"}, "172.30.17.5:1", map[string]string{"X-Forwarded-For": "198.51.100.7"}, "198.51.100.7"},
		{"outside the kamal subnet", []string{"172.30.17.0/24"}, "172.30.18.5:1", map[string]string{"X-Forwarded-For": "198.51.100.7"}, "172.30.18.5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := keyFor(t, tc.cidrs, tc.peer, tc.headers); got != tc.want {
				t.Errorf("got %s, want %s", got, tc.want)
			}
		})
	}
}

func TestClientIPSourceRejectsBadCIDR(t *testing.T) {
	for _, bad := range []string{"not-a-network", "10.0.0.1", "0.0.0.0/0"} {
		if _, err := ClientIPSource([]string{bad}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestNewLimiterFullRefusesAndLogsOnce(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	l := NewLimiter("t", time.Hour, 1)
	for i := range 10_000 {
		if !l.Allow("k" + strconv.Itoa(i)) {
			t.Fatalf("refused before capacity at %d", i)
		}
	}
	for range 6 {
		if l.Allow("fresh") {
			t.Fatal("fresh key admitted by a full fail-closed limiter")
		}
	}
	if l.Len() != 10_000 {
		t.Errorf("table grew to %d", l.Len())
	}
	if n := strings.Count(buf.String(), "rate limiter full; refusing new keys"); n != 1 {
		t.Errorf("logged %d times for six refusals, want 1:\n%s", n, &buf)
	}
}
