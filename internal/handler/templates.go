package handler

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

//go:embed templates
var templateFS embed.FS

// Pages are parsed one template set per page: the partials, the page's layout,
// then the page. Every page defines "content", and in one flat set the last
// file parsed would silently win and pages would render as each other.
//
// The layout is chosen by directory rather than by a flag. templates/public/
// renders for someone with no session through the plain layout, which has no
// navigation or sign-out form in scope to render by mistake.
var pageDirs = map[string]string{
	"templates/pages":  "templates/layouts/app.html",
	"templates/public": "templates/layouts/plain.html",
}

func parsePages() (map[string]*template.Template, *template.Template, error) {
	base, err := template.New("root").Funcs(funcs).ParseFS(templateFS, "templates/partials/*.html")
	if err != nil {
		return nil, nil, fmt.Errorf("parse partials: %w", err)
	}
	out := map[string]*template.Template{}
	for dir, layout := range pageDirs {
		entries, err := fs.ReadDir(templateFS, dir)
		if err != nil {
			return nil, nil, fmt.Errorf("read %s: %w", dir, err)
		}
		for _, e := range entries {
			if e.IsDir() || path.Ext(e.Name()) != ".html" {
				continue
			}
			set, err := base.Clone()
			if err != nil {
				return nil, nil, err
			}
			if set, err = set.ParseFS(templateFS, layout, dir+"/"+e.Name()); err != nil {
				return nil, nil, fmt.Errorf("parse %s: %w", e.Name(), err)
			}
			if set.Lookup("content") == nil {
				return nil, nil, fmt.Errorf("page %s defines no \"content\" template", e.Name())
			}
			if _, dup := out[e.Name()]; dup {
				return nil, nil, fmt.Errorf("page name %s is used in two directories", e.Name())
			}
			out[e.Name()] = set
		}
	}
	if len(out) == 0 {
		return nil, nil, fmt.Errorf("no pages found")
	}
	// base is kept for htmx endpoints that return one partial with no layout.
	return out, base, nil
}

// funcs is the template func map. Deliberately small.
var funcs = template.FuncMap{
	"asset": assetURL,
	// date renders a YYYY-MM-DD string as "Mon 6 Oct 2026". Anything that does
	// not parse is shown as it is rather than hidden.
	"date": func(s string) string {
		t, err := time.Parse(time.DateOnly, s)
		if err != nil {
			return s
		}
		return t.Format("Mon 2 Jan 2006")
	},
	"plural": func(n int, one, many string) string {
		if n == 1 || n == -1 {
			return one
		}
		return many
	},
	"lower":  strings.ToLower,
	"status": statusClass,
	// statuslabel is a status as a person reads it: awaiting_hr is "awaiting HR".
	"statuslabel": statusLabel,
	"add":         func(a, b int) int { return a + b },
	// units names what a count of a type's unit is: "3 working days",
	// "1 weekend".
	"units": unitLabel,
	"list":  func(xs ...string) []string { return xs },
}

// view is what every page renders from. Page-specific data goes in Data, so the
// layout can rely on the rest being there.
type view struct {
	Title  string
	Me     *me
	CSRF   string
	Notice string
	Path   string
	Data   any
	// Theme is the person's forced appearance ("light" or "dark"), or empty to
	// follow the system. Next is where the theme form sends them back to.
	Theme string
	Next  string
}

// me is the signed-in person as the layout needs them.
type me struct {
	ID               int64
	Name             string
	Admin            bool
	PendingApprovals int
}

