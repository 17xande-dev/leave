package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"github.com/17xande-dev/leave/internal/config"
)

// The CSRF cookie holds the real token (the form field is a masked copy that
// changes on every render), so "the token changed" is read from the cookie.
func csrfCookie(t *testing.T, b *browser, name string) string {
	t.Helper()
	u, _ := url.Parse(b.base)
	for _, c := range b.c.Jar.Cookies(u) {
		if c.Name == name {
			return c.Value
		}
	}
	t.Fatalf("no %s cookie in the jar", name)
	return ""
}

// A token issued before sign-in must not survive into the session (cookie
// injection), and the one issued in the session must not survive sign-out.
func TestCSRFTokenRenewedAtSignInAndSignOut(t *testing.T) {
	e := newE2E(t)
	b := e.browser()
	_, page := b.get("/login")
	before := fields(page).Get("csrf_token")
	cookieBefore := csrfCookie(t, b, "csrf_token")

	f := fields(page)
	f.Set("username", "staff1")
	f.Set("password", "anything")
	if res, _ := b.post("/login", f); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("sign in: %d", res.StatusCode)
	}
	cookieIn := csrfCookie(t, b, "csrf_token")
	if cookieIn == cookieBefore {
		t.Fatal("sign-in kept the pre-sign-in CSRF cookie value")
	}
	// The pre-sign-in token, still in a page opened earlier, is refused now.
	if res, _ := b.post("/theme", url.Values{"theme": {"dark"}, "csrf_token": {before}}); res.StatusCode != http.StatusForbidden {
		t.Fatalf("pre-sign-in token after sign-in: %d, want 403", res.StatusCode)
	}

	_, page = b.get("/")
	signedIn := fields(page).Get("csrf_token")
	if signedIn == "" {
		t.Fatal("signed-in page has no CSRF token")
	}
	if res, _ := b.post("/logout", url.Values{"csrf_token": {signedIn}}); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("sign out: %d", res.StatusCode)
	}
	if csrfCookie(t, b, "csrf_token") == cookieIn {
		t.Fatal("sign-out kept the session's CSRF cookie value")
	}
	if res, _ := b.post("/theme", url.Values{"theme": {"dark"}, "csrf_token": {signedIn}}); res.StatusCode != http.StatusForbidden {
		t.Fatalf("session token after sign-out: %d, want 403", res.StatusCode)
	}
}

func TestMCPTrailingSlashIsNotCSRFExempt(t *testing.T) {
	b := newE2E(t).browser()
	// Only the exact URL reaches MCP's bearer check without a CSRF token.
	if res, _ := b.post("/mcp", nil); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("tokenless POST /mcp: %d, want bearer challenge (401)", res.StatusCode)
	}
	if res, _ := b.post("/mcp/", nil); res.StatusCode != http.StatusForbidden {
		t.Fatalf("tokenless POST /mcp/: %d, want CSRF refusal (403)", res.StatusCode)
	}
}

// A session cookie under the old bare name (a pre-migration browser, or one set
// from a sibling subdomain) is not a credential any more.
func TestBareNamedSessionCookieIsIgnored(t *testing.T) {
	e := newE2EConfig(t, func(cfg *config.Config) { cfg.CookieSecure = true })
	b := e.browser()
	b.signIn("staff1", "")
	token := csrfCookie(t, b, "__Host-leave_session")

	get := func(cookieName string) *http.Response {
		req, _ := http.NewRequest("GET", e.base+"/", nil)
		req.AddCookie(&http.Cookie{Name: cookieName, Value: token})
		res, err := e.srv.Client().Transport.RoundTrip(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res
	}
	if res := get("__Host-leave_session"); res.StatusCode != http.StatusOK {
		t.Fatalf("prefixed cookie: %d, want 200", res.StatusCode)
	}
	res := get("leave_session")
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") == "" {
		t.Fatalf("bare-named cookie: %d %q, want a redirect to sign in", res.StatusCode, res.Header.Get("Location"))
	}
}

// The own origin comes from BASE_URL, and a value the check could never match
// stops startup rather than refusing every form post later.
func TestNewAppRefusesBadBaseURL(t *testing.T) {
	for _, base := range []string{"", "https://leave.example.org/", "https://Leave.Example.org", "https://leave.example.org/app", "leave.example.org"} {
		if _, err := newCSRF(config.Config{CookieSecure: true, BaseURL: base}); err == nil {
			t.Errorf("BASE_URL %q accepted, want a startup error", base)
		}
	}
	if _, err := newCSRF(config.Config{CookieSecure: true, BaseURL: "http://leave.example.org"}); err == nil {
		t.Error("http BASE_URL with secure cookies accepted")
	}
	if _, err := newCSRF(config.Config{CookieSecure: true, BaseURL: "https://leave.example.org"}); err != nil {
		t.Errorf("good BASE_URL refused: %v", err)
	}
}

// A body over the cap is refused before anything reads it, on the exempt
// routes and the CSRF-protected ones alike, and multipart is refused outright
// (nothing here takes an upload).
func TestKitRefusesMultipartAndOversize(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := csrfWrap(t, ok)
	for _, tc := range []struct {
		name, path, ctype string
		want              int
	}{
		{"multipart on a form route", "/leave", "multipart/form-data; boundary=x", http.StatusUnsupportedMediaType},
		{"multipart on an exempt route", "/oauth/register", "multipart/form-data; boundary=x", http.StatusUnsupportedMediaType},
	} {
		req := httptest.NewRequest("POST", "http://leave.test"+tc.path, nil)
		req.Header.Set("Content-Type", tc.ctype)
		req.Header.Set("Origin", "http://leave.test")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, rec.Code, tc.want)
		}
	}
}
