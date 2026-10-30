// Package oauth is the app acting as an OAuth 2.1 authorization server for its
// own MCP endpoint: people sign in here (the same LDAP sign-in as the pages),
// consent to an AI assistant, and the assistant gets a bearer token that acts as
// them and no one else.
//
// What is supported is exactly what MCP clients use: authorization code with
// PKCE (S256 only), dynamic client registration for public clients, refresh
// token rotation and revocation. There are no client secrets: the registration
// proves nothing about who a client is, so a secret would add ceremony and no
// security, while PKCE binds each code to the party that started the flow.
//
// Codes and tokens are opaque 256-bit random values and only their sha256 is
// stored, as with sessions. A token acts as its user through the same lookup
// sessions use: disabling someone in the directory ends their tokens at once.
package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"

	"github.com/17xande-dev/leave/internal/middleware"
)

const (
	// Scope is the one scope: everything the person could do themselves.
	// Narrower scopes would be a second permission system next to the app's own.
	Scope = "leave"

	// ResourcePath is where the MCP endpoint is mounted.
	ResourcePath = "/mcp"

	CodeTTL    = 10 * time.Minute
	AccessTTL  = time.Hour
	RefreshTTL = 30 * 24 * time.Hour
)

// ErrNotFound is a grant that does not exist or is not the caller's.
var ErrNotFound = errors.New("oauth: not found")

type Config struct {
	// BaseURL is the origin people reach the app at, with no trailing slash. It
	// is the issuer, and the MCP endpoint is BaseURL + ResourcePath.
	BaseURL        string
	TrustedProxies []*net.IPNet
}

type Server struct {
	db  *sql.DB
	cfg Config
	// Now is replaceable so tests can step past an expiry.
	Now func() time.Time

	// Token issuance and registration are limited per client IP. The limits are
	// loose on purpose: claude.ai calls from a few shared addresses on behalf
	// of everyone, and the secrets are 256 bits so there is nothing to guess.
	// They stop a script from filling the database, not a guesser.
	tokenLimit    *middleware.Limiter
	registerLimit *middleware.Limiter
}

func New(db *sql.DB, cfg Config) *Server {
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	return &Server{db: db, cfg: cfg, Now: time.Now,
		tokenLimit:    middleware.NewLimiter(time.Second, 60),
		registerLimit: middleware.NewLimiter(6*time.Second, 20),
	}
}

// Resource is the MCP endpoint's URL: the audience tokens are issued for.
func (s *Server) Resource() string { return s.cfg.BaseURL + ResourcePath }

// ResourceMetadataURL is where the endpoint says its metadata is, in the
// WWW-Authenticate header of a 401.
func (s *Server) ResourceMetadataURL() string {
	return s.cfg.BaseURL + "/.well-known/oauth-protected-resource"
}

