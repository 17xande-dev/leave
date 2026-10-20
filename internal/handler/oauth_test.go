package handler

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/17xande-dev/leave/internal/oauth"
)

const cb = "http://localhost:3142/callback"

func (hs *harness) authorizeQuery(c oauth.Client) url.Values {
	sum := sha256.Sum256([]byte(strings.Repeat("v", 50)))
	return url.Values{"response_type": {"code"}, "client_id": {c.ID}, "redirect_uri": {cb},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"},
		"state": {"abc 123"}, "scope": {"leave"}, "resource": {"http://leave.test/mcp"}}
}

func (hs *harness) client_(name string) oauth.Client {
	hs.t.Helper()
	c, err := hs.oauth.RegisterClient(context.Background(), name, []string{cb})
	if err != nil {
		hs.t.Fatal(err)
	}
	return c
}

// An anonymous visitor goes to the normal sign-in page and, once in, lands on
// the same authorize URL: the consent screen reuses the app's sign-in.
func TestAuthorizeSignsInThenConsents(t *testing.T) {
	hs := newHarness(t)
	c := hs.client_("Claude")
	q := hs.authorizeQuery(c)
	authURL := "/oauth/authorize?" + q.Encode()

	anon := hs.client()
	res, err := anon.Get(hs.srv.URL + authURL)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	want := "/login?next=" + url.QueryEscape(authURL)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != want {
		t.Fatalf("anonymous authorize: %d %q, want redirect to %q", res.StatusCode, res.Header.Get("Location"), want)
	}

	// The sign-in page carries the URL through, and signing in returns to it.
	login, _ := url.Parse(res.Header.Get("Location"))
	code, body := hs.get(anon, login.RequestURI())
	if code != 200 || !strings.Contains(body, `name="next"`) {
		t.Fatalf("login page: %d", code)
	}
	r, _ := hs.post(anon, "/login", url.Values{"username": {"staff1"}, "password": {"x"}, "next": {authURL}})
	if r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != authURL {
		t.Fatalf("sign in from authorize: %d %q", r.StatusCode, r.Header.Get("Location"))
	}

	// The consent screen names the client and offers both answers as POST buttons.
	res, err = anon.Get(hs.srv.URL + authURL)
	if err != nil {
		t.Fatal(err)
	}
	b := new(strings.Builder)
	buf := make([]byte, 4096)
	for {
		n, err := res.Body.Read(buf)
		b.Write(buf[:n])
		if err != nil {
			break
		}
	}
	res.Body.Close()
	page := b.String()
	if res.StatusCode != 200 {
		t.Fatalf("consent: %d %s", res.StatusCode, page)
	}
	for _, want := range []string{"Claude", "localhost:3142", `method="post" action="/oauth/authorize"`,
		`name="decision" value="allow"`, `name="decision" value="deny"`, `name="client_id" value="` + c.ID + `"`,
		`name="state" value="abc 123"`} {
		if !strings.Contains(page, want) {
			t.Errorf("consent page lacks %q", want)
		}
	}
	// Browsers check the redirect after a form post against form-action; this
	// page alone allows its client's origin.
	if csp := res.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "form-action 'self' http://localhost:3142;") {
		t.Errorf("consent CSP = %q", csp)
	}
}

