package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/17xande-dev/leave/internal/config"
)

// The handler tests run without the CSRF layer, so this pins that the real
// stack refuses a form post that carries no token -- a sign-in form included,
// or a cross-site page could sign a visitor in as someone else.
func TestCSRFRefusesTokenlessPost(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := csrf(ok, config.Config{CookieSecure: false, BaseURL: "http://leave.test"})

	req := httptest.NewRequest("POST", "http://leave.test/login", strings.NewReader(url.Values{"username": {"a"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://leave.test")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("tokenless POST: %d, want 403", rec.Code)
	}

	// A GET passes and is issued a token cookie.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "http://leave.test/login", nil))
	if rec.Code != http.StatusOK || rec.Header().Get("Set-Cookie") == "" {
		t.Fatalf("GET: %d, cookie %q", rec.Code, rec.Header().Get("Set-Cookie"))
	}
}

// The appearance form is public but not exempt: without the token check a
// cross-site page could flip a visitor's theme.
func TestCSRFRefusesTokenlessThemePost(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusSeeOther) })
	h := csrf(ok, config.Config{CookieSecure: false, BaseURL: "http://leave.test"})

	req := httptest.NewRequest("POST", "http://leave.test/theme", strings.NewReader(url.Values{"theme": {"dark"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://leave.test")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("tokenless POST /theme: %d, want 403", rec.Code)
	}
}

// The exempt list is pinned, so adding an endpoint to it has to be done in the
// open. Each of these takes a bearer token, its own secret, or nothing at all
// (public metadata); none of them reads a cookie.
func TestCSRFExemptions(t *testing.T) {
	want := []string{"/mcp", "/oauth/token", "/oauth/register", "/oauth/revoke",
		"/.well-known/oauth-authorization-server", "/.well-known/oauth-protected-resource",
		"/.well-known/oauth-protected-resource/mcp"}
	if strings.Join(csrfExempt, ",") != strings.Join(want, ",") {
		t.Fatalf("csrfExempt = %v, want %v", csrfExempt, want)
	}

	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := csrf(ok, config.Config{CookieSecure: false, BaseURL: "http://leave.test"})
	post := func(path string) int {
		req := httptest.NewRequest("POST", "http://leave.test"+path, strings.NewReader("x=1"))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", "http://leave.test")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	for _, p := range csrfExempt {
		if code := post(p); code != http.StatusOK {
			t.Errorf("POST %s without a token: %d, want it exempt", p, code)
		}
	}
	// Everything a browser form posts keeps the check, the consent form first of all.
	for _, p := range []string{"/oauth/authorize", "/oauth/authorize/", "/oauth/tokens", "/mcp/x", "/login", "/logout",
		"/leave", "/leave/1/decide", "/apps/abc/revoke", "/admin/settings",
		"/admin/users/1/leave", "/admin/requests/1/unrecord"} {
		if code := post(p); code != http.StatusForbidden {
			t.Errorf("POST %s without a token: %d, want 403", p, code)
		}
	}
}

// Bulk approval is a state change in one post: it needs the token like any form.
func TestCSRFRefusesTokenlessBulkApprove(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := csrf(ok, config.Config{CookieSecure: false, BaseURL: "http://leave.test"})
	req := httptest.NewRequest("POST", "http://leave.test/approvals/approve", strings.NewReader(url.Values{"id": {"1"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", "http://leave.test")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("tokenless bulk approve: %d, want 403", rec.Code)
	}
}