// render executes a page through its layout into a buffer first, so a template
// error is still answerable with a 500 instead of a committed 200 and half a page.
func (h *Handler) render(w http.ResponseWriter, r *http.Request, status int, page string, v view) {
	set, ok := h.pages[page]
	if !ok {
		h.serverError(w, r, fmt.Errorf("no such page %q", page))
		return
	}
	v.CSRF = csrfToken(r)
	v.Path = r.URL.Path
	v.Theme = themeFrom(r)
	v.Next = nextFor(r)
	if v.Me == nil {
		v.Me = h.meFor(r)
	}
	if v.Notice == "" {
		v.Notice = notices[r.URL.Query().Get("notice")]
	}
	var buf bytes.Buffer
	if err := set.ExecuteTemplate(&buf, "layout", v); err != nil {
		h.serverError(w, r, fmt.Errorf("render %s: %w", page, err))
		return
	}
	writeHTML(w, status, &buf)
}

// nextFor is where the theme form sends the person back to. A GET URL is a page
// one can return to. A re-rendered POST (a refused form) is not, but its form
// page is what the browser sent as Referer, when that is this site.
func nextFor(r *http.Request) string {
	if r.Method == http.MethodGet {
		return safePath(r.URL.RequestURI())
	}
	if u, err := url.Parse(r.Referer()); err == nil && u.Host != "" && u.Host == r.Host {
		return safePath(u.RequestURI())
	}
	return "/"
}

// renderPartial executes one partial with no layout, for an htmx swap.
func (h *Handler) renderPartial(w http.ResponseWriter, r *http.Request, name string, data any) {
	var buf bytes.Buffer
	if err := h.partials.ExecuteTemplate(&buf, name, data); err != nil {
		h.serverError(w, r, fmt.Errorf("render partial %s: %w", name, err))
		return
	}
	writeHTML(w, http.StatusOK, &buf)
}

func writeHTML(w http.ResponseWriter, status int, buf *bytes.Buffer) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Every page is someone's leave record; none of it belongs in a shared cache.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	buf.WriteTo(w)
}

func (h *Handler) serverError(w http.ResponseWriter, r *http.Request, err error) {
	slog.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	http.Error(w, "Something went wrong. Please try again, and tell the office if it keeps happening.",
		http.StatusInternalServerError)
}

// notices is the fixed set of messages a redirect can ask a page to show. A
// ?notice= value outside this map shows nothing: query text is never echoed.
var notices = map[string]string{
	"signed-out":         "You have been signed out.",
	"submitted":          "Your leave request has been submitted for approval.",
	"withdrawn":          "Your request has been withdrawn.",
	"cancel-requested":   "Your cancellation request has been sent for approval.",
	"approved":           "Request approved.",
	"declined":           "Request declined.",
	"saved":              "Saved.",
	"reassigned":         "Request reassigned.",
	"holidays-generated": "Public holidays generated.",
	"retried":            "Queued for another attempt.",
	"shutdown-created":   "Shutdown created, and everyone has been sent their pre-filled requests.",
	"drafts-generated":   "Requests created for anyone who did not have them yet.",
	"shutdown-deleted":   "Shutdown deleted, with its unsubmitted requests.",
	"not-deletable":      "Someone has already submitted leave for this shutdown, so it can no longer be deleted.",
	"escalated":          "Approved, and sent on to the next person, because it needs special permission.",
	"negative-requested": "Sent to HR. They need to allow this before you can submit it, and you will be emailed.",
	"negative-granted":   "Permission granted. The request is waiting for them to submit it.",
	"negative-refused":   "Request refused.",
	"coverage-saved":     "Coverage limit saved.",
	"team-saved":         "Team limit saved.",
	"team-default-saved": "Default team limit saved.",
	"app-revoked":        "App disconnected. It can no longer act for you.",
	"not-possible":       "That is no longer possible. The request may already have been decided or withdrawn.",
}

func unitLabel(n int, unit string) string {
	switch unit {
	case "weekend":
		if n == 1 || n == -1 {
			return "weekend"
		}
		return "weekends"
	case "calendar":
		if n == 1 || n == -1 {
			return "day"
		}
		return "days"
	}
	if n == 1 || n == -1 {
		return "working day"
	}
	return "working days"
}
