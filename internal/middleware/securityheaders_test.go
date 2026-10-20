package middleware

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Pins the policy, so a later convenience ("just allow inline styles for this
// one page") has to change this test and be seen doing it.
func TestSecurityHeadersPinned(t *testing.T) {
	h := SecurityHeaders(http.NotFoundHandler())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/nope", nil))

	csp := rec.Header().Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "script-src 'self'", "style-src 'self'",
		"frame-ancestors 'none'", "base-uri 'none'", "form-action 'self'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP %q lacks %q", csp, want)
		}
	}
	if strings.Contains(csp, "unsafe") {
		t.Errorf("CSP %q allows something unsafe", csp)
	}
	// A 404 carries the headers too: they wrap the mux, not a route.
	if rec.Code != http.StatusNotFound || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("404 missing headers: %d %v", rec.Code, rec.Header())
	}
}

func TestCSPWithFormAction(t *testing.T) {
	got := CSPWithFormAction("http://localhost:3142")
	if !strings.Contains(got, "form-action 'self' http://localhost:3142;") {
		t.Errorf("CSP %q does not add the source to form-action", got)
	}
	if strings.Replace(got, " http://localhost:3142", "", 1) != CSP {
		t.Errorf("CSP changed in more than form-action: %q", got)
	}
}
