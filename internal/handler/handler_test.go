package handler

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/17xande-dev/websec/session"
	"github.com/17xande-dev/websec/webtest"

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
	sessions, err := auth.NewSessions(d, session.Default)
	if err != nil {
		t.Fatal(err)
	}
	h, err := New(Config{CSRF: noRenew{}}, Deps{Users: us, Sessions: sessions, Directory: dir,
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

// noRenew stands in for the CSRF policy: these tests serve the mux without the
// CSRF layer (the real stack is exercised by the e2e tests in package main,
// which assert the token is renewed at sign-in and sign-out).
type noRenew struct{}

func (noRenew) Renew(http.ResponseWriter, *http.Request) (string, error) { return "", nil }

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

// post sends a form as a signed-in browser would. Decision forms carry what the
// approver saw (the hidden "seen", and "id:fingerprint" for bulk), which a page
// always renders, so it is filled in from the current request when a test did
// not set it; postRaw sends exactly what it is given.
func (hs *harness) post(c *http.Client, path string, form url.Values) (*http.Response, string) {
	hs.t.Helper()
	fp := func(id string) string {
		n, err := strconv.ParseInt(id, 10, 64)
		if err != nil {
			return ""
		}
		r, err := hs.svc.Get(context.Background(), n)
		if err != nil {
			return ""
		}
		return r.Fingerprint()
	}
	if m := regexp.MustCompile(`^/leave/(\d+)/decide`).FindStringSubmatch(path); m != nil && form != nil && form.Get("seen") == "" {
		if f := fp(m[1]); f != "" {
			form = cloneForm(form)
			form.Set("seen", f)
		}
	}
	if path == "/approvals/approve" {
		form = cloneForm(form)
		for i, v := range form["id"] {
			if f := fp(v); f != "" {
				form["id"][i] = v + ":" + f
			}
		}
	}
	return hs.postRaw(c, path, form)
}

func cloneForm(f url.Values) url.Values {
	out := url.Values{}
	for k, v := range f {
		out[k] = append([]string(nil), v...)
	}
	return out
}

func (hs *harness) postRaw(c *http.Client, path string, form url.Values) (*http.Response, string) {
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

// The helper tests above call safeNext directly; this goes through POST /login, so
// a handler that redirected to the raw next field (or to something other than
// safeNext's answer) fails. A successful sign-in with a hostile next must land on
// the fallback, never on another host and never back on the sign-in page.
func TestLoginRefusesHostileNext(t *testing.T) {
	// webtest's client reads the raw Location, so a spelling Go's own client
	// refuses to parse (a tab inside the path) is asserted on, not a transport error.
	nexts := append([]string{"/\t/evil.example", "//evil.example", "/%6cogin", "https://evil.example/", "/login"}, webtest.HostileRedirects()...)
	for i, next := range nexts {
		t.Run(fmt.Sprintf("%d/%.30q", i, next), func(t *testing.T) {
			hs := newHarness(t)
			webtest.NewServer(t, hs.srv).PostForm("/login", url.Values{"username": {`EXAMPLE\Staff1`}, "password": {"x"}, "next": {next}}).
				WantRedirect(http.StatusSeeOther, "/")
		})
	}
}

// safeNext and safePath are thin wrappers over websec.SafeRedirect, which owns
// the algorithm and its own table. This pins the wiring: the options leave
// passes (Deny "/login" for safeNext only, "/" as the fallback), one hostile
// row per wrapper, and the value the app's own middleware generates.
func TestSafeNext(t *testing.T) {
	const consent = "/oauth/authorize?client_id=c&redirect_uri=http%3A%2F%2Flocalhost%3A1%2Fcb&state=s"
	for in, want := range map[string]string{
		"":               "/",
		"/team":          "/team",
		"/login?next=/x": "/", // the sign-in page itself is denied
		"/%6cogin":       "/", // Deny matches the decoded path
		"/loginhistory":  "/", // plain prefix, as before
		"/team\t":        "/",
		// A browser drops a literal tab, so this reads "//evil.example".
		"/\t/evil.example": "/",
		// The consent screen is reached through sign-in with its whole query,
		// so the person lands back on it: the middleware's RequestURI.
		consent: consent,
	} {
		if got := safeNext(in); got != want {
			t.Errorf("safeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSafePath(t *testing.T) {
	for in, want := range map[string]string{
		"":                     "/",
		"/team":                "/team",
		"/login":               "/login", // safePath has no Deny: the theme form may return to /login
		"https://evil.example": "/",
	} {
		if got := safePath(in); got != want {
			t.Errorf("safePath(%q) = %q, want %q", in, got, want)
		}
	}
	if got := safePath("/\t/evil.example"); got != "/" {
		t.Errorf("a literal tab hid a second slash: %q", got)
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
// GET-only sweep would prove nothing. The refusal is the real one per method:
// a GET is sent to the sign-in page, anything else gets 401 (a form post has
// no page to go back to). The mux is served without the CSRF layer (see
// newHarness), so no token is sent. The floor is the count of protected
// patterns today (68) minus a small margin; it was 2 and accepted any 303 or
// 401, so a route answering the wrong way for its method passed.
func TestProtectedRoutes(t *testing.T) {
	hs := newHarness(t)
	var patterns []string
	for _, rt := range hs.h.protectedRoutes() {
		patterns = append(patterns, rt.pattern)
	}
	webtest.Sweep{
		Routes:  webtest.Routes(t, patterns, map[string]string{"id": "1"}),
		Min:     64,
		Client:  func() *webtest.Client { return webtest.NewServer(t, hs.srv) },
		NoToken: true,
		Refused: webtest.ByMethod(
			webtest.RedirectsTo("/login?next="),
			webtest.Status(http.StatusUnauthorized)),
	}.Run(t)

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
	if n < 36 { // 33 before the stand-in pages; the count only goes up
		t.Fatalf("checked only %d templates", n)
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

// A username is the per-username limiter's key and is stored before the password
// is checked, so a long one is refused without being counted, and without being
// echoed back into the form.
func TestLoginRefusesOverlongUsername(t *testing.T) {
	hs := newHarness(t)
	c := hs.client()
	long := strings.Repeat("a", maxUsernameBytes+1)
	for range 10 {
		r, body := hs.post(c, "/login", url.Values{"username": {long}, "password": {"x"}})
		if r.StatusCode != http.StatusUnauthorized {
			t.Fatalf("overlong username: %d, want 401 (not 429: it must not spend a bucket)", r.StatusCode)
		}
		if strings.Contains(body, long) {
			t.Fatal("overlong username echoed back")
		}
	}
	if n := hs.h.loginByUser.Len(); n != 0 {
		t.Fatalf("overlong usernames created %d buckets", n)
	}
}

func TestLoginRefusesOversizedBody(t *testing.T) {
	hs := newHarness(t)
	r, _ := hs.post(hs.client(), "/login", url.Values{"username": {"staff1"}, "password": {strings.Repeat("p", maxFormBytes+1)}})
	if r.StatusCode != http.StatusBadRequest {
		t.Fatalf("oversized login body: %d, want 400", r.StatusCode)
	}
}

// The per-address sign-in limit keys IPv6 by /64, so a client cannot walk
// around it by changing the low bits of its address.
func TestLoginIPLimitSharesAnIPv6Prefix(t *testing.T) {
	hs := newHarness(t)
	post := func(addr string, n int) int {
		form := url.Values{"username": {"nobody" + strconv.Itoa(n)}, "password": {"x"}}
		req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.RemoteAddr = "[" + addr + "]:4000"
		rec := httptest.NewRecorder()
		hs.h.Login(rec, req)
		return rec.Code
	}
	// A fresh username each time keeps the per-username limit out of it.
	for i := range 20 {
		if code := post("2001:db8:0:1::"+strconv.FormatInt(int64(i+1), 16), i); code == http.StatusTooManyRequests {
			t.Fatalf("attempt %d refused before the burst was spent", i)
		}
	}
	if code := post("2001:db8:0:1:ffff::1", 100); code != http.StatusTooManyRequests {
		t.Errorf("another address in the same /64: %d, want 429", code)
	}
	if code := post("2001:db8:0:2::1", 101); code == http.StatusTooManyRequests {
		t.Error("an address in a different /64 shared the bucket")
	}
}

// With no TRUSTED_PROXIES, forwarding headers from an untrusted peer must not buy fresh sign-in IP buckets.
func TestLoginIPLimitIgnoresSpoofedHeaders(t *testing.T) {
	hs := newHarness(t)
	var last int
	for i := range 21 {
		form := url.Values{"username": {"nobody" + strconv.Itoa(i)}, "password": {"x"}}
		req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.RemoteAddr = "192.0.2.1:4000"
		req.Header.Set("X-Forwarded-For", "198.51.100."+strconv.Itoa(i+1))
		req.Header.Set("CF-Connecting-IP", "203.0.113."+strconv.Itoa(i+1))
		rec := httptest.NewRecorder()
		hs.h.Login(rec, req)
		last = rec.Code
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("21st attempt with a fresh spoofed address each time: %d, want 429", last)
	}
}
