package main

import (
	"context"
	"database/sql"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"

	auths "github.com/17xande-dev/leave/internal/auth"
	"github.com/17xande-dev/leave/internal/config"
	"github.com/17xande-dev/leave/internal/dbtest"
	"github.com/17xande-dev/leave/internal/directory"
	"github.com/17xande-dev/leave/internal/handler"
	"github.com/17xande-dev/leave/internal/leave"
	"github.com/17xande-dev/leave/internal/outbox"
	"github.com/17xande-dev/leave/internal/users"
)

const callback = "http://127.0.0.1:45678/callback"

// e2e is the whole app: the real mux and middleware stack (CSRF, security
// headers) over a real database and the fake directory, served by httptest.
type e2e struct {
	t    *testing.T
	base string
	db   *sql.DB
	srv  *httptest.Server
}

func newE2E(t *testing.T) *e2e {
	t.Helper()
	ctx := context.Background()
	d := dbtest.New(t)
	dir, err := directory.LoadFake("testdata/directory.json")
	if err != nil {
		t.Fatal(err)
	}
	us := users.NewStore(d)
	svc := leave.New(d, us, leave.Config{Location: time.UTC})
	svc.Now = func() time.Time { return time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC) }
	if err := svc.SeedTypes(ctx); err != nil {
		t.Fatal(err)
	}
	all, _ := dir.AllUsers(ctx)
	if _, _, err := us.Sync(ctx, all); err != nil {
		t.Fatal(err)
	}
	pastor, _ := us.ByUsername(ctx, "pastor")
	hr, _ := us.ByUsername(ctx, "hr")
	st, _ := svc.Settings(ctx)
	st.FinalApproverID = pastor.ID
	if err := svc.SaveSettings(ctx, hr, st); err != nil {
		t.Fatal(err)
	}

	// The base URL is the listener's address, so it has to exist first.
	srv := httptest.NewUnstartedServer(nil)
	cfg := config.Config{CookieSecure: false, SessionTTL: time.Hour, Location: time.UTC,
		BaseURL: "http://" + srv.Listener.Addr().String()}
	jobs := outbox.NewStore(d)
	oa, mcpHandler := assistant(cfg, d, us, svc, jobs)
	app, err := newApp(cfg, handler.Deps{Users: us, Sessions: auths.NewSessions(d, time.Hour), Directory: dir,
		Leave: svc, Outbox: jobs, OAuth: oa, MCP: mcpHandler})
	if err != nil {
		t.Fatal(err)
	}
	srv.Config.Handler = app
	srv.Start()
	t.Cleanup(srv.Close)
	return &e2e{t: t, base: cfg.BaseURL, db: d, srv: srv}
}

var hiddenInput = regexp.MustCompile(`<input type="hidden" name="([^"]+)" value="([^"]*)">`)

// browser is a person's browser: cookies, no automatic redirects, and an
// Origin header on posts as a real one sends.
type browser struct {
	t    *testing.T
	base string
	c    *http.Client
}

