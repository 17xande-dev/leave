package oauth

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"

	"github.com/17xande-dev/leave/internal/dbtest"
	"github.com/17xande-dev/leave/internal/directory"
	"github.com/17xande-dev/leave/internal/users"
)

const redirect = "http://localhost:3142/callback"

type env struct {
	t    *testing.T
	s    *Server
	db   *sql.DB
	user users.User
	now  time.Time
}

func newEnv(t *testing.T) *env {
	t.Helper()
	d := dbtest.New(t)
	u, err := users.NewStore(d).Upsert(context.Background(),
		directory.User{ID: "g1", DN: "CN=alice", Username: "alice", Name: "Alice"})
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, db: d, user: u, now: time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC),
		s: New(d, Config{BaseURL: "http://leave.test/"})}
	e.s.Now = func() time.Time { return e.now }
	return e
}

func (e *env) register() Client {
	e.t.Helper()
	c, err := e.s.RegisterClient(context.Background(), "Test assistant", []string{redirect})
	if err != nil {
		e.t.Fatal(err)
	}
	return c
}

func pkce() (verifier, challenge string) {
	verifier = strings.Repeat("v", 43) + "-._~" + strings.Repeat("w", 10)
	sum := sha256.Sum256([]byte(verifier))
	return verifier, base64.RawURLEncoding.EncodeToString(sum[:])
}

func authValues(c Client, challenge string) url.Values {
	return url.Values{"response_type": {"code"}, "client_id": {c.ID}, "redirect_uri": {redirect},
		"code_challenge": {challenge}, "code_challenge_method": {"S256"}, "state": {"st"}}
}

// code runs the authorize step as the consenting user and returns the code.
func (e *env) code(c Client, challenge string) string {
	e.t.Helper()
	req, aerr := e.s.ParseAuthorize(context.Background(), authValues(c, challenge))
	if aerr != nil {
		e.t.Fatalf("authorize: %v", aerr)
	}
	to, err := e.s.Approve(context.Background(), req, e.user.ID)
	if err != nil {
		e.t.Fatal(err)
	}
	u, _ := url.Parse(to)
	if u.Query().Get("state") != "st" {
		e.t.Fatalf("state not echoed in %s", to)
	}
	return u.Query().Get("code")
}

type tokenReply struct {
	Status       int
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
	Error        string `json:"error"`
}

func (e *env) post(handler http.HandlerFunc, form url.Values) tokenReply {
	e.t.Helper()
	req := httptest.NewRequest("POST", "/oauth/token", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	handler(rec, req)
	var r tokenReply
	json.Unmarshal(rec.Body.Bytes(), &r)
	r.Status = rec.Code
	return r
}

func (e *env) exchange(c Client, code, verifier string) tokenReply {
	return e.post(e.s.Token, url.Values{"grant_type": {"authorization_code"}, "code": {code},
		"client_id": {c.ID}, "redirect_uri": {redirect}, "code_verifier": {verifier}})
}

func (e *env) refresh(c Client, token string) tokenReply {
	return e.post(e.s.Token, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {token}, "client_id": {c.ID}})
}

// connect runs the whole flow and returns the first tokens.
func (e *env) connect(c Client) tokenReply {
	e.t.Helper()
	v, ch := pkce()
	r := e.exchange(c, e.code(c, ch), v)
	if r.Status != 200 || r.AccessToken == "" || r.RefreshToken == "" {
		e.t.Fatalf("exchange: %+v", r)
	}
	return r
}

func (e *env) valid(token string) bool {
	e.t.Helper()
	_, err := e.s.Verify(context.Background(), token, nil)
	if err != nil && !errors.Is(err, auth.ErrInvalidToken) {
		e.t.Fatalf("verify: unexpected error %v", err)
	}
	return err == nil
}

