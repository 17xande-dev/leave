package main

import (
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/17xande-dev/leave/internal/config"
)

// The limiters are wired in newApp (sign-in) and assistant (OAuth), from TRUSTED_PROXIES. With the kamal subnet
// trusted, two clients arriving through the same proxy peer must get separate buckets, and one client is still
// limited. Without the trusted subnet the peer is the key (the spoofing cases live beside each limiter).
const proxyPeer = "172.30.17.5:4000"

func trustKamal(c *config.Config) { c.TrustedProxies = []string{"172.30.17.0/24"} }

// serve sends r through the whole app (CSRF, headers, limiters) from the proxy peer.
func (e *e2e) serve(r *http.Request, peer, forwarded string) *httptest.ResponseRecorder {
	r.RemoteAddr = peer
	r.Header.Set("X-Forwarded-For", forwarded+", 172.30.17.9")
	rec := httptest.NewRecorder()
	e.srv.Config.Handler.ServeHTTP(rec, r)
	return rec
}

// loginAttempt does a GET /login for a CSRF token and cookie, then POSTs a bad sign-in as the given client.
func (e *e2e) loginAttempt(client string, n int) int {
	e.t.Helper()
	get := e.serve(httptest.NewRequest("GET", e.base+"/login", nil), proxyPeer, client)
	m := hiddenInput.FindAllStringSubmatch(get.Body.String(), -1)
	form := url.Values{"username": {"nobody-" + client + "-" + strconv.Itoa(n)}, "password": {"x"}}
	for _, f := range m {
		form.Set(f[1], html.UnescapeString(f[2]))
	}
	post := httptest.NewRequest("POST", e.base+"/login", strings.NewReader(form.Encode()))
	post.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	post.Header.Set("Origin", e.base)
	for _, c := range get.Result().Cookies() {
		post.AddCookie(c)
	}
	return e.serve(post, proxyPeer, client).Code
}

func TestLoginLimitKeysOnForwardedClientThroughTrustedProxy(t *testing.T) {
	e := newE2EConfig(t, trustKamal)
	// The IP bucket holds 20; fresh usernames keep the per-username limit out of it.
	for i := range 20 {
		if code := e.loginAttempt("198.51.100.1", i); code == http.StatusTooManyRequests || code == http.StatusForbidden {
			t.Fatalf("client A attempt %d: %d", i, code)
		}
	}
	if code := e.loginAttempt("198.51.100.1", 20); code != http.StatusTooManyRequests {
		t.Fatalf("client A past the burst: %d, want 429", code)
	}
	if code := e.loginAttempt("198.51.100.2", 0); code == http.StatusTooManyRequests || code == http.StatusForbidden {
		t.Fatalf("client B shared client A's bucket: %d", code)
	}
}

func (e *e2e) oauthPost(path, client, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", e.base+path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return e.serve(r, proxyPeer, client)
}

func TestOAuthLimitsKeyOnForwardedClientThroughTrustedProxy(t *testing.T) {
	e := newE2EConfig(t, trustKamal)
	// Token limit: burst 60, Retry-After 5.
	var rec *httptest.ResponseRecorder
	for range 61 {
		rec = e.oauthPost("/oauth/token", "198.51.100.1", "grant_type=nope")
	}
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != "5" {
		t.Fatalf("token client A past the burst: %d Retry-After %q", rec.Code, rec.Header().Get("Retry-After"))
	}
	if rec = e.oauthPost("/oauth/token", "198.51.100.2", "grant_type=nope"); rec.Code == http.StatusTooManyRequests {
		t.Fatal("token client B shared client A's bucket")
	}
	// Register limit: burst 20 (the body need not be valid; the limiter runs first).
	for range 21 {
		rec = e.oauthPost("/oauth/register", "198.51.100.1", `{}`)
	}
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("register client A past the burst: %d, want 429", rec.Code)
	}
	if rec = e.oauthPost("/oauth/register", "198.51.100.2", `{}`); rec.Code == http.StatusTooManyRequests {
		t.Fatal("register client B shared client A's bucket")
	}
}