func (e *e2e) browser() *browser {
	jar, _ := cookiejar.New(nil)
	return &browser{t: e.t, base: e.base, c: &http.Client{Jar: jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (b *browser) get(target string) (*http.Response, string) {
	b.t.Helper()
	if strings.HasPrefix(target, "/") {
		target = b.base + target
	}
	res, err := b.c.Get(target)
	if err != nil {
		b.t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res, string(body)
}

func (b *browser) post(path string, form url.Values) (*http.Response, string) {
	b.t.Helper()
	req, _ := http.NewRequest("POST", b.base+path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", b.base)
	res, err := b.c.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	return res, string(body)
}

// fields reads a form's hidden inputs, the CSRF token among them.
func fields(page string) url.Values {
	v := url.Values{}
	for _, m := range hiddenInput.FindAllStringSubmatch(page, -1) {
		v.Set(m[1], html.UnescapeString(m[2]))
	}
	return v
}

// signIn signs in through the real form, CSRF token and all.
func (b *browser) signIn(username, next string) {
	b.t.Helper()
	_, page := b.get("/login")
	f := fields(page)
	f.Set("username", username)
	f.Set("password", "anything")
	if next != "" {
		f.Set("next", next)
	}
	res, body := b.post("/login", f)
	if res.StatusCode != http.StatusSeeOther {
		b.t.Fatalf("sign in as %s: %d %s", username, res.StatusCode, body)
	}
}

// consent is the AuthorizationCodeFetcher: what a person does in a browser when
// an assistant sends them to the authorize URL. It signs in, answers the
// consent screen and reports the redirect the browser would have followed.
func (e *e2e) consent(username, decision string, seen *consentSeen) auth.AuthorizationCodeFetcher {
	return func(ctx context.Context, args *auth.AuthorizationArgs) (*auth.AuthorizationResult, error) {
		t := e.t
		b := e.browser()

		// Anonymous: sent to the sign-in page, remembering where to come back to.
		res, _ := b.get(args.URL)
		loc := res.Header.Get("Location")
		if res.StatusCode != http.StatusSeeOther || !strings.HasPrefix(loc, "/login?next=") {
			return nil, fmt.Errorf("anonymous authorize: %d %q", res.StatusCode, loc)
		}
		next, _ := url.Parse(loc)
		b.signIn(username, next.Query().Get("next"))

		// Signed in: the consent screen.
		authURL, _ := url.Parse(args.URL)
		res, page := b.get(authURL.RequestURI())
		if res.StatusCode != 200 || !strings.Contains(page, "E2E assistant") {
			return nil, fmt.Errorf("consent page: %d %s", res.StatusCode, page)
		}
		if seen != nil {
			seen.csp = res.Header.Get("Content-Security-Policy")
		}
		f := fields(page)
		if f.Get("csrf_token") == "" || f.Get("client_id") == "" || f.Get("code_challenge") == "" {
			return nil, fmt.Errorf("consent form lacks fields: %v", f)
		}

		// The consent form keeps its CSRF protection.
		noToken := url.Values{}
		for k, v := range f {
			if k != "csrf_token" {
				noToken[k] = v
			}
		}
		noToken.Set("decision", "allow")
		if res, _ := b.post("/oauth/authorize", noToken); res.StatusCode != http.StatusForbidden {
			t.Errorf("consent without a CSRF token: %d, want 403", res.StatusCode)
		}

		f.Set("decision", decision)
		res, body := b.post("/oauth/authorize", f)
		if res.StatusCode != http.StatusSeeOther {
			return nil, fmt.Errorf("consent answer: %d %s", res.StatusCode, body)
		}
		back, err := url.Parse(res.Header.Get("Location"))
		if err != nil || back.Scheme+"://"+back.Host+back.Path != callback {
			return nil, fmt.Errorf("redirected to %q, want %s", res.Header.Get("Location"), callback)
		}
		if e := back.Query().Get("error"); e != "" {
			return nil, fmt.Errorf("authorization refused: %s", e)
		}
		return &auth.AuthorizationResult{Code: back.Query().Get("code"), State: back.Query().Get("state")}, nil
	}
}

type consentSeen struct{ csp string }

// connect runs the SDK's own OAuth client against the app: discovery, dynamic
// registration, authorize (through the browser above), code exchange, then MCP.
func (e *e2e) connect(username string, seen *consentSeen) (*mcp.ClientSession, *auth.AuthorizationCodeHandler) {
	e.t.Helper()
	h, err := auth.NewAuthorizationCodeHandler(&auth.AuthorizationCodeHandlerConfig{
		DynamicClientRegistrationConfig: &auth.DynamicClientRegistrationConfig{Metadata: &oauthex.ClientRegistrationMetadata{
			ClientName: "E2E assistant", RedirectURIs: []string{callback},
			GrantTypes: []string{"authorization_code", "refresh_token"}, ResponseTypes: []string{"code"},
			TokenEndpointAuthMethod: "none",
		}},
		RedirectURL:              callback,
		AuthorizationCodeFetcher: e.consent(username, "allow", seen),
	})
	if err != nil {
		e.t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "1"}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: e.base + "/mcp", OAuthHandler: h,
		DisableStandaloneSSE: true}, nil)
	if err != nil {
		e.t.Fatalf("connect as %s: %v", username, err)
	}
	e.t.Cleanup(func() { cs.Close() })
	return cs, h
}

// rawMCP is one MCP request with a bearer token and no SDK client, so the
// HTTP status is visible: the SDK client answers a 401 by signing in again.
func (e *e2e) rawMCP(token, host string) *http.Response {
	e.t.Helper()
	r, _ := http.NewRequest("POST", e.base+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if host != "" {
		r.Host = host
	}
	r.Header.Set("Authorization", "Bearer "+token)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	res, err := http.DefaultClient.Do(r)
	if err != nil {
		e.t.Fatal(err)
	}
	res.Body.Close()
	return res
}

// refresh redeems a refresh token at the token endpoint, as a client does.
func (e *e2e) refresh(refreshToken string) (status int, body string) {
	var clientID string
	e.db.QueryRow(`SELECT id FROM oauth_clients LIMIT 1`).Scan(&clientID)
	res, err := http.PostForm(e.base+"/oauth/token", url.Values{"grant_type": {"refresh_token"},
		"refresh_token": {refreshToken}, "client_id": {clientID}})
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return res.StatusCode, string(b)
}

func call(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) (*mcp.CallToolResult, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return cs.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
}

func text(r *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

// TestEndToEndOAuthAndTools is the whole path: a real OAuth client discovers the
// server, registers, sends a person through sign-in and consent, exchanges the
// code, and calls tools with the token.
func TestEndToEndOAuthAndTools(t *testing.T) {
	e := newE2E(t)
	var seen consentSeen
	cs, handlerOAuth := e.connect("staff1", &seen)

	// The consent page let the browser follow the redirect to the client's own origin.
	if !strings.Contains(seen.csp, "form-action 'self' http://127.0.0.1:45678;") {
		t.Errorf("consent CSP = %q", seen.csp)
	}

	// The tool list came through the token.
	tools, err := cs.ListTools(context.Background(), nil)
	if err != nil || len(tools.Tools) < 10 {
		t.Fatalf("tools: %v, %v", tools, err)
	}

	res, err := call(t, cs, "my_balances", nil)
	if err != nil || res.IsError || !strings.Contains(text(res), "Annual leave") {
		t.Fatalf("my_balances: %v %v", err, res)
	}
	sc, _ := res.StructuredContent.(map[string]any)
	if sc == nil || len(sc["balances"].([]any)) == 0 {
		t.Errorf("no structured content: %v", res.StructuredContent)
	}

	// Acting as the person: the request is Sam Staff's, routed to Sam's manager.
	res, err = call(t, cs, "apply_for_leave", map[string]any{"type": "annual", "start": "2026-10-12", "end": "2026-10-14"})
	if err != nil || res.IsError {
		t.Fatalf("apply_for_leave: %v %v", err, text(res))
	}
	req := res.StructuredContent.(map[string]any)["request"].(map[string]any)
	if req["person"] != "Sam Staff" || req["status"] != "pending" || req["approver"] != "Olivia Operations" || req["days"].(float64) != 3 {
		t.Fatalf("request = %v", req)
	}
	// ...and it is in the database as the person's, through the same service the pages use.
	var who string
	e.db.QueryRow(`SELECT u.username FROM requests r JOIN users u ON u.id = r.user_id WHERE r.id = ?`, req["id"]).Scan(&who)
	if who != "staff1" {
		t.Errorf("request belongs to %q", who)
	}

	// Staff may not use an admin tool: a tool error with the human message.
	res, err = call(t, cs, "all_requests", nil)
	if err != nil || !res.IsError || !strings.Contains(text(res), "needs a leave administrator") || res.StructuredContent != nil {
		t.Fatalf("all_requests as staff: err=%v res=%v", err, res)
	}

	// The browser's Connected apps page lists it, with a last-used time now.
	b := e.browser()
	b.signIn("staff1", "")
	res2, page := b.get("/apps")
	if res2.StatusCode != 200 || !strings.Contains(page, "E2E assistant") || strings.Contains(page, "Not yet") {
		t.Fatalf("apps page: %d\n%s", res2.StatusCode, page)
	}
	id := regexp.MustCompile(`/apps/([^/]+)/revoke`).FindStringSubmatch(page)
	if id == nil {
		t.Fatal("no disconnect button")
	}

	// The token works, then a refresh rotates it: the new access token works
	// and the old one does not.
	ts, _ := handlerOAuth.TokenSource(context.Background())
	tok, err := ts.Token()
	if err != nil || tok.AccessToken == "" || tok.RefreshToken == "" {
		t.Fatalf("token: %+v %v", tok, err)
	}
	if got := e.rawMCP(tok.AccessToken, "").StatusCode; got != http.StatusOK {
		t.Fatalf("access token: %d", got)
	}
	status, body := e.refresh(tok.RefreshToken)
	if status != 200 {
		t.Fatalf("refresh: %d %s", status, body)
	}
	newAccess := regexp.MustCompile(`"access_token":"([^"]+)"`).FindStringSubmatch(body)[1]
	newRefresh := regexp.MustCompile(`"refresh_token":"([^"]+)"`).FindStringSubmatch(body)[1]
	if e.rawMCP(newAccess, "").StatusCode != http.StatusOK || e.rawMCP(tok.AccessToken, "").StatusCode != http.StatusUnauthorized {
		t.Error("rotation: the new access token should work and the old one not")
	}

	// The DNS-rebinding guard: a request arriving on the loopback address with
	// another Host is refused by the SDK, even with a good token. Behind a
	// reverse proxy that connects over loopback and forwards the public Host,
	// every call would get this; the README says how to avoid it.
	if got := e.rawMCP(newAccess, "leave.example.com").StatusCode; got != http.StatusForbidden {
		t.Errorf("loopback with a foreign Host: %d, want 403", got)
	}

	// Disconnecting from the page ends the token and its refresh token.
	res3, _ := b.post("/apps/"+id[1]+"/revoke", fields(page))
	if res3.StatusCode != http.StatusSeeOther {
		t.Fatalf("disconnect: %d", res3.StatusCode)
	}
	if got := e.rawMCP(newAccess, "").StatusCode; got != http.StatusUnauthorized {
		t.Errorf("a disconnected app's access token: %d, want 401", got)
	}
	if status, _ := e.refresh(newRefresh); status != http.StatusBadRequest {
		t.Errorf("a disconnected app's refresh token: %d, want 400", status)
	}
}

func TestEndToEndAdminToolsForAdmins(t *testing.T) {
	e := newE2E(t)
	cs, _ := e.connect("hr", nil)
	res, err := call(t, cs, "all_requests", nil)
	if err != nil || res.IsError {
		t.Fatalf("all_requests as admin: %v %v", err, res)
	}
	res, err = call(t, cs, "outbox_problems", nil)
	if err != nil || res.IsError {
		t.Fatalf("outbox_problems as admin: %v %v", err, res)
	}
}

// A person disabled in the directory loses the assistants' access at once, on
// the same check that ends their session.
func TestEndToEndDisabledUserTokenRefused(t *testing.T) {
	e := newE2E(t)
	cs, h := e.connect("staff1", nil)
	if res, err := call(t, cs, "my_balances", nil); err != nil || res.IsError {
		t.Fatalf("before: %v %v", err, res)
	}
	ts, _ := h.TokenSource(context.Background())
	tok, _ := ts.Token()
	if _, err := e.db.Exec(`UPDATE users SET disabled = 1 WHERE username = 'staff1'`); err != nil {
		t.Fatal(err)
	}
	// Raw, so the status is visible: 401 with the pointer to sign in again.
	r, _ := http.NewRequest("POST", e.base+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	r.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	rr, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	rr.Body.Close()
	if rr.StatusCode != http.StatusUnauthorized || !strings.Contains(rr.Header.Get("WWW-Authenticate"), "resource_metadata=") {
		t.Errorf("disabled user's token: %d %q", rr.StatusCode, rr.Header.Get("WWW-Authenticate"))
	}
	if status, _ := e.refresh(tok.RefreshToken); status != http.StatusBadRequest {
		t.Errorf("disabled user's refresh token: %d, want 400", status)
	}
}

// A person who presses Deny gives the client an access_denied error and no token.
func TestEndToEndDenyConnectsNothing(t *testing.T) {
	e := newE2E(t)
	h, err := auth.NewAuthorizationCodeHandler(&auth.AuthorizationCodeHandlerConfig{
		DynamicClientRegistrationConfig: &auth.DynamicClientRegistrationConfig{Metadata: &oauthex.ClientRegistrationMetadata{
			ClientName: "E2E assistant", RedirectURIs: []string{callback}}},
		RedirectURL:              callback,
		AuthorizationCodeFetcher: e.consent("staff1", "deny", nil),
	})
	if err != nil {
		t.Fatal(err)
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "e2e", Version: "1"}, nil)
	_, err = client.Connect(context.Background(), &mcp.StreamableClientTransport{Endpoint: e.base + "/mcp",
		OAuthHandler: h, DisableStandaloneSSE: true}, nil)
	if err == nil || !strings.Contains(err.Error(), "access_denied") {
		t.Fatalf("deny: err = %v", err)
	}
	var n int
	e.db.QueryRow(`SELECT count(*) FROM oauth_grants`).Scan(&n)
	if n != 0 {
		t.Errorf("%d grants after a denial", n)
	}
}

// The OAuth and metadata endpoints are reachable by programs: no cookie, no CSRF token.
func TestOAuthEndpointsRespondToPrograms(t *testing.T) {
	e := newE2E(t)
	for _, p := range csrfExempt {
		req, _ := http.NewRequest("POST", e.base+p, strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode == http.StatusForbidden && p != "/mcp" {
			t.Errorf("POST %s: 403; the CSRF layer is in the way", p)
		}
		if res.StatusCode == http.StatusNotFound {
			t.Errorf("POST %s: 404; the exempt path is not routed", p)
		}
	}
}