func TestFlowIssuesWorkingTokens(t *testing.T) {
	e := newEnv(t)
	c := e.register()
	r := e.connect(c)
	if r.TokenType != "Bearer" || r.ExpiresIn != 3600 || r.Scope != Scope {
		t.Errorf("response = %+v", r)
	}
	info, err := e.s.Verify(context.Background(), r.AccessToken, nil)
	if err != nil {
		t.Fatal(err)
	}
	if info.UserID != "1" || len(info.Scopes) != 1 || info.Scopes[0] != Scope || !info.Expiration.Equal(e.now.Add(time.Hour)) {
		t.Errorf("token info = %+v", info)
	}
	// The first use stamps "last used" on the Connected apps page: the column
	// starts as '' (not NULL), which sorts before every timestamp.
	var last string
	e.db.QueryRow(`SELECT last_used_at FROM oauth_grants`).Scan(&last)
	if last == "" {
		t.Error("last_used_at not set by the first call")
	}
	// A refresh token is not an access token.
	if e.valid(r.RefreshToken) {
		t.Error("refresh token accepted as an access token")
	}
	// Only hashes are stored.
	var n int
	e.db.QueryRow(`SELECT count(*) FROM oauth_tokens WHERE token_hash IN (?, ?)`, r.AccessToken, r.RefreshToken).Scan(&n)
	if n != 0 {
		t.Error("a token is stored in the clear")
	}
}

func TestPKCEFailure(t *testing.T) {
	e := newEnv(t)
	c := e.register()
	verifier, challenge := pkce()
	code := e.code(c, challenge)

	if r := e.exchange(c, code, strings.Repeat("x", 50)); r.Status != 400 || r.Error != "invalid_grant" || r.AccessToken != "" {
		t.Fatalf("wrong verifier: %+v", r)
	}
	// The failed attempt spent the code: a guesser gets one try, and the
	// legitimate client with the right verifier now has to start again.
	if r := e.exchange(c, code, verifier); r.Status != 400 || r.Error != "invalid_grant" {
		t.Fatalf("code reused after a failed attempt: %+v", r)
	}

	// Too short or with characters outside the PKCE alphabet.
	for _, v := range []string{"", "short", strings.Repeat("a", 43) + "!", strings.Repeat("a", 129)} {
		code := e.code(c, challenge)
		if r := e.exchange(c, code, v); r.Status != 400 {
			t.Errorf("verifier %q accepted: %+v", v, r)
		}
	}
}

func TestAuthorizeRequiresS256PKCE(t *testing.T) {
	e := newEnv(t)
	c := e.register()
	_, challenge := pkce()
	for name, mutate := range map[string]func(url.Values){
		"no challenge":   func(v url.Values) { v.Del("code_challenge") },
		"plain method":   func(v url.Values) { v.Set("code_challenge_method", "plain") },
		"no method":      func(v url.Values) { v.Del("code_challenge_method") },
		"short":          func(v url.Values) { v.Set("code_challenge", "abc") },
		"bad characters": func(v url.Values) { v.Set("code_challenge", strings.Repeat("!", 43)) },
		"token flow":     func(v url.Values) { v.Set("response_type", "token") },
		"other scope":    func(v url.Values) { v.Set("scope", "leave admin") },
		"other resource": func(v url.Values) { v.Set("resource", "http://evil.test/mcp") },
		"long state":     func(v url.Values) { v.Set("state", strings.Repeat("s", 501)) },
		"repeated param": func(v url.Values) { v.Add("state", "again") },
	} {
		v := authValues(c, challenge)
		mutate(v)
		_, aerr := e.s.ParseAuthorize(context.Background(), v)
		if aerr == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		// The client and redirect URI were fine, so the client hears about it.
		// A repeated parameter is refused before either is trusted.
		if name != "repeated param" && !aerr.Redirect {
			t.Errorf("%s: not redirectable: %v", name, aerr)
		}
	}
	// The one scope and the matching resource are accepted.
	v := authValues(c, challenge)
	v.Set("scope", Scope)
	v.Set("resource", "http://leave.test/mcp")
	if _, aerr := e.s.ParseAuthorize(context.Background(), v); aerr != nil {
		t.Errorf("valid request refused: %v", aerr)
	}
}