func TestConsentAllowAndDeny(t *testing.T) {
	hs := newHarness(t)
	c := hs.client_("Claude")
	staff := hs.signIn("staff1")
	form := func(decision string) url.Values {
		f := hs.authorizeQuery(c)
		if decision != "" {
			f.Set("decision", decision)
		}
		return f
	}

	r, _ := hs.post(staff, "/oauth/authorize", form("allow"))
	if r.StatusCode != http.StatusSeeOther {
		t.Fatalf("allow: %d", r.StatusCode)
	}
	loc, _ := url.Parse(r.Header.Get("Location"))
	if loc.Scheme+"://"+loc.Host+loc.Path != cb || loc.Query().Get("code") == "" || loc.Query().Get("state") != "abc 123" {
		t.Fatalf("allow redirect = %s", loc)
	}
	if r.Header.Get("Cache-Control") != "no-store" {
		t.Error("redirect carrying a code is cacheable")
	}

	r, _ = hs.post(staff, "/oauth/authorize", form("deny"))
	loc, _ = url.Parse(r.Header.Get("Location"))
	if r.StatusCode != http.StatusSeeOther || loc.Query().Get("error") != "access_denied" || loc.Query().Get("code") != "" ||
		loc.Query().Get("state") != "abc 123" {
		t.Fatalf("deny: %d %s", r.StatusCode, loc)
	}

	// Anything but an explicit allow or deny is refused, not read as consent.
	for _, d := range []string{"", "yes", "ALLOW"} {
		if r, _ := hs.post(staff, "/oauth/authorize", form(d)); r.StatusCode != http.StatusBadRequest {
			t.Errorf("decision %q: %d, want 400", d, r.StatusCode)
		}
	}

	// A tampered redirect URI is never followed: the person sees an error.
	bad := form("allow")
	bad.Set("redirect_uri", "http://evil.test/cb")
	r, body := hs.post(staff, "/oauth/authorize", bad)
	if r.StatusCode != http.StatusBadRequest || r.Header.Get("Location") != "" || !strings.Contains(body, "did not register") {
		t.Fatalf("tampered redirect: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	// A broken request from a known client goes back to the client as an error.
	noPKCE := form("allow")
	noPKCE.Del("code_challenge")
	r, _ = hs.post(staff, "/oauth/authorize", noPKCE)
	loc, _ = url.Parse(r.Header.Get("Location"))
	if r.StatusCode != http.StatusSeeOther || loc.Query().Get("error") != "invalid_request" {
		t.Fatalf("missing PKCE: %d %s", r.StatusCode, loc)
	}

	// An anonymous POST is refused outright, not replayed after sign-in.
	anon := hs.client()
	if r, _ := hs.post(anon, "/oauth/authorize", form("allow")); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous consent: %d, want 401", r.StatusCode)
	}
}

func TestConnectedApps(t *testing.T) {
	hs := newHarness(t)
	c := hs.client_("Claude <b>Desktop</b>")
	staff := hs.signIn("staff1")
	other := hs.signIn("staff2")

	if code, body := hs.get(staff, "/apps"); code != 200 || !strings.Contains(body, "No apps are connected") {
		t.Fatalf("empty apps page: %d", code)
	}

	// Connect it as staff1 the way a client does: consent, then exchange.
	f := hs.authorizeQuery(c)
	f.Set("decision", "allow")
	r, _ := hs.post(staff, "/oauth/authorize", f)
	loc, _ := url.Parse(r.Header.Get("Location"))
	res := exchangeForTest(t, hs, c, loc.Query().Get("code"))

	code, body := hs.get(staff, "/apps")
	if code != 200 || !strings.Contains(body, "Claude &lt;b&gt;Desktop&lt;/b&gt;") || strings.Contains(body, "<b>Desktop") ||
		!strings.Contains(body, "Not yet") {
		t.Fatalf("apps page: %d\n%s", code, body)
	}
	if _, body := hs.get(other, "/apps"); strings.Contains(body, "Claude") {
		t.Error("another person sees the app")
	}
	id := between(body, `action="/apps/`, `/revoke"`)
	if id == "" {
		t.Fatal("no revoke form")
	}

	// Another person's revoke is a 404 and changes nothing.
	if r, _ := hs.post(other, "/apps/"+id+"/revoke", nil); r.StatusCode != http.StatusNotFound {
		t.Errorf("revoking someone else's app: %d", r.StatusCode)
	}
	if _, err := hs.oauth.Verify(context.Background(), res, nil); err != nil {
		t.Fatalf("token died from someone else's revoke: %v", err)
	}

	r, _ = hs.post(staff, "/apps/"+id+"/revoke", nil)
	if r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/apps?notice=app-revoked" {
		t.Fatalf("revoke: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	if _, err := hs.oauth.Verify(context.Background(), res, nil); err == nil {
		t.Error("token works after the person disconnected the app")
	}
	if code, body := hs.get(staff, "/apps?notice=app-revoked"); code != 200 || !strings.Contains(body, "App disconnected") ||
		!strings.Contains(body, "No apps are connected") {
		t.Errorf("after revoke: %d", code)
	}
	// An unknown notice shows nothing.
	if _, body := hs.get(staff, "/apps?notice=<script>"); strings.Contains(body, "<script>") {
		t.Error("notice echoed")
	}
}

func TestPublicEndpointsNeedNoSession(t *testing.T) {
	hs := newHarness(t)
	anon := hs.client()
	for _, path := range []string{"/.well-known/oauth-authorization-server", "/.well-known/oauth-protected-resource",
		"/.well-known/oauth-protected-resource/mcp"} {
		if code, body := hs.get(anon, path); code != 200 || !strings.Contains(body, "leave.test") {
			t.Errorf("%s: %d", path, code)
		}
	}
	// /mcp answers an anonymous caller with the pointer to the metadata.
	res, err := anon.Post(hs.srv.URL+"/mcp", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if got := res.Header.Get("WWW-Authenticate"); res.StatusCode != 401 ||
		!strings.Contains(got, `resource_metadata="http://leave.test/.well-known/oauth-protected-resource"`) {
		t.Errorf("anonymous /mcp: %d %q", res.StatusCode, got)
	}
}

func exchangeForTest(t *testing.T, hs *harness, c oauth.Client, code string) (access string) {
	t.Helper()
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {c.ID},
		"redirect_uri": {cb}, "code_verifier": {strings.Repeat("v", 50)}}
	r, body := hs.post(hs.client(), "/oauth/token", form)
	if r.StatusCode != 200 {
		t.Fatalf("token: %d %s", r.StatusCode, body)
	}
	return between(body, `"access_token":"`, `"`)
}

func between(s, a, b string) string {
	i := strings.Index(s, a)
	if i < 0 {
		return ""
	}
	s = s[i+len(a):]
	j := strings.Index(s, b)
	if j < 0 {
		return ""
	}
	return s[:j]
}

func TestFormActionSource(t *testing.T) {
	for in, want := range map[string]string{
		"http://localhost:3142/cb":     "http://localhost:3142",
		"https://claude.ai/api/cb":     "https://claude.ai",
		"http://[::1]:8080/cb":         "http://[::1]:8080",
		"claude://callback":            "claude:",
		"https://a.test;script-src/cb": "",
		"https://a.test,b.test/cb":     "",
	} {
		if got := formActionSource(in); got != want {
			t.Errorf("formActionSource(%q) = %q, want %q", in, got, want)
		}
	}
}
