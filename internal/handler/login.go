package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/17xande-dev/leave/internal/directory"
	"github.com/17xande-dev/leave/internal/middleware"
)

// sessionCookie is always SameSite=Lax and HttpOnly. There is no setting to
// loosen it: the app is never embedded cross-site, and a None cookie would let
// any site make a signed-in browser send it.
const sessionCookie = "leave_session"

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
	if !h.loginByIP.AllowIP(middleware.ClientIP(r, h.cfg.TrustedProxies)) || !h.loginByUser.Allow(username) {
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
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/",
		HttpOnly: true, Secure: h.cfg.CookieSecure, SameSite: http.SameSiteLaxMode,
		// No Max-Age: a browser session cookie, with the real expiry held
		// server-side where it slides with use.
	})
	slog.Info("signed in", "user", u.Username)
	http.Redirect(w, r, data.Next, http.StatusSeeOther)
}

func (h *Handler) Logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		if err := h.sessions.Delete(r.Context(), c.Value); err != nil {
			h.serverError(w, r, err)
			return
		}
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: h.cfg.CookieSecure, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "/login?notice=signed-out", http.StatusSeeOther)
}

// safePath keeps a redirect on this site. "//evil.example" and "/\evil.example"
// are both read by browsers as another host, so a leading slash alone is not
// enough.
func safePath(p string) string {
	// Browsers strip tab and newline from a URL before reading it, so
	// "/<TAB>/evil.example" is "//evil.example" to them, though it looks fine here.
	for i := 0; i < len(p); i++ {
		if p[i] < 0x20 || p[i] == 0x7f {
			return "/"
		}
	}
	if p == "" || !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.HasPrefix(p, `/\`) {
		return "/"
	}
	return p
}

// safeNext is safePath for after sign-in, which also refuses the sign-in page
// itself so nobody is bounced straight back to it.
func safeNext(next string) string {
	if p := safePath(next); !strings.HasPrefix(p, "/login") {
		return p
	}
	return "/"
}