func TestRedirectURIMustMatchExactly(t *testing.T) {
	e := newEnv(t)
	c := e.register()
	_, challenge := pkce()
	for _, bad := range []string{
		"", "http://localhost:3142/callback/", "http://localhost:3142/callbac", "http://localhost:3142/callback?x=1",
		"http://localhost:3143/callback", "http://evil.test/callback", "HTTP://localhost:3142/callback",
	} {
		v := authValues(c, challenge)
		v.Set("redirect_uri", bad)
		_, aerr := e.s.ParseAuthorize(context.Background(), v)
		if aerr == nil || aerr.Redirect {
			t.Errorf("redirect_uri %q: err = %v; want a refusal that does not redirect", bad, aerr)
		}
	}
	// An unknown client is not redirected either.
	v := authValues(c, challenge)
	v.Set("client_id", "nobody")
	if _, aerr := e.s.ParseAuthorize(context.Background(), v); aerr == nil || aerr.Redirect {
		t.Errorf("unknown client: %v", aerr)
	}

	// At the token endpoint the URI must be the one the code was issued for.
	verifier, ch := pkce()
	code := e.code(c, ch)
	r := e.post(e.s.Token, url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {c.ID},
		"redirect_uri": {"http://localhost:3142/other"}, "code_verifier": {verifier}})
	if r.Status != 400 || r.Error != "invalid_grant" {
		t.Errorf("token redirect mismatch: %+v", r)
	}
	// And the code belongs to the client it was issued to.
	other, _ := e.s.RegisterClient(context.Background(), "Other", []string{redirect})
	code = e.code(c, ch)
	if r := e.exchange(other, code, verifier); r.Status != 400 {
		t.Errorf("code redeemed by another client: %+v", r)
	}
}

func TestRegistrationRedirectURIs(t *testing.T) {
	e := newEnv(t)
	for uri, ok := range map[string]bool{
		"https://claude.ai/api/mcp/auth_callback": true,
		"http://localhost:3142/callback":          true,
		"http://127.0.0.1:9/cb":                   true,
		"http://[::1]:9/cb":                       true,
		"claude://callback":                       true,
		"http://example.com/cb":                   false,
		"http://localhost.evil.com/cb":            false,
		"javascript:alert(1)":                     false,
		"data:text/html,x":                        false,
		"file:///etc/passwd":                      false,
		"https://example.com/cb#frag":             false,
		"/relative":                               false,
		"":                                        false,
	} {
		_, err := e.s.RegisterClient(context.Background(), "x", []string{uri})
		var ie InvalidError
		if ok && err != nil {
			t.Errorf("%q refused: %v", uri, err)
		}
		if !ok && !errors.As(err, &ie) {
			t.Errorf("%q accepted, or failed for another reason: %v", uri, err)
		}
	}
}

