package handler

import (
	"context"
	"database/sql"
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
	"github.com/17xande-dev/leave/internal/mcpserver"
	"github.com/17xande-dev/leave/internal/oauth"
	"github.com/17xande-dev/leave/internal/outbox"
	"github.com/17xande-dev/leave/internal/users"
)

type harness struct {
	t     *testing.T
	h     *Handler
	srv   *httptest.Server
	dir   *directory.Fake
	svc   *leave.Service
	us    *users.Store
	oauth *oauth.Server
	db    *sql.DB
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
	svc := leave.New(d, us, leave.Config{Location: time.UTC})
	svc.Now = func() time.Time { return time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC) }
	if err := svc.SeedTypes(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, _, err := us.Sync(context.Background(), mustAll(t, dir)); err != nil {
		t.Fatal(err)
	}
	if ts, err := dir.Teams(context.Background()); err != nil {
		t.Fatal(err)
	} else if _, _, err := us.SyncTeams(context.Background(), "fake", ts); err != nil {
		t.Fatal(err)
	}
	// The pastor is the final approver, as in the directory fixture's chain;
	// hr is the admin who saves it.
	pastor, _ := us.ByUsername(context.Background(), "pastor")
	hr, _ := us.ByUsername(context.Background(), "hr")
	st, err := svc.Settings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	st.FinalApproverID = pastor.ID
	if err := svc.SaveSettings(context.Background(), hr, st); err != nil {
		t.Fatal(err)
	}
	jobs := outbox.NewStore(d)
	oa := oauth.New(d, oauth.Config{BaseURL: "http://leave.test"})
	h, err := New(Config{SessionTTL: time.Hour}, Deps{Users: us, Sessions: auth.NewSessions(d, time.Hour), Directory: dir,
		Leave: svc, Outbox: jobs, TeamsEnabled: true, OAuth: oa,
		MCP: mcpserver.New(mcpserver.Config{Leave: svc, Users: us, Outbox: jobs, Verifier: oa.Verify,
			ResourceMetadataURL: oa.ResourceMetadataURL(), Scopes: []string{oauth.Scope}})})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &harness{t: t, h: h, srv: srv, dir: dir, svc: svc, us: us, oauth: oa, db: d}
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
		"/\t/evil.example":     "/",
		"/\v/evil.example":     "/",
		"/\f/evil.example":     "/",
		"/\n/evil.example":     "/",
		"/\r/evil.example":     "/",
		"/\x7f/evil.example":   "/",
		"/team\t":              "/",
		"/team\x00":            "/",
		// The consent screen is reached through sign-in with its whole query,
		// so the person lands back on it.
		"/oauth/authorize?client_id=c&redirect_uri=http%3A%2F%2Flocalhost%3A1%2Fcb&state=s": "/oauth/authorize?client_id=c&redirect_uri=http%3A%2F%2Flocalhost%3A1%2Fcb&state=s",
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
	// The first five are the site's (the appearance form among them); the rest are the AI-assistant endpoints,
	// public to the session check because they carry no session: the MCP
	// endpoint (bearer token, checked inside), the OAuth token, registration
	// and revocation endpoints, and discovery metadata. The consent screen,
	// /oauth/authorize, is not here: it is behind sign-in.
	want := "GET /healthz,GET /static/{file},GET /login,POST /login,POST /theme," +
		"/mcp,POST /oauth/token,POST /oauth/register,POST /oauth/revoke," +
		"/.well-known/oauth-authorization-server,/.well-known/oauth-protected-resource," +
		"/.well-known/oauth-protected-resource/mcp"
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

func TestSafePath(t *testing.T) {
	for in, want := range map[string]string{
		"":                     "/",
		"/team":                "/team",
		"/login":               "/login",
		"/\t/evil.example":     "/",
		"/\v/evil.example":     "/",
		"/\f/evil.example":     "/",
		"/\n/evil.example":     "/",
		"/\r/evil.example":     "/",
		"/\x7f/evil.example":   "/",
		"/team\t":              "/",
		"/team\x00":            "/",
		"//evil.example":       "/",
		`/\evil.example`:       "/",
		"https://evil.example": "/",
	} {
		if got := safePath(in); got != want {
			t.Errorf("safePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSetTheme(t *testing.T) {
	hs := newHarness(t)
	themeCookieOf := func(res *http.Response) *http.Cookie {
		for _, c := range res.Cookies() {
			if c.Name == "theme" {
				return c
			}
		}
		return nil
	}
	for _, v := range []string{"light", "dark"} {
		res, _ := hs.post(hs.client(), "/theme", url.Values{"theme": {v}, "next": {"/team?x=1"}})
		c := themeCookieOf(res)
		if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/team?x=1" {
			t.Errorf("%s: %d %q", v, res.StatusCode, res.Header.Get("Location"))
		}
		if c == nil || c.Value != v || c.Path != "/" || !c.HttpOnly || c.SameSite != http.SameSiteLaxMode || c.MaxAge != 365*24*3600 {
			t.Errorf("%s: cookie %+v", v, c)
		}
	}

	res, _ := hs.post(hs.client(), "/theme", url.Values{"theme": {"system"}, "next": {"/login"}})
	if c := themeCookieOf(res); c == nil || c.MaxAge >= 0 || c.Value != "" {
		t.Errorf("system should clear the cookie, got %+v", c)
	}
	if res.Header.Get("Location") != "/login" {
		t.Errorf("/login is a valid next, got %q", res.Header.Get("Location"))
	}

	for _, v := range []url.Values{{}, {"theme": {""}}, {"theme": {"purple"}}, {"theme": {"Dark"}}} {
		res, _ := hs.post(hs.client(), "/theme", v)
		if res.StatusCode != http.StatusBadRequest || themeCookieOf(res) != nil {
			t.Errorf("%v: %d, cookie %v; want 400 and none", v, res.StatusCode, themeCookieOf(res))
		}
	}

	for _, next := range []string{"//evil.example", `/\evil.example`, "https://evil.example",
		"/\t/evil.example", "/\n/evil.example", "/\x7f/evil.example", "/team\r"} {
		res, _ := hs.post(hs.client(), "/theme", url.Values{"theme": {"dark"}, "next": {next}})
		if res.Header.Get("Location") != "/" {
			t.Errorf("next %q redirected to %q, want /", next, res.Header.Get("Location"))
		}
	}
}

func TestThemeRendering(t *testing.T) {
	hs := newHarness(t)
	withCookie := func(c *http.Client, v string) {
		u, _ := url.Parse(hs.srv.URL)
		c.Jar.SetCookies(u, []*http.Cookie{{Name: "theme", Value: v, Path: "/"}})
	}
	// buttons is the theme buttons on the page, as "class=value" pairs. The toggle
	// renders one per system preference and CSS shows one, so what matters is
	// that each carries the right value.
	buttons := func(body string) []string {
		var got []string
		// class=value:label, so a label that names the wrong theme for what the
		// button posts fails too ("system" leads to whichever the system is).
		for _, m := range regexp.MustCompile(`<button type="submit" class="(sys-\w+)" name="theme" value="(\w+)" aria-label="Switch to (\w+) theme"`).FindAllStringSubmatch(body, -1) {
			got = append(got, m[1]+"="+m[2]+":"+m[3])
		}
		return got
	}
	want := map[string][]string{
		"":      {"sys-light=dark:dark", "sys-dark=light:light"},
		"light": {"sys-light=dark:dark", "sys-dark=system:dark"},
		"dark":  {"sys-light=system:light", "sys-dark=light:light"},
	}
	check := func(t *testing.T, name, state, body string) {
		t.Helper()
		if got := buttons(body); strings.Join(got, " ") != strings.Join(want[state], " ") {
			t.Errorf("%s with cookie %q: buttons %v, want %v", name, state, got, want[state])
		}
	}

	// Plain layout (sign-in) and app layout (home), each in every state.
	for _, page := range []struct{ name, path, user string }{
		{"plain", "/login", ""}, {"app", "/", "staff1"},
	} {
		newClient := func() *http.Client {
			if page.user == "" {
				return hs.client()
			}
			return hs.signIn(page.user)
		}
		c := newClient()
		_, body := hs.get(c, page.path)
		if strings.Contains(body, "data-theme") {
			t.Errorf("%s without a cookie: forced theme", page.name)
		}
		check(t, page.name, "", body)
		if !strings.Contains(body, `action="/theme"`) || !regexp.MustCompile(`action="/theme"[\s\S]*?name="csrf_token"`).MatchString(body) {
			t.Errorf("%s: no theme toggle with a CSRF field", page.name)
		}
		if !strings.Contains(body, `name="next" value="`+page.path+`"`) {
			t.Errorf("%s: next is not the current path", page.name)
		}

		for _, v := range []string{"light", "dark"} {
			c := newClient()
			withCookie(c, v)
			_, body := hs.get(c, page.path)
			if !strings.Contains(body, `<html lang="en" data-theme="`+v+`">`) {
				t.Errorf("%s with cookie %s: wrong theme", page.name, v)
			}
			check(t, page.name, v, body)
		}

		c = newClient()
		withCookie(c, `"><script>`)
		_, body = hs.get(c, page.path)
		if strings.Contains(body, "data-theme") || strings.Contains(body, "<script>") {
			t.Errorf("%s: a junk cookie was not ignored", page.name)
		}
		check(t, page.name, "", body)
	}

	// A re-rendered POST has no GET URL to go back to.
	r, body := hs.post(hs.client(), "/login", url.Values{"username": {"nobody"}, "password": {"x"}})
	if r.StatusCode != http.StatusUnauthorized || !strings.Contains(body, `name="next" value="/"`) {
		t.Errorf("refused sign-in should offer / as next: %d", r.StatusCode)
	}
}

func TestThemeNextAfterRefusedPost(t *testing.T) {
	hs := newHarness(t)
	c := hs.signIn("staff1")
	post := func(referer string) string {
		req, _ := http.NewRequest("POST", hs.srv.URL+"/leave", strings.NewReader(url.Values{"type": {"1"}, "start": {"2026-11-12"}, "end": {"2026-11-10"}}.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if referer != "" {
			req.Header.Set("Referer", referer)
		}
		res, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		b, _ := io.ReadAll(res.Body)
		if res.StatusCode != http.StatusUnprocessableEntity {
			t.Fatalf("refused apply: %d", res.StatusCode)
		}
		m := regexp.MustCompile(`name="next" value="([^"]*)"`).FindStringSubmatch(string(b))
		if m == nil {
			t.Fatal("no next field")
		}
		return m[1]
	}
	if got := post(hs.srv.URL + "/leave/new?x=1"); got != "/leave/new?x=1" {
		t.Errorf("same-origin Referer: next = %q", got)
	}
	for name, ref := range map[string]string{
		"missing": "", "foreign": "https://evil.example/leave/new", "garbage": "%zz",
		"scheme-relative": "//evil.example/x",
	} {
		if got := post(ref); got != "/" {
			t.Errorf("%s Referer: next = %q, want /", name, got)
		}
	}
}
