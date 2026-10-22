// Package middleware holds the handlers that wrap every route.
package middleware

import (
	"net/http"
	"strings"
)

// CSP is the one policy the whole app serves. Every page is a whole document
// that loads a stylesheet and htmx from this origin and nothing else -- both are
// embedded in the binary, so there is no CDN to allow -- and no template carries
// a style="…" attribute or an on*= handler, so 'unsafe-inline' is never needed.
//
// connect-src is 'self' because htmx swaps are fetch() requests: script-src covers
// loading htmx, not letting it fetch, and without connect-src an hx-get renders
// fine, passes every handler test and is refused by the browser.
const CSP = "default-src 'none'; style-src 'self'; script-src 'self'; " +
	"connect-src 'self'; img-src 'self'; form-action 'self'; " +
	"base-uri 'none'; frame-ancestors 'none'"

// SecurityHeaders sets the headers every response carries, including 404s and
// the CSRF failure page, which is why it wraps the mux rather than living in it.
//
// No HSTS: that is the TLS-terminating proxy's decision, and sending it from a
// plain-http dev server pins a rule that breaks the next project on that port.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", CSP)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		// A private staff tool: nothing here belongs in a search index.
		h.Set("X-Robots-Tag", "noindex, nofollow")
		next.ServeHTTP(w, r)
	})
}

// CSPWithFormAction is CSP with one more place a form may send the browser.
//
// Browsers check form-action against the redirects that follow a form post,
// not just its target: the consent form posts to this site, which answers with
// a redirect to the AI assistant's own redirect URI, and under plain
// form-action 'self' that last hop is blocked and the person is left looking at
// the consent page. The consent page alone adds its client's registered redirect
// origin, which is validated at registration.
func CSPWithFormAction(source string) string {
	return strings.Replace(CSP, "form-action 'self'", "form-action 'self' "+source, 1)
}
