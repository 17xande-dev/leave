package handler

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"time"

	"github.com/17xande-dev/leave/internal/middleware"
	"github.com/17xande-dev/leave/internal/oauth"
)

// The sign-in half of "connect an AI assistant". The protocol endpoints (token,
// register, revoke, metadata) are in internal/oauth and mounted as public routes
// in Routes; what is here needs a session and a page, so it lives with the pages
// and goes through signedIn like every other one. An anonymous visitor is sent
// to /login and back to the same authorize URL, which is how the consent screen
// reuses the normal sign-in.

type consentField struct{ Name, Value string }

type consentData struct {
	ClientName   string
	RedirectHost string
	Fields       []consentField
	Admin        bool
}

// consentParams are the authorization request's fields, repeated as hidden
// inputs so the answer carries the request it answers. They are all validated
// again when the form comes back; nothing is trusted for having been rendered.
var consentParams = []string{"response_type", "client_id", "redirect_uri", "code_challenge",
	"code_challenge_method", "state", "scope", "resource"}

func (h *Handler) Authorize(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	req, aerr := h.oauth.ParseAuthorize(r.Context(), q)
	if aerr != nil {
		h.authorizeFailed(w, r, aerr)
		return
	}
	d := consentData{ClientName: req.Client.Name, RedirectHost: redirectDisplay(req.RedirectURI),
		Admin: currentUser(r).Admin}
	for _, k := range consentParams {
		if v := q.Get(k); v != "" {
			d.Fields = append(d.Fields, consentField{k, v})
		}
	}
	if src := formActionSource(req.RedirectURI); src != "" {
		w.Header().Set("Content-Security-Policy", middleware.CSPWithFormAction(src))
	}
	h.render(w, r, http.StatusOK, "consent.html", view{Title: "Connect an app", Data: d})
}

// AuthorizeDecision is the person's answer to the consent screen.
func (h *Handler) AuthorizeDecision(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "That form could not be read.", http.StatusBadRequest)
		return
	}
	req, aerr := h.oauth.ParseAuthorize(r.Context(), r.PostForm)
	if aerr != nil {
		h.authorizeFailed(w, r, aerr)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	switch r.PostFormValue("decision") {
	case "allow":
		u := currentUser(r)
		to, err := h.oauth.Approve(r.Context(), req, u.ID)
		if err != nil {
			h.serverError(w, r, err)
			return
		}
		slog.Info("app connected", "user", u.Username, "client", req.Client.Name)
		http.Redirect(w, r, to, http.StatusSeeOther)
	case "deny":
		http.Redirect(w, r, req.DenyURL(), http.StatusSeeOther)
	default:
		http.Error(w, "Choose Allow or Deny.", http.StatusBadRequest)
	}
}

// authorizeFailed answers a request that cannot be shown for consent. If the
// client and its redirect URI checked out, the client is told and the person
// returns to it; otherwise nothing is sent anywhere, and the person reads why.
func (h *Handler) authorizeFailed(w http.ResponseWriter, r *http.Request, e *oauth.AuthError) {
	if e.Redirect {
		http.Redirect(w, r, e.RedirectURL(), http.StatusSeeOther)
		return
	}
	h.render(w, r, http.StatusBadRequest, "consent_error.html", view{Title: "Cannot connect", Data: e.Description})
}

// redirectDisplay is where the person will be sent, short enough to read: the
// host for a web address, the scheme for an app's own.
func redirectDisplay(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "the application"
	}
	if u.Scheme == "http" || u.Scheme == "https" {
		return u.Host
	}
	return u.Scheme + ":// (an app on your computer)"
}

var cspHost = regexp.MustCompile(`^[A-Za-z0-9.\-:\[\]]+$`)

// formActionSource is the CSP source for a redirect URI's origin: the host for
// a web address, the scheme alone for an app's own. Empty when the URI has
// nothing safe to put in a policy, which leaves the stricter default.
func formActionSource(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	if u.Scheme == "http" || u.Scheme == "https" {
		if !cspHost.MatchString(u.Host) {
			return ""
		}
		return u.Scheme + "://" + u.Host
	}
	return u.Scheme + ":"
}

// --- Connected apps ----------------------------------------------------------

type appRow struct {
	ID        string
	Name      string
	Connected string
	LastUsed  string
}

func formatInstant(s string) string {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return s
	}
	return t.UTC().Format("2 Jan 2006 15:04") + " UTC"
}

func (h *Handler) Apps(w http.ResponseWriter, r *http.Request) {
	gs, err := h.oauth.Grants(r.Context(), currentUser(r).ID)
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	rows := make([]appRow, 0, len(gs))
	for _, g := range gs {
		row := appRow{ID: g.ID, Name: g.ClientName, Connected: formatInstant(g.CreatedAt), LastUsed: "Not yet"}
		if g.LastUsedAt != "" {
			row.LastUsed = formatInstant(g.LastUsedAt)
		}
		rows = append(rows, row)
	}
	h.render(w, r, http.StatusOK, "apps.html", view{Title: "Connected apps", Data: rows})
}

func (h *Handler) RevokeApp(w http.ResponseWriter, r *http.Request) {
	err := h.oauth.RevokeGrant(r.Context(), currentUser(r).ID, r.PathValue("id"))
	if errors.Is(err, oauth.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.serverError(w, r, err)
		return
	}
	http.Redirect(w, r, "/apps?notice=app-revoked", http.StatusSeeOther)
}
