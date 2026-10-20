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
