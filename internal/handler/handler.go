// Package handler is the HTTP side: routing, sign-in, and rendering pages.
package handler

import (
	"context"
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"time"

	"github.com/17xande-dev/ratelimit"
	"github.com/17xande-dev/ratelimit/clientip"
	"github.com/17xande-dev/websec/cookie"
	"github.com/17xande-dev/websec/csrf"

	"github.com/17xande-dev/leave/internal/auth"
	"github.com/17xande-dev/leave/internal/directory"
	"github.com/17xande-dev/leave/internal/leave"
	"github.com/17xande-dev/leave/internal/middleware"
	"github.com/17xande-dev/leave/internal/oauth"
	"github.com/17xande-dev/leave/internal/outbox"
	"github.com/17xande-dev/leave/internal/users"
)

// tokenRenewer is what the handlers need of the CSRF policy: a fresh token when
// the credential changes. *csrf.Protect satisfies it; handler tests, which run
// without the CSRF layer, substitute a stub.
type tokenRenewer interface {
	Renew(w http.ResponseWriter, r *http.Request) (string, error)
}

type Config struct {
	CookieSecure bool
	// CSRF is the policy the app's mux is wrapped in. It is required, and
	// passed in rather than set later: a handler that never got it would sign
	// people in without renewing their token, silently.
	CSRF tokenRenewer
	// ClientIP decides which address a request came from, for the sign-in IP limit. The zero value is the peer.
	ClientIP clientip.Source
}

// Deps are the stores and services the handlers use.
type Deps struct {
	Users     *users.Store
	Sessions  *auth.Sessions
	Directory directory.Directory
	Leave     *leave.Service
	Outbox    *outbox.Queue
	// TeamsEnabled is whether a team source is configured; the Teams page
	// says so when it is not.
	TeamsEnabled bool
	// OAuth and MCP are the AI-assistant endpoints: OAuth serves the token,
	// registration, revocation and metadata routes and the pages' grants; MCP
	// is the bearer-protected tool endpoint.
	OAuth *oauth.Server
	MCP   http.Handler
}

type Handler struct {
	cfg       Config
	cookie    *cookie.Cookie
	users     *users.Store
	sessions  *auth.Sessions
	directory directory.Directory
	leave     *leave.Service
	outbox    *outbox.Queue
	teamsOn   bool
	oauth     *oauth.Server
	mcp       http.Handler

	pages    map[string]*template.Template
	partials *template.Template

	// Sign-in is limited per username and per client IP. Neither key is
	// something an attacker can discard for free, unlike a cookie, and AD's
	// own lockout is behind these limits, so they must be the tighter wall:
	// a guesser should hit them, not lock a colleague out of Windows.
	loginByUser *ratelimit.Limiter
	loginByIP   *ratelimit.Limiter
}

func New(cfg Config, d Deps) (*Handler, error) {
	if cfg.CSRF == nil {
		return nil, errors.New("handler: Config.CSRF is required")
	}
	// A browser-session cookie: the real limits are held server-side, and a
	// directory sign-in should not outlive the browser.
	sessionCookie, err := cookie.New(cookie.Config{Name: sessionCookieName, Insecure: !cfg.CookieSecure, BrowserSession: true})
	if err != nil {
		return nil, err
	}
	a, err := loadAssets()
	if err != nil {
		return nil, err
	}
	assets = a
	pages, partials, err := parsePages()
	if err != nil {
		return nil, err
	}
	return &Handler{
		cfg: cfg, cookie: sessionCookie, users: d.Users, sessions: d.Sessions, directory: d.Directory, leave: d.Leave,
		outbox: d.Outbox, teamsOn: d.TeamsEnabled, oauth: d.OAuth, mcp: d.MCP,
		pages: pages, partials: partials,
		// Five tries per username per 15 minutes -- below AD's usual lockout
		// threshold, so a guesser meets this wall before a colleague is locked
		// out of Windows. The IP ceiling is looser for a shared office NAT.
		loginByUser: middleware.NewLimiter("login-user", 3*time.Minute, 5),
		loginByIP:   middleware.NewLimiter("login-ip", 10*time.Second, 20),
	}, nil
}

// Routes registers every route. Anything not wrapped in h.signedIn or h.admin
// is public, and TestProtectedRoutes pins exactly which lines those are.
func (h *Handler) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	mux.HandleFunc("GET /static/{file}", h.ServeStatic)
	mux.HandleFunc("GET /login", h.LoginForm)
	mux.HandleFunc("POST /login", h.Login)
	mux.HandleFunc("POST /theme", h.SetTheme)

	// The AI-assistant endpoints are public to the session check because they
	// carry no session: /mcp wants a bearer token (checked inside it), the OAuth
	// endpoints are how a client gets one, and the metadata is for discovery.
	// None uses cookies, so none is CSRF-exempt by accident; main lists the
	// exemptions and a test pins them.
	mux.Handle("/mcp", h.mcp)
	mux.HandleFunc("POST /oauth/token", h.oauth.Token)
	mux.HandleFunc("POST /oauth/register", h.oauth.Register)
	mux.HandleFunc("POST /oauth/revoke", h.oauth.Revoke)
	mux.Handle("/.well-known/oauth-authorization-server", h.oauth.AuthorizationServerMetadata())
	mux.Handle("/.well-known/oauth-protected-resource", h.oauth.ResourceMetadata())
	mux.Handle("/.well-known/oauth-protected-resource/mcp", h.oauth.ResourceMetadata())

	for _, rt := range h.protectedRoutes() {
		mux.Handle(rt.pattern, rt.handler)
	}
}