func TestRegisterEndpoint(t *testing.T) {
	e := newEnv(t)
	do := func(body string) (int, map[string]any) {
		req := httptest.NewRequest("POST", "/oauth/register", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		e.s.Register(rec, req)
		var m map[string]any
		json.Unmarshal(rec.Body.Bytes(), &m)
		return rec.Code, m
	}
	code, m := do(`{"client_name":"  Claude\u0007 ","redirect_uris":["` + redirect + `"],"grant_types":["authorization_code","refresh_token"],"token_endpoint_auth_method":"client_secret_post"}`)
	if code != 201 || m["client_id"] == "" || m["client_name"] != "Claude" || m["token_endpoint_auth_method"] != "none" {
		t.Fatalf("register: %d %v", code, m)
	}
	if _, has := m["client_secret"]; has {
		t.Error("a public client was issued a secret")
	}
	for name, body := range map[string]string{
		"not json":      `nope`,
		"no uris":       `{"client_name":"x"}`,
		"bad uri":       `{"redirect_uris":["javascript:alert(1)"]}`,
		"implicit":      `{"redirect_uris":["` + redirect + `"],"grant_types":["implicit"]}`,
		"password":      `{"redirect_uris":["` + redirect + `"],"grant_types":["authorization_code","password"]}`,
		"token flow":    `{"redirect_uris":["` + redirect + `"],"response_types":["token"]}`,
		"other scope":   `{"redirect_uris":["` + redirect + `"],"scope":"leave admin"}`,
		"refresh alone": `{"redirect_uris":["` + redirect + `"],"grant_types":["refresh_token"]}`,
	} {
		if code, m := do(body); code != 400 || m["error"] == nil {
			t.Errorf("%s: %d %v", name, code, m)
		}
	}
	// A long name is cut, not refused.
	_, m = do(`{"client_name":"` + strings.Repeat("n", 500) + `","redirect_uris":["` + redirect + `"]}`)
	if got := m["client_name"].(string); len(got) != maxClientName {
		t.Errorf("name length %d, want %d", len(got), maxClientName)
	}
}

func TestCodeUsedTwiceRevokesTheGrant(t *testing.T) {
	e := newEnv(t)
	c := e.register()
	verifier, challenge := pkce()
	code := e.code(c, challenge)

	first := e.exchange(c, code, verifier)
	if first.Status != 200 {
		t.Fatalf("first use: %+v", first)
	}
	if r := e.exchange(c, code, verifier); r.Status != 400 || r.Error != "invalid_grant" {
		t.Fatalf("second use: %+v", r)
	}
	// Whoever got tokens the first time may be the thief: they are revoked.
	if e.valid(first.AccessToken) {
		t.Error("access token from a replayed code still works")
	}
	if r := e.refresh(c, first.RefreshToken); r.Status != 400 {
		t.Errorf("refresh token from a replayed code still works: %+v", r)
	}
	// An expired code is refused too.
	code = e.code(c, challenge)
	e.now = e.now.Add(CodeTTL + time.Second)
	if r := e.exchange(c, code, verifier); r.Status != 400 {
		t.Errorf("expired code accepted: %+v", r)
	}
}

func TestRefreshRotationAndReuse(t *testing.T) {
	e := newEnv(t)
	c := e.register()
	first := e.connect(c)

	second := e.refresh(c, first.RefreshToken)
	if second.Status != 200 || second.AccessToken == first.AccessToken || second.RefreshToken == first.RefreshToken {
		t.Fatalf("rotation: %+v", second)
	}
	if !e.valid(second.AccessToken) {
		t.Error("new access token does not work")
	}
	if e.valid(first.AccessToken) {
		t.Error("the access token a refresh replaced still works")
	}
	third := e.refresh(c, second.RefreshToken)
	if third.Status != 200 {
		t.Fatalf("chained refresh: %+v", third)
	}

	// The first refresh token is spent. Presenting it means two parties hold
	// it, so the grant ends: the newest tokens stop working too.
	if r := e.refresh(c, first.RefreshToken); r.Status != 400 || r.Error != "invalid_grant" {
		t.Fatalf("reuse: %+v", r)
	}
	if e.valid(third.AccessToken) {
		t.Error("access token survived the reuse of an old refresh token")
	}
	if r := e.refresh(c, third.RefreshToken); r.Status != 400 {
		t.Errorf("newest refresh token survived the reuse: %+v", r)
	}
	if gs, _ := e.s.Grants(context.Background(), e.user.ID); len(gs) != 0 {
		t.Errorf("revoked grant still listed: %+v", gs)
	}
}

func TestRefreshChecksTheClientAndScope(t *testing.T) {
	e := newEnv(t)
	c := e.register()
	other, _ := e.s.RegisterClient(context.Background(), "Other", []string{redirect})
	first := e.connect(c)
	if r := e.refresh(other, first.RefreshToken); r.Status != 400 {
		t.Errorf("refresh by another client: %+v", r)
	}
	r := e.post(e.s.Token, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {first.RefreshToken},
		"client_id": {c.ID}, "scope": {"leave admin"}})
	if r.Status != 400 || r.Error != "invalid_scope" {
		t.Errorf("wider scope: %+v", r)
	}
	// The refusals above did not spend or revoke anything.
	if r := e.refresh(c, first.RefreshToken); r.Status != 200 {
		t.Errorf("legitimate refresh after refusals: %+v", r)
	}
}

func TestTokenLifetimes(t *testing.T) {
	e := newEnv(t)
	c := e.register()
	r := e.connect(c)

	e.now = e.now.Add(AccessTTL + time.Second)
	if e.valid(r.AccessToken) {
		t.Error("access token valid past its hour")
	}
	// The refresh token outlives it, and is good for a fresh hour.
	n := e.refresh(c, r.RefreshToken)
	if n.Status != 200 || !e.valid(n.AccessToken) {
		t.Fatalf("refresh after expiry: %+v", n)
	}
	e.now = e.now.Add(RefreshTTL + time.Second)
	if r := e.refresh(c, n.RefreshToken); r.Status != 400 {
		t.Errorf("refresh token valid past 30 days: %+v", r)
	}
}

