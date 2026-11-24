package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/17xande-dev/leave/internal/directory"
	"github.com/17xande-dev/ratelimit"
	"github.com/17xande-dev/websec"
)

// sessionCookieName is the bare name; cookie.New derives the wire name
// (__Host-leave_session over https). The kit fixes SameSite=Lax and HttpOnly:
// the app is never embedded cross-site, and a None cookie would let any site
// make a signed-in browser send it.
const sessionCookieName = "leave_session"

type loginData struct {
	Username string
	Next     string
	Error    string
}

func (h *Handler) LoginForm(w http.ResponseWriter, r *http.Request) {
	h.render(w, r, http.StatusOK, "login.html", view{Title: "Sign in",
		Data: loginData{Next: safeNext(r.URL.Query().Get("next"))}})
}

// maxUsernameBytes bounds what the per-username limiter will key on. Real
// account names are far shorter; the limiter stores the key before the password
// is checked, so an unbounded one is memory an attacker chooses.
const maxUsernameBytes = 256

func (h *Handler) Login(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad form.", http.StatusBadRequest)
		return
	}
	username := directory.NormalizeUsername(r.PostFormValue("username"))
	password := r.PostFormValue("password")
	data := loginData{Username: r.PostFormValue("username"), Next: safeNext(r.PostFormValue("next"))}

	fail := func(status int, msg string) {
		data.Error = msg
		h.render(w, r, status, "login.html", view{Title: "Sign in", Data: data})
	}

	// Before Allow: a name nobody has is not worth a bucket, and costs the
	// directory nothing, so it is refused without being counted.
	if len(username) > maxUsernameBytes {
		data.Username = ""
		fail(http.StatusUnauthorized, "That username and password did not match. Use your usual Windows sign-in.")
		return
	}
	if !h.loginByIP.Allow(ratelimit.IPKey(h.cfg.ClientIP.Addr(r))) || !h.loginByUser.Allow(username) {
		fail(http.StatusTooManyRequests, "Too many sign-in attempts. Please wait a few minutes and try again.")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	du, err := h.directory.Authenticate(ctx, username, password)
	if errors.Is(err, directory.ErrInvalidCredentials) {
		// One message for every cause; see ErrInvalidCredentials.
		fail(http.StatusUnauthorized, "That username and password did not match. Use your usual Windows sign-in.")
		return
	}
	if err != nil {
		slog.Error("sign-in: directory unavailable", "err", err)
		fail(http.StatusServiceUnavailable, "Sign-in is unavailable right now. Please try again shortly.")
		return
	}

	u, err := h.users.Upsert(r.Context(), du)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	token, err := h.sessions.Create(r.Context(), u.ID)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	// A token issued before sign-in (a page opened earlier, or one planted by
	// a sibling site) must not survive into the signed-in session.
	if _, err := h.cfg.CSRF.Renew(w, r); err != nil {
		h.serverError(w, r, err)
		return
	}
	// Browser-session cookie: the kit ignores the expiry argument.
	h.cookie.Set(w, token, time.Time{})
	slog.Info("signed in", "user", u.Username)
	http.Redirect(w, r, data.Next, http.StatusSeeOther)
}

func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	if token, ok := h.cookie.Value(r); ok {
		if err := h.sessions.Delete(r.Context(), token); err != nil {
			h.serverError(w, r, err)
			return
		}
	}
	h.cookie.Clear(w)
	// The next person at this browser starts with a fresh token too.
	if _, err := h.cfg.CSRF.Renew(w, r); err != nil {
		h.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/login?notice=signed-out", http.StatusSeeOther)
}

// pathOpts and nextOpts are the redirect policies. Both fall back to "/", the
// home page, which needs no sign-in decision of its own. The rules themselves
// (and the browser behaviour behind them) live in websec.SafeRedirect.
var (
	// pathOpts has no Prefix and no Deny: any page of this app is a legal
	// target for the theme form's "back to where you were" redirect.
	pathOpts = websec.RedirectOptions{Fallback: "/"}
	// nextOpts is pathOpts plus the sign-in page itself, so nobody is bounced
	// straight back to it. Deny matches the decoded path by plain prefix, which
	// also covers "/login?next=..." and "/%6cogin".
	nextOpts = websec.RedirectOptions{Deny: []string{"/login"}, Fallback: "/"}
)

// safePath keeps a redirect on this site.
func safePath(p string) string { return websec.SafeRedirect(p, pathOpts) }

// safeNext is safePath for after sign-in, which also refuses the sign-in page.
func safeNext(next string) string { return websec.SafeRedirect(next, nextOpts) }