func (s *Server) now() time.Time { return s.Now().UTC() }

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func hash(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

func random() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// --- Clients ---------------------------------------------------------------

type Client struct {
	ID           string
	Name         string
	RedirectURIs []string
}

func (c Client) hasRedirect(uri string) bool {
	for _, r := range c.RedirectURIs {
		if r == uri {
			return true
		}
	}
	return false
}

func (s *Server) client(ctx context.Context, id string) (Client, error) {
	var c Client
	var uris string
	err := s.db.QueryRowContext(ctx, `SELECT id, name, redirect_uris FROM oauth_clients WHERE id = ?`, id).
		Scan(&c.ID, &c.Name, &uris)
	if errors.Is(err, sql.ErrNoRows) {
		return Client{}, ErrNotFound
	}
	if err != nil {
		return Client{}, err
	}
	if err := json.Unmarshal([]byte(uris), &c.RedirectURIs); err != nil {
		return Client{}, fmt.Errorf("oauth: client %s redirect uris: %w", id, err)
	}
	return c, nil
}

// maxClientName bounds what an anonymous registration can put on the consent
// screen and the Connected apps page.
const maxClientName = 100

func cleanName(name string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(name) {
		if r >= ' ' && r != 0x7f {
			b.WriteRune(r)
		}
	}
	n := []rune(strings.TrimSpace(b.String()))
	if len(n) > maxClientName {
		n = n[:maxClientName]
	}
	if len(n) == 0 {
		return "Unnamed application"
	}
	return string(n)
}

// Redirect schemes that run code or read files when navigated to.
var dangerousSchemes = map[string]bool{"javascript": true, "data": true, "file": true, "vbscript": true,
	"blob": true, "about": true, "ftp": true, "ws": true, "wss": true}

// validRedirectURI accepts what real clients register: https, http only to
// the loopback (a desktop app's local listener), or an app's own scheme
// (claude://, cursor://). Fragments are refused, as the RFC says.
func validRedirectURI(raw string) error {
	if raw == "" || len(raw) > 2000 {
		return errors.New("redirect URI is empty or too long")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Fragment != "" || strings.Contains(raw, "#") {
		return errors.New("redirect URI must be an absolute URI without a fragment")
	}
	switch u.Scheme {
	case "https":
		if u.Host == "" {
			return errors.New("https redirect URI needs a host")
		}
	case "http":
		if !IsLoopbackHost(u.Hostname()) {
			return errors.New("http redirect URIs are only allowed for localhost")
		}
	default:
		if dangerousSchemes[u.Scheme] {
			return fmt.Errorf("redirect URI scheme %q is not allowed", u.Scheme)
		}
		if u.Host == "" && u.Opaque == "" && u.Path == "" {
			return errors.New("redirect URI is empty")
		}
	}
	return nil
}

// IsLoopbackHost reports whether host names this machine.
func IsLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// InvalidError is a registration the caller got wrong, as opposed to one that
// failed on our side.
type InvalidError string

func (e InvalidError) Error() string { return string(e) }

// RegisterClient stores a client. The redirect URIs are validated here, and
// are the only ones the client may ever be sent back to.
func (s *Server) RegisterClient(ctx context.Context, name string, uris []string) (Client, error) {
	if len(uris) == 0 || len(uris) > 10 {
		return Client{}, InvalidError("give between 1 and 10 redirect URIs")
	}
	for _, u := range uris {
		if err := validRedirectURI(u); err != nil {
			return Client{}, InvalidError(err.Error())
		}
	}
	id, err := random()
	if err != nil {
		return Client{}, err
	}
	c := Client{ID: id[:22], Name: cleanName(name), RedirectURIs: uris}
	j, _ := json.Marshal(uris)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO oauth_clients (id, name, redirect_uris, created_at)
		VALUES (?, ?, ?, ?)`, c.ID, c.Name, string(j), stamp(s.now())); err != nil {
		return Client{}, err
	}
	return c, nil
}

// --- Authorization ---------------------------------------------------------

// AuthRequest is a validated authorization request.
type AuthRequest struct {
	Client        Client
	RedirectURI   string
	State         string
	Scope         string
	CodeChallenge string
}

// AuthError is a refused authorization request. When Redirect is false the
// client or redirect URI could not be trusted, so the person is shown the
// problem instead of being sent anywhere.
type AuthError struct {
	Code        string
	Description string
	Redirect    bool
	req         AuthRequest
}

func (e *AuthError) Error() string { return e.Code + ": " + e.Description }

// RedirectURL is where a redirectable error goes.
func (e *AuthError) RedirectURL() string {
	return e.req.respond(url.Values{"error": {e.Code}, "error_description": {e.Description}})
}

func (r AuthRequest) respond(extra url.Values) string {
	u, _ := url.Parse(r.RedirectURI)
	q := u.Query()
	for k, v := range extra {
		q.Set(k, v[0])
	}
	if r.State != "" {
		q.Set("state", r.State)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// DenyURL is where a person who pressed Deny goes.
func (r AuthRequest) DenyURL() string {
	return r.respond(url.Values{"error": {"access_denied"}, "error_description": {"The person declined."}})
}

// Parameters an authorization request may carry, each at most once: a repeated
// parameter is how two parsers end up disagreeing about a request.
var authParams = []string{"response_type", "client_id", "redirect_uri", "code_challenge",
	"code_challenge_method", "state", "scope", "resource"}

// ParseAuthorize validates an authorization request from the query string
// (the first visit) or the form (the person's answer, which repeats it in
// hidden fields).
func (s *Server) ParseAuthorize(ctx context.Context, v url.Values) (AuthRequest, *AuthError) {
	fail := func(code, desc string) (AuthRequest, *AuthError) {
		return AuthRequest{}, &AuthError{Code: code, Description: desc}
	}
	for _, k := range authParams {
		if len(v[k]) > 1 {
			return fail("invalid_request", "The parameter "+k+" was given more than once.")
		}
	}
	c, err := s.client(ctx, v.Get("client_id"))
	if errors.Is(err, ErrNotFound) {
		return fail("invalid_request", "This application is not registered. Reconnect it from the assistant.")
	}
	if err != nil {
		return fail("server_error", "Something went wrong.")
	}
	uri := v.Get("redirect_uri")
	if !c.hasRedirect(uri) {
		return fail("invalid_request", "The application asked to be sent somewhere it did not register.")
	}
	req := AuthRequest{Client: c, RedirectURI: uri, State: v.Get("state")}
	redirectable := func(code, desc string) (AuthRequest, *AuthError) {
		return AuthRequest{}, &AuthError{Code: code, Description: desc, Redirect: true, req: req}
	}
	if len(req.State) > 500 {
		return redirectable("invalid_request", "The state parameter is too long.")
	}
	if v.Get("response_type") != "code" {
		return redirectable("unsupported_response_type", "Only response_type=code is supported.")
	}
	if v.Get("code_challenge_method") != "S256" || !validChallenge(v.Get("code_challenge")) {
		return redirectable("invalid_request", "PKCE with code_challenge_method=S256 is required.")
	}
	req.CodeChallenge = v.Get("code_challenge")
	req.Scope = Scope
	if sc := strings.Fields(v.Get("scope")); len(sc) > 0 {
		for _, one := range sc {
			if one != Scope {
				return redirectable("invalid_scope", "The only scope is "+Scope+".")
			}
		}
	}
	if res := v.Get("resource"); res != "" && res != s.Resource() {
		return redirectable("invalid_target", "This server only issues tokens for "+s.Resource()+".")
	}
	return req, nil
}

// validChallenge is a base64url SHA-256: 43 characters.
func validChallenge(c string) bool {
	if len(c) != 43 {
		return false
	}
	for _, r := range c {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// Approve records the person's consent and returns where to send them: the
// client's redirect URI with a one-use code.
func (s *Server) Approve(ctx context.Context, req AuthRequest, userID int64) (string, error) {
	code, err := random()
	if err != nil {
		return "", err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO oauth_codes (code_hash, client_id, user_id, code_challenge,
		redirect_uri, scope, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		hash(code), req.Client.ID, userID, req.CodeChallenge, req.RedirectURI, req.Scope,
		stamp(s.now().Add(CodeTTL)))
	if err != nil {
		return "", err
	}
	return req.respond(url.Values{"code": {code}}), nil
}

// --- Tokens ----------------------------------------------------------------

// tokenResponse is RFC 6749's success body.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	RefreshToken string `json:"refresh_token"`
	Scope        string `json:"scope"`
}

// oauthError is RFC 6749's error body, with the status to send it at.
type oauthError struct {
	status      int
	Code        string `json:"error"`
	Description string `json:"error_description,omitempty"`
}

func (e *oauthError) Error() string { return e.Code }

func badRequest(code, desc string) *oauthError {
	return &oauthError{status: http.StatusBadRequest, Code: code, Description: desc}
}

var errInvalidGrant = badRequest("invalid_grant", "The code or refresh token is invalid, expired or already used.")

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	// RFC 6749 5.1: a token response must not be cached.
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, e *oauthError) { writeJSON(w, e.status, e) }

// postForm parses a form-encoded POST body only: a secret in the query string
// ends up in logs.
func postForm(w http.ResponseWriter, r *http.Request) (url.Values, *oauthError) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		return nil, badRequest("invalid_request", "The request body is not a valid form.")
	}
	for k, v := range r.PostForm {
		if len(v) > 1 {
			return nil, badRequest("invalid_request", "The parameter "+k+" was given more than once.")
		}
	}
	return r.PostForm, nil
}

func (s *Server) limited(w http.ResponseWriter, r *http.Request, l *middleware.Limiter) bool {
	if l.AllowIP(middleware.ClientIP(r, s.cfg.TrustedProxies)) {
		return false
	}
	w.Header().Set("Retry-After", "5")
	writeErr(w, &oauthError{status: http.StatusTooManyRequests, Code: "temporarily_unavailable",
		Description: "Too many requests. Try again in a few seconds."})
	return true
}

// Token is the token endpoint (POST /oauth/token).
func (s *Server) Token(w http.ResponseWriter, r *http.Request) {
	if s.limited(w, r, s.tokenLimit) {
		return
	}
	f, perr := postForm(w, r)
	if perr != nil {
		writeErr(w, perr)
		return
	}
	if res := f.Get("resource"); res != "" && res != s.Resource() {
		writeErr(w, badRequest("invalid_target", "This server only issues tokens for "+s.Resource()+"."))
		return
	}
	var resp tokenResponse
	var err error
	switch f.Get("grant_type") {
	case "authorization_code":
		resp, err = s.exchangeCode(r.Context(), f)
	case "refresh_token":
		resp, err = s.refresh(r.Context(), f)
	default:
		err = badRequest("unsupported_grant_type", "Use authorization_code or refresh_token.")
	}
	var oe *oauthError
	switch {
	case errors.As(err, &oe):
		writeErr(w, oe)
	case err != nil:
		slog.Error("oauth token", "err", err)
		writeErr(w, &oauthError{status: http.StatusInternalServerError, Code: "server_error"})
	default:
		writeJSON(w, http.StatusOK, resp)
	}
}

// verifierOK checks a PKCE verifier against the stored S256 challenge.
func verifierOK(verifier, challenge string) bool {
	if len(verifier) < 43 || len(verifier) > 128 {
		return false
	}
	for _, r := range verifier {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || strings.ContainsRune("-._~", r)) {
			return false
		}
	}
	sum := sha256.Sum256([]byte(verifier))
	got := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(got), []byte(challenge)) == 1
}

func (s *Server) exchangeCode(ctx context.Context, f url.Values) (tokenResponse, error) {
	code := f.Get("code")
	if code == "" || f.Get("client_id") == "" || f.Get("code_verifier") == "" || f.Get("redirect_uri") == "" {
		return tokenResponse{}, badRequest("invalid_request", "code, client_id, redirect_uri and code_verifier are required.")
	}
	var c struct {
		clientID, challenge, redirect, scope, expires string
		userID                                        int64
	}
	var claimed bool
	// The claim commits whatever follows: a code is spent by being presented,
	// so a guesser gets one try per code, and a replay is noticed.
	err := s.tx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `UPDATE oauth_codes SET used_at = ? WHERE code_hash = ? AND used_at IS NULL
			RETURNING client_id, user_id, code_challenge, redirect_uri, scope, expires_at`,
			stamp(s.now()), hash(code)).Scan(&c.clientID, &c.userID, &c.challenge, &c.redirect, &c.scope, &c.expires)
		if errors.Is(err, sql.ErrNoRows) {
			// Used before (or never issued): whoever got tokens from the first
			// use may be the thief, so end that grant.
			_, err = tx.ExecContext(ctx, `UPDATE oauth_grants SET revoked_at = ? WHERE revoked_at IS NULL AND id =
				(SELECT grant_id FROM oauth_codes WHERE code_hash = ?)`, stamp(s.now()), hash(code))
			return err
		}
		claimed = err == nil
		return err
	})
	if err != nil {
		return tokenResponse{}, err
	}
	if !claimed {
		return tokenResponse{}, errInvalidGrant
	}
	switch {
	case c.expires <= stamp(s.now()),
		c.clientID != f.Get("client_id"),
		c.redirect != f.Get("redirect_uri"),
		!verifierOK(f.Get("code_verifier"), c.challenge):
		return tokenResponse{}, errInvalidGrant
	}

	var out tokenResponse
	err = s.tx(ctx, func(tx *sql.Tx) error {
		var disabled bool
		if err := tx.QueryRowContext(ctx, `SELECT disabled FROM users WHERE id = ?`, c.userID).Scan(&disabled); err != nil {
			return err
		}
		if disabled {
			return errInvalidGrant
		}
		gid, err := random()
		if err != nil {
			return err
		}
		now := s.now()
		if _, err := tx.ExecContext(ctx, `INSERT INTO oauth_grants (id, client_id, user_id, scope, created_at)
			VALUES (?, ?, ?, ?, ?)`, gid, c.clientID, c.userID, c.scope, stamp(now)); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE oauth_codes SET grant_id = ? WHERE code_hash = ?`, gid, hash(code)); err != nil {
			return err
		}
		out, err = s.issue(ctx, tx, gid, c.scope, now)
		return err
	})
	return out, err
}

