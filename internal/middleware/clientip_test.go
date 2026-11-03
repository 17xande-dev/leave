package middleware_test

import (
	"net/http"
	"testing"

	"github.com/17xande-dev/leave/internal/middleware"
)

func TestClientIP(t *testing.T) {
	trusted := middleware.ParseTrustedProxies([]string{"10.0.0.0/8"})

	newReq := func(remoteAddr, cf, xff string) *http.Request {
		r := &http.Request{RemoteAddr: remoteAddr, Header: http.Header{}}
		if cf != "" {
			r.Header.Set("CF-Connecting-IP", cf)
		}
		if xff != "" {
			r.Header.Set("X-Forwarded-For", xff)
		}
		return r
	}

	t.Run("untrusted peer ignores both headers", func(t *testing.T) {
		r := newReq("203.0.113.5:1234", "1.2.3.4", "1.2.3.4, 5.6.7.8")
		if got := middleware.ClientIP(r, trusted); got != "203.0.113.5" {
			t.Errorf("ClientIP = %q, want %q", got, "203.0.113.5")
		}
	})

	t.Run("trusted peer honours CF-Connecting-IP", func(t *testing.T) {
		r := newReq("10.0.0.1:1234", "1.2.3.4", "9.9.9.9")
		if got := middleware.ClientIP(r, trusted); got != "1.2.3.4" {
			t.Errorf("ClientIP = %q, want %q", got, "1.2.3.4")
		}
	})

	t.Run("trusted peer falls back to XFF right-most untrusted hop", func(t *testing.T) {
		r := newReq("10.0.0.1:1234", "", "1.2.3.4, 10.0.0.2")
		if got := middleware.ClientIP(r, trusted); got != "1.2.3.4" {
			t.Errorf("ClientIP = %q, want %q", got, "1.2.3.4")
		}
	})

	t.Run("header values with a port are reduced to the address", func(t *testing.T) {
		for _, c := range []struct{ cf, xff, want string }{
			{"1.2.3.4:5555", "", "1.2.3.4"},
			{"[2001:db8::1]:443", "", "2001:db8::1"},
			{"[2001:db8::1]", "", "2001:db8::1"},
			{"", "9.9.9.9:80, 1.2.3.4:4000, 10.0.0.2:1", "1.2.3.4"},
			{"", "[2001:db8::7]:9", "2001:db8::7"},
		} {
			r := newReq("10.0.0.1:1234", c.cf, c.xff)
			if got := middleware.ClientIP(r, trusted); got != c.want {
				t.Errorf("cf %q xff %q: ClientIP = %q, want %q", c.cf, c.xff, got, c.want)
			}
		}
	})

	t.Run("garbage header values fall back to the peer", func(t *testing.T) {
		r := newReq("10.0.0.1:1234", "not-an-ip", "also-not-an-ip")
		if got := middleware.ClientIP(r, trusted); got != "10.0.0.1" {
			t.Errorf("ClientIP = %q, want %q", got, "10.0.0.1")
		}
	})

	t.Run("empty trusted list always returns the peer", func(t *testing.T) {
		r := newReq("10.0.0.1:1234", "1.2.3.4", "1.2.3.4")
		if got := middleware.ClientIP(r, nil); got != "10.0.0.1" {
			t.Errorf("ClientIP = %q, want %q", got, "10.0.0.1")
		}
	})
}

// The production path: kamal-proxy sits in the `kamal` docker network's fixed
// subnet and passes Cloudflare's CF-Connecting-IP through (config/deploy.yml).
func TestClientIPKamalNetwork(t *testing.T) {
	trusted := middleware.ParseTrustedProxies([]string{"172.30.17.0/24"})

	newReq := func(remoteAddr string) *http.Request {
		r := &http.Request{RemoteAddr: remoteAddr, Header: http.Header{}}
		r.Header.Set("CF-Connecting-IP", "198.51.100.7")
		return r
	}

	for _, peer := range []string{"172.30.17.2:5555", "172.30.17.254:5555"} {
		if got := middleware.ClientIP(newReq(peer), trusted); got != "198.51.100.7" {
			t.Errorf("peer %s in the kamal subnet: ClientIP = %q, want the CF-Connecting-IP", peer, got)
		}
	}

	// Outside the subnet, including the old cloudflared address and the
	// neighbouring /24, the header is attacker-controlled and ignored.
	for _, peer := range []string{"172.30.16.10:5555", "172.30.18.2:5555", "203.0.113.9:5555"} {
		want := peer[:len(peer)-len(":5555")]
		if got := middleware.ClientIP(newReq(peer), trusted); got != want {
			t.Errorf("peer %s outside the kamal subnet: ClientIP = %q, want the peer %q", peer, got, want)
		}
	}
}