func TestDisabledUserTokensStopAtOnce(t *testing.T) {
	e := newEnv(t)
	c := e.register()
	r := e.connect(c)
	if !e.valid(r.AccessToken) {
		t.Fatal("token should work before the user is disabled")
	}
	if _, err := e.db.Exec(`UPDATE users SET disabled = 1 WHERE id = ?`, e.user.ID); err != nil {
		t.Fatal(err)
	}
	_, err := e.s.Verify(context.Background(), r.AccessToken, nil)
	if !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("disabled user's token: err = %v, want ErrInvalidToken", err)
	}
	if n := e.refresh(c, r.RefreshToken); n.Status != 400 {
		t.Errorf("disabled user refreshed: %+v", n)
	}
	// Re-enabled in the directory: the same grant works again, as a session would.
	e.db.Exec(`UPDATE users SET disabled = 0 WHERE id = ?`, e.user.ID)
	if !e.valid(r.AccessToken) {
		t.Error("token did not come back with the user")
	}
	// A disabled user cannot complete a flow that was already under way either.
	v, ch := pkce()
	code := e.code(c, ch)
	e.db.Exec(`UPDATE users SET disabled = 1 WHERE id = ?`, e.user.ID)
	if n := e.exchange(c, code, v); n.Status != 400 {
		t.Errorf("code redeemed for a disabled user: %+v", n)
	}
}