// issue mints an access and refresh token for a grant.
func (s *Server) issue(ctx context.Context, tx *sql.Tx, grantID, scope string, now time.Time) (tokenResponse, error) {
	access, err := random()
	if err != nil {
		return tokenResponse{}, err
	}
	refresh, err := random()
	if err != nil {
		return tokenResponse{}, err
	}
	for _, t := range []struct {
		secret, kind string
		ttl          time.Duration
	}{{access, "access", AccessTTL}, {refresh, "refresh", RefreshTTL}} {
		if _, err := tx.ExecContext(ctx, `INSERT INTO oauth_tokens (token_hash, kind, grant_id, expires_at)
			VALUES (?, ?, ?, ?)`, hash(t.secret), t.kind, grantID, stamp(now.Add(t.ttl))); err != nil {
			return tokenResponse{}, err
		}
	}
	return tokenResponse{AccessToken: access, TokenType: "Bearer", ExpiresIn: int(AccessTTL / time.Second),
		RefreshToken: refresh, Scope: scope}, nil
}

func (s *Server) refresh(ctx context.Context, f url.Values) (tokenResponse, error) {
	token := f.Get("refresh_token")
	if token == "" || f.Get("client_id") == "" {
		return tokenResponse{}, badRequest("invalid_request", "refresh_token and client_id are required.")
	}
	for _, one := range strings.Fields(f.Get("scope")) {
		if one != Scope {
			return tokenResponse{}, badRequest("invalid_scope", "The only scope is "+Scope+".")
		}
	}
	var out tokenResponse
	var refused error
	err := s.tx(ctx, func(tx *sql.Tx) error {
		var (
			grantID, clientID, scope, expires string
			used, revoked                     sql.NullString
			userID                            int64
			disabled                          bool
		)
		err := tx.QueryRowContext(ctx, `SELECT t.grant_id, t.expires_at, t.used_at, g.client_id, g.user_id, g.scope,
			g.revoked_at, u.disabled FROM oauth_tokens t
			JOIN oauth_grants g ON g.id = t.grant_id JOIN users u ON u.id = g.user_id
			WHERE t.token_hash = ? AND t.kind = 'refresh'`, hash(token)).
			Scan(&grantID, &expires, &used, &clientID, &userID, &scope, &revoked, &disabled)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && clientID != f.Get("client_id")) {
			refused = errInvalidGrant
			return nil
		}
		if err != nil {
			return err
		}
		now := s.now()
		switch {
		case revoked.Valid || disabled || expires <= stamp(now):
			refused = errInvalidGrant
			return nil
		case used.Valid:
			// A rotated token again: the client or a thief is replaying it, and
			// from here they cannot be told apart. End the grant; the person
			// reconnects.
			refused = errInvalidGrant
			_, err := tx.ExecContext(ctx, `UPDATE oauth_grants SET revoked_at = ? WHERE id = ?`, stamp(now), grantID)
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE oauth_tokens SET used_at = ? WHERE token_hash = ?`,
			stamp(now), hash(token)); err != nil {
			return err
		}
		// The access tokens it replaces go too; spent refresh tokens stay until
		// they expire, because they are how a replay is recognised.
		if _, err := tx.ExecContext(ctx, `DELETE FROM oauth_tokens WHERE grant_id = ? AND kind = 'access'`, grantID); err != nil {
			return err
		}
		out, err = s.issue(ctx, tx, grantID, scope, now)
		return err
	})
	if err != nil {
		return tokenResponse{}, err
	}
	return out, refused
}

// tx runs f in a transaction, committing unless f returns an error.
func (s *Server) tx(ctx context.Context, f func(tx *sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := f(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// Revoke is the revocation endpoint (RFC 7009, POST /oauth/revoke). The
// token is the credential, so presenting it ends its whole grant, access and
// refresh together. It answers 200 whether or not the token was known.
func (s *Server) Revoke(w http.ResponseWriter, r *http.Request) {
	if s.limited(w, r, s.tokenLimit) {
		return
	}
	f, perr := postForm(w, r)
	if perr != nil {
		writeErr(w, perr)
		return
	}
	token := f.Get("token")
	if token == "" {
		writeErr(w, badRequest("invalid_request", "token is required."))
		return
	}
	_, err := s.db.ExecContext(r.Context(), `UPDATE oauth_grants SET revoked_at = ? WHERE revoked_at IS NULL
		AND id = (SELECT grant_id FROM oauth_tokens WHERE token_hash = ?)
		AND (? = '' OR client_id = ?)`, stamp(s.now()), hash(token), f.Get("client_id"), f.Get("client_id"))
	if err != nil {
		slog.Error("oauth revoke", "err", err)
		writeErr(w, &oauthError{status: http.StatusInternalServerError, Code: "server_error"})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
}

// Verify is the MCP endpoint's token check. Like a session lookup it joins the
// user, so a person disabled in the directory has no working token, whatever
// the tables say. Errors wrap auth.ErrInvalidToken so the middleware answers
// 401 with the metadata pointer rather than 500.
func (s *Server) Verify(ctx context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
	now := s.now()
	var (
		grantID, scope, expires string
		userID                  int64
	)
	err := s.db.QueryRowContext(ctx, `SELECT g.id, g.user_id, g.scope, t.expires_at FROM oauth_tokens t
		JOIN oauth_grants g ON g.id = t.grant_id JOIN users u ON u.id = g.user_id
		WHERE t.token_hash = ? AND t.kind = 'access' AND t.expires_at > ?
		AND g.revoked_at IS NULL AND u.disabled = 0`, hash(token), stamp(now)).
		Scan(&grantID, &userID, &scope, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("%w: unknown, expired or revoked token", auth.ErrInvalidToken)
	}
	if err != nil {
		return nil, err
	}
	exp, err := time.Parse(time.RFC3339, expires)
	if err != nil {
		return nil, err
	}
	// "Last used" to the minute: a write per tool call would put every MCP
	// request in line behind the single SQLite writer for no one's benefit.
	if _, err := s.db.ExecContext(ctx, `UPDATE oauth_grants SET last_used_at = ? WHERE id = ? AND last_used_at < ?`,
		stamp(now), grantID, stamp(now.Add(-time.Minute))); err != nil {
		return nil, err
	}
	return &auth.TokenInfo{UserID: fmt.Sprint(userID), Scopes: strings.Fields(scope), Expiration: exp}, nil
}

// --- Connected apps --------------------------------------------------------

// Grant is one connected application, as the person sees it.
type Grant struct {
	ID         string
	ClientName string
	CreatedAt  string
	LastUsedAt string // empty until the first call
}

// Grants lists a person's active grants: not revoked, with a refresh token
// that has not expired or been spent. Newest first.
func (s *Server) Grants(ctx context.Context, userID int64) ([]Grant, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT g.id, c.name, g.created_at, g.last_used_at
		FROM oauth_grants g JOIN oauth_clients c ON c.id = g.client_id
		WHERE g.user_id = ? AND g.revoked_at IS NULL AND EXISTS (SELECT 1 FROM oauth_tokens t
			WHERE t.grant_id = g.id AND t.kind = 'refresh' AND t.used_at IS NULL AND t.expires_at > ?)
		ORDER BY g.created_at DESC, g.id`, userID, stamp(s.now()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Grant
	for rows.Next() {
		var g Grant
		if err := rows.Scan(&g.ID, &g.ClientName, &g.CreatedAt, &g.LastUsedAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// RevokeGrant ends one of the person's own grants. Someone else's is
// ErrNotFound, as with requests: its existence is not for them to learn.
func (s *Server) RevokeGrant(ctx context.Context, userID int64, id string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE oauth_grants SET revoked_at = ? WHERE id = ? AND user_id = ? AND revoked_at IS NULL`,
		stamp(s.now()), id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Sweep is housekeeping: expired codes and tokens, grants that have nothing
// left to refresh, and clients that registered and never completed a sign-in.
// Everything it removes is already refused on use.
func (s *Server) Sweep(ctx context.Context) (int64, error) {
	now := stamp(s.now())
	var total int64
	for _, q := range []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM oauth_codes WHERE expires_at <= ?`, []any{now}},
		{`DELETE FROM oauth_tokens WHERE expires_at <= ?`, []any{now}},
		// Revoked grants are kept a week, so the Connected apps page and any
		// support question can still see what happened; their tokens go with them.
		{`DELETE FROM oauth_grants WHERE revoked_at IS NOT NULL AND revoked_at <= ?`,
			[]any{stamp(s.now().Add(-7 * 24 * time.Hour))}},
		// A grant with no unexpired token left is dead whether or not it was revoked.
		{`DELETE FROM oauth_grants WHERE id NOT IN (SELECT grant_id FROM oauth_tokens)`, nil},
		{`DELETE FROM oauth_clients WHERE created_at <= ? AND id NOT IN (SELECT client_id FROM oauth_grants)
			AND id NOT IN (SELECT client_id FROM oauth_codes)`, []any{stamp(s.now().Add(-24 * time.Hour))}},
	} {
		res, err := s.db.ExecContext(ctx, q.sql, q.args...)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, nil
}
