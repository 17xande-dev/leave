package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/17xande-dev/leave/internal/config"
	"github.com/17xande-dev/leave/internal/middleware"
)

func TestTokenUnreadableBodyReturnsOAuthError(t *testing.T) {
	e := newE2E(t)
	for _, tc := range []struct{ name, body string }{
		{"malformed", "grant_type=refresh_token&bad=%ZZ"},
		{"token cap", "padding=" + strings.Repeat("x", 64<<10)},
		{"outer cap", "padding=" + strings.Repeat("x", 2*middleware.MaxFormBytes)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res, err := http.Post(e.base+"/oauth/token", "application/x-www-form-urlencoded", strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			if res.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", res.StatusCode)
			}
			if got := res.Header.Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q", got)
			}
			if got := res.Header.Get("Cache-Control"); got != "no-store" {
				t.Errorf("Cache-Control = %q", got)
			}
			var body struct {
				Error       string `json:"error"`
				Description string `json:"error_description"`
			}
			if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Error != "invalid_request" || body.Description == "" {
				t.Errorf("OAuth error = %+v", body)
			}
		})
	}
}

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

// nosurf parses the form before any handler runs, and net/http allows 10 MB for
// that, so the body cap must sit in front of the CSRF layer to mean anything.
// Through the whole app: an over-cap body carrying a valid token must not sign
// anyone in.
func TestOversizeLoginWithValidCSRFTokenIsRejected(t *testing.T) {
	e := newE2E(t)
	b := e.browser()
	_, page := b.get("/login")
	f := fields(page)
	f.Set("username", "staff1")
	f.Set("password", "anything")
	f.Set("pad", strings.Repeat("x", 2*middleware.MaxFormBytes))
	res, _ := b.post("/login", f)
	if res.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("over-cap login with a valid token: %d, want 413", res.StatusCode)
	}
	// The same post without the padding signs in, so the 413 is the cap.
	f.Del("pad")
	if res, _ := b.post("/login", f); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("normal login: %d, want 303", res.StatusCode)
	}
}
