package handler

import (
	"context"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/17xande-dev/leave/internal/auth"
	"github.com/17xande-dev/leave/internal/dbtest"
	"github.com/17xande-dev/leave/internal/directory"
	"github.com/17xande-dev/leave/internal/leave"
	"github.com/17xande-dev/leave/internal/outbox"
	"github.com/17xande-dev/leave/internal/users"
)

type harness struct {
	t   *testing.T
	h   *Handler
	srv *httptest.Server
	dir *directory.Fake
	svc *leave.Service
	us  *users.Store
}

// newHarness serves the real mux (no CSRF layer: main_test covers that) over
// a real SQLite database and the fake directory fixture.
func newHarness(t *testing.T) *harness {
	t.Helper()
	d := dbtest.New(t)
	dir, err := directory.LoadFake("../../testdata/directory.json")
	if err != nil {
		t.Fatal(err)
	}
	us := users.NewStore(d)
	svc := leave.New(d, us, leave.Config{Location: time.UTC, FinalApprover: "pastor", SickCycleAnchor: 2025, SickBackdateDays: 14})
	svc.Now = func() time.Time { return time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC) }
	if err := svc.SeedTypes(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := us.Sync(context.Background(), mustAll(t, dir)); err != nil {
		t.Fatal(err)
	}
	h, err := New(Config{SessionTTL: time.Hour}, Deps{Users: us, Sessions: auth.NewSessions(d, time.Hour), Directory: dir, Leave: svc, Outbox: outbox.NewStore(d)})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &harness{t: t, h: h, srv: srv, dir: dir, svc: svc, us: us}
}

func mustAll(t *testing.T, dir *directory.Fake) []directory.User {
	all, err := dir.AllUsers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return all
}

// client returns a browser-like client: it keeps cookies and does not follow
// redirects, so tests can assert where a response sends you.
func (hs *harness) client() *http.Client {
	jar, _ := cookiejar.New(nil)
	return &http.Client{Jar: jar, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
}

func (hs *harness) signIn(username string) *http.Client {
	hs.t.Helper()
	c := hs.client()
	res, err := c.PostForm(hs.srv.URL+"/login", url.Values{"username": {username}, "password": {"x"}})
	if err != nil {
		hs.t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther {
		hs.t.Fatalf("sign in as %s: status %d", username, res.StatusCode)
	}
	return c
}

func (hs *harness) get(c *http.Client, path string) (int, string) {
	hs.t.Helper()
	res, err := c.Get(hs.srv.URL + path)
	if err != nil {
		hs.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func (hs *harness) post(c *http.Client, path string, form url.Values) (*http.Response, string) {
	hs.t.Helper()
	res, err := c.PostForm(hs.srv.URL+path, form)
	if err != nil {
		hs.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res, string(b)
}

func TestLoginFlow(t *testing.T) {
	hs := newHarness(t)
	c := hs.client()

	// Anonymous GET goes to sign-in and remembers where it was going.
	res, err := c.Get(hs.srv.URL + "/?x=1")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if loc := res.Header.Get("Location"); res.StatusCode != http.StatusSeeOther || loc != "/login?next=%2F%3Fx%3D1" {
		t.Fatalf("anonymous home: %d %q", res.StatusCode, loc)
	}

	r, body := hs.post(c, "/login", url.Values{"username": {"nobody"}, "password": {"x"}})
	if r.StatusCode != http.StatusUnauthorized || !strings.Contains(body, "did not match") {
		t.Fatalf("unknown user: %d", r.StatusCode)
	}

	r, _ = hs.post(c, "/login", url.Values{"username": {`EXAMPLE\Staff1`}, "password": {"x"}, "next": {"/team"}})
	if r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/team" {
		t.Fatalf("sign in: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	if code, body := hs.get(c, "/"); code != 200 || !strings.Contains(body, "Sam Staff") {
		t.Fatalf("home after sign-in: %d", code)
	}

	r, _ = hs.post(c, "/logout", nil)
	if r.StatusCode != http.StatusSeeOther {
		t.Fatalf("logout: %d", r.StatusCode)
	}
	if code, _ := hs.get(c, "/"); code != http.StatusSeeOther {
		t.Fatalf("home after logout: %d, want redirect to sign-in", code)
	}
}

func TestSafeNext(t *testing.T) {
	for in, want := range map[string]string{
		"":                     "/",
		"/team":                "/team",
		"//evil.example":       "/",
		`/\evil.example`:       "/",
		"https://evil.example": "/",
		"/login?next=/x":       "/",
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

// A guesser must hit this limit before AD's lockout policy locks a colleague
// out of Windows.
func TestLoginRateLimitedPerUsername(t *testing.T) {
	hs := newHarness(t)
	c := hs.client()
	var last int
	for range 6 {
		r, _ := hs.post(c, "/login", url.Values{"username": {"staff1"}, "password": {""}})
		last = r.StatusCode
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("6th attempt: %d, want 429", last)
	}
}

// Every protected route must refuse an anonymous request, by its own method:
// a POST route answers a GET with 405 before any middleware runs, so a
// GET-only sweep would prove nothing.
func TestProtectedRoutes(t *testing.T) {
	hs := newHarness(t)
	routes := hs.h.protectedRoutes()
	if len(routes) < 2 {
		t.Fatalf("only %d protected routes; the list stopped loading?", len(routes))
	}
	c := hs.client()
	for _, rt := range routes {
		method, path, _ := strings.Cut(rt.pattern, " ")
		path = strings.ReplaceAll(strings.ReplaceAll(path, "{$}", ""), "{id}", "1")
		path = regexp.MustCompile(`\{[a-z]+\}`).ReplaceAllString(path, "x")
		req, _ := http.NewRequest(method, hs.srv.URL+path, nil)
		res, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusSeeOther && res.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s anonymous: %d, want 303 or 401", rt.pattern, res.StatusCode)
		}
	}

	// The public lines are pinned by source text, so a new route mounted
	// outside protectedRoutes fails here and has to be looked at.
	src, _ := os.ReadFile("handler.go")
	got := regexp.MustCompile(`mux\.Handle(?:Func)?\("([^"]+)"`).FindAllStringSubmatch(string(src), -1)
	var public []string
	for _, m := range got {
		public = append(public, m[1])
	}
	want := "GET /healthz,GET /static/{file},GET /login,POST /login"
	if strings.Join(public, ",") != want {
		t.Errorf("public routes = %v, want %s", public, want)
	}
}

// style="" and on*= are both refused by the CSP (no 'unsafe-inline'). They
// render fine and pass every handler test, then silently do nothing in a
// browser -- so they are banned from the templates outright.
func TestNoInlineStyleOrHandlers(t *testing.T) {
	bad := regexp.MustCompile(`(?i)\sstyle\s*=|\son[a-z]+\s*=`)
	n := 0
	err := walkTemplates(func(name, body string) {
		n++
		if loc := bad.FindStringIndex(body); loc != nil {
			t.Errorf("%s: inline style or handler near %q", name, body[loc[0]:min(loc[1]+20, len(body))])
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if n < 5 {
		t.Fatalf("checked only %d templates", n)
	}
}
