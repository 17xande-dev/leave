package handler

import (
	"net/http"
	"time"
)

// themeCookie holds "light" or "dark". No cookie means follow the system. It is
// a server-side cookie because the CSP rules out the script that would
// otherwise remember the choice in localStorage.
const themeCookie = "theme"

// themeFrom is the forced theme, or "" for the system's. A value that is
// neither of the two is ignored rather than echoed into the page.
func themeFrom(r *http.Request) string {
	c, err := r.Cookie(themeCookie)
	if err != nil || (c.Value != "light" && c.Value != "dark") {
		return ""
	}
	return c.Value
}

// SetTheme is public, because the sign-in page has the toggle too, but it is
// still under the CSRF layer like every other form. The toggle posts "light" or
// "dark" to force a theme, or "system" when flipping lands back on what the
// system already prefers, so the cookie is dropped and the page follows it again.
func (h *Handler) SetTheme(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request.", http.StatusBadRequest)
		return
	}
	c := &http.Cookie{Name: themeCookie, Path: "/", HttpOnly: true,
		Secure: h.cfg.CookieSecure, SameSite: http.SameSiteLaxMode}
	switch v := r.PostForm.Get("theme"); v {
	case "light", "dark":
		c.Value, c.MaxAge = v, int(365*24*time.Hour/time.Second)
	case "system":
		c.MaxAge = -1
	default:
		http.Error(w, "Choose auto, light or dark.", http.StatusBadRequest)
		return
	}
	http.SetCookie(w, c)
	http.Redirect(w, r, safePath(r.PostForm.Get("next")), http.StatusSeeOther)
}