func TestRevokeEndpointAndGrants(t *testing.T) {
	e := newEnv(t)
	c := e.register()
	r := e.connect(c)

	gs, err := e.s.Grants(context.Background(), e.user.ID)
	if err != nil || len(gs) != 1 || gs[0].ClientName != "Test assistant" || gs[0].LastUsedAt != "" {
		t.Fatalf("grants = %+v, %v", gs, err)
	}
	e.valid(r.AccessToken) // a call: "last used" is set
	gs, _ = e.s.Grants(context.Background(), e.user.ID)
	if gs[0].LastUsedAt == "" {
		t.Error("last used not recorded")
	}

	// Someone else cannot revoke it, or learn that it exists.
	if err := e.s.RevokeGrant(context.Background(), e.user.ID+1, gs[0].ID); !errors.Is(err, ErrNotFound) {
		t.Errorf("revoking another person's grant: %v", err)
	}
	if !e.valid(r.AccessToken) {
		t.Fatal("token died from someone else's revoke attempt")
	}

	// RFC 7009: revoking either token ends the whole grant; unknown tokens are not an error.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/oauth/revoke", strings.NewReader(url.Values{"token": {r.RefreshToken}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	e.s.Revoke(rec, req)
	if rec.Code != 200 {
		t.Fatalf("revoke: %d", rec.Code)
	}
	if e.valid(r.AccessToken) {
		t.Error("access token survived revoking its refresh token")
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest("POST", "/oauth/revoke", strings.NewReader(url.Values{"token": {"unknown"}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	e.s.Revoke(rec, req)
	if rec.Code != 200 {
		t.Errorf("revoking an unknown token: %d", rec.Code)
	}

	// Revoking from the Connected apps page.
	r2 := e.connect(c)
	gs, _ = e.s.Grants(context.Background(), e.user.ID)
	if len(gs) != 1 {
		t.Fatalf("grants after revoke and reconnect = %+v", gs)
	}
	if err := e.s.RevokeGrant(context.Background(), e.user.ID, gs[0].ID); err != nil {
		t.Fatal(err)
	}
	if e.valid(r2.AccessToken) {
		t.Error("token survived revoking the grant")
	}
	if gs, _ = e.s.Grants(context.Background(), e.user.ID); len(gs) != 0 {
		t.Errorf("revoked grant listed: %+v", gs)
	}
}

func TestTokenEndpointIsStrict(t *testing.T) {
	e := newEnv(t)
	c := e.register()
	v, ch := pkce()
	code := e.code(c, ch)
	for name, form := range map[string]url.Values{
		"no grant type":    {"code": {code}},
		"password grant":   {"grant_type": {"password"}, "username": {"a"}, "password": {"b"}},
		"missing code":     {"grant_type": {"authorization_code"}, "client_id": {c.ID}, "redirect_uri": {redirect}, "code_verifier": {v}},
		"missing client":   {"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirect}, "code_verifier": {v}},
		"missing verifier": {"grant_type": {"authorization_code"}, "code": {code}, "client_id": {c.ID}, "redirect_uri": {redirect}},
		"wrong resource":   {"grant_type": {"authorization_code"}, "code": {code}, "client_id": {c.ID}, "redirect_uri": {redirect}, "code_verifier": {v}, "resource": {"http://evil.test/mcp"}},
		"repeated":         {"grant_type": {"authorization_code", "refresh_token"}},
	} {
		if r := e.post(e.s.Token, form); r.Status != 400 || r.Error == "" {
			t.Errorf("%s: %+v", name, r)
		}
	}
	// None of those spent the good code except by being a real attempt at it.
	if r := e.exchange(c, code, v); r.Status != 200 {
		t.Errorf("good exchange after malformed requests: %+v", r)
	}
	// A secret in the query string is not read.
	req := httptest.NewRequest("POST", "/oauth/token?grant_type=refresh_token&refresh_token=x&client_id=y", nil)
	rec := httptest.NewRecorder()
	e.s.Token(rec, req)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "unsupported_grant_type") {
		t.Errorf("query-string parameters were read: %d %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q", got)
	}
}

func TestTokenIssuanceIsRateLimited(t *testing.T) {
	e := newEnv(t)
	var last int
	for range 70 {
		last = e.post(e.s.Token, url.Values{"grant_type": {"nope"}}).Status
	}
	if last != http.StatusTooManyRequests {
		t.Fatalf("70th request: %d, want 429", last)
	}
}

func TestMetadataMatchesTheEndpoints(t *testing.T) {
	e := newEnv(t)
	rec := httptest.NewRecorder()
	e.s.AuthorizationServerMetadata().ServeHTTP(rec, httptest.NewRequest("GET", "/.well-known/oauth-authorization-server", nil))
	var as map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &as); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{"issuer": "http://leave.test", "authorization_endpoint": "http://leave.test/oauth/authorize",
		"token_endpoint": "http://leave.test/oauth/token", "registration_endpoint": "http://leave.test/oauth/register",
		"revocation_endpoint": "http://leave.test/oauth/revoke"} {
		if as[k] != want {
			t.Errorf("%s = %v, want %s", k, as[k], want)
		}
	}
	if m, _ := as["code_challenge_methods_supported"].([]any); len(m) != 1 || m[0] != "S256" {
		t.Errorf("code_challenge_methods_supported = %v", as["code_challenge_methods_supported"])
	}

	rec = httptest.NewRecorder()
	e.s.ResourceMetadata().ServeHTTP(rec, httptest.NewRequest("GET", "/.well-known/oauth-protected-resource", nil))
	var pr map[string]any
	json.Unmarshal(rec.Body.Bytes(), &pr)
	if pr["resource"] != "http://leave.test/mcp" || pr["authorization_servers"].([]any)[0] != "http://leave.test" {
		t.Errorf("protected resource metadata = %v", pr)
	}
}

func TestSweep(t *testing.T) {
	e := newEnv(t)
	c := e.register()
	r := e.connect(c)
	unused, _ := e.s.RegisterClient(context.Background(), "Never finished", []string{redirect})
	e.code(c, strings.Repeat("a", 43)) // a code nobody redeems

	if n, err := e.s.Sweep(context.Background()); err != nil || n != 0 {
		t.Fatalf("sweep of live data removed %d, %v", n, err)
	}
	e.now = e.now.Add(RefreshTTL + 48*time.Hour)
	if n, err := e.s.Sweep(context.Background()); err != nil || n == 0 {
		t.Fatalf("sweep of expired data removed %d, %v", n, err)
	}
	var left int
	e.db.QueryRow(`SELECT (SELECT count(*) FROM oauth_tokens) + (SELECT count(*) FROM oauth_grants) +
		(SELECT count(*) FROM oauth_codes) + (SELECT count(*) FROM oauth_clients)`).Scan(&left)
	if left != 0 {
		t.Errorf("%d rows left after everything expired", left)
	}
	if e.valid(r.AccessToken) {
		t.Error("swept token valid")
	}
	_ = unused
}