type route struct {
	pattern string
	handler http.Handler
}

// protectedRoutes is the one list of routes behind sign-in, so the test that
// asserts each refuses an anonymous request cannot drift from what is mounted.
func (h *Handler) protectedRoutes() []route {
	u := func(pattern string, f http.HandlerFunc) route { return route{pattern, h.signedIn(f)} }
	routes := []route{
		u("POST /logout", h.Logout),
		u("GET /{$}", h.Home),
		u("GET /leave/new", h.ApplyForm),
		u("GET /leave/quote", h.Quote),
		u("POST /leave", h.Apply),
		u("GET /leave/{id}", h.RequestPage),
		u("GET /leave/{id}/edit", h.EditForm),
		u("POST /leave/{id}/edit", h.Edit),
		u("POST /leave/{id}/withdraw", h.Withdraw),
		u("POST /leave/{id}/submit", h.SubmitDraft),
		u("POST /leave/{id}/cancel", h.RequestCancellation),
		u("POST /leave/{id}/decide", h.Decide),
		u("GET /approvals", h.Approvals),
		u("POST /approvals/approve", h.ApproveMany),
		u("GET /team", h.Team),
		u("GET /delegations", h.Delegations),
		u("POST /delegations", h.CreateDelegation),
		u("POST /delegations/{id}/revoke", h.RevokeDelegation),
		// The consent screen. It stays behind the session and under CSRF: it is
		// a form a person submits in a browser.
		u("GET /oauth/authorize", h.Authorize),
		u("POST /oauth/authorize", h.AuthorizeDecision),
		u("GET /apps", h.Apps),
		u("POST /apps/{id}/revoke", h.RevokeApp),
	}
	return append(routes, h.adminRoutes()...)
}

// csrfToken is the token forms embed, from the CSRF middleware.
func csrfToken(r *http.Request) string { return csrf.Token(r) }

type ctxKey int

const userKey ctxKey = 0

// maxFormBytes bounds a form body read in a handler; the whole app is capped by
// middleware.MaxBody, this keeps each handler safe on its own.
const maxFormBytes = middleware.MaxFormBytes

// signedIn requires a session. A GET without one is sent to the sign-in page
// and returns here afterwards; anything else is refused, since replaying a
// POST after sign-in would act on a form the person never saw submitted.
func (h *Handler) signedIn(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, _ := h.cookie.Value(r)
		u, err := h.sessions.Lookup(r.Context(), token)
		if err != nil {
			if err != auth.ErrNoSession {
				h.serverError(w, r, err)
				return
			}
			if r.Method == http.MethodGet {
				http.Redirect(w, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()), http.StatusSeeOther)
				return
			}
			http.Error(w, "Your session has ended. Please sign in again.", http.StatusUnauthorized)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), userKey, u)))
	})
}

// admin requires a session belonging to a member of the admin group.
func (h *Handler) admin(next http.HandlerFunc) http.Handler {
	return h.signedIn(func(w http.ResponseWriter, r *http.Request) {
		if !currentUser(r).Admin {
			h.render(w, r, http.StatusForbidden, "forbidden.html", view{Title: "Not allowed"})
			return
		}
		next(w, r)
	})
}

// currentUser is the signed-in user. Only valid inside signedIn.
func currentUser(r *http.Request) users.User {
	u, _ := r.Context().Value(userKey).(users.User)
	return u
}

func (h *Handler) meFor(r *http.Request) *me {
	u, ok := r.Context().Value(userKey).(users.User)
	if !ok {
		return nil
	}
	m := &me{ID: u.ID, Name: u.Name, Admin: u.Admin}
	// The badge is a convenience: failing to count must not fail the page.
	m.PendingApprovals, _ = h.leave.PendingCount(r.Context(), u)
	m.ShowAway = h.showAway(r.Context(), u, m.PendingApprovals)
	return m
}

// showAway is whether the "Away" link (the page for naming a stand-in) shows:
// for admins, anyone with something waiting, anyone in a delegation (so one
// with a stand-in, or standing in, can always find it), anyone with reports and
// the final approver. Like the badge it is a convenience, so a failed lookup
// shows nothing rather than failing the page; the page itself is open to all.
func (h *Handler) showAway(ctx context.Context, u users.User, waiting int) bool {
	if u.Admin || waiting > 0 {
		return true
	}
	if in, err := h.leave.InDelegation(ctx, u); err == nil && in {
		return true
	}
	if has, err := h.leave.HasReports(ctx, u); err == nil && has {
		return true
	}
	st, err := h.leave.Settings(ctx)
	return err == nil && st.FinalApproverID != 0 && st.FinalApproverID == u.ID
}
