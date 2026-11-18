package main

import (
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"testing"
)

// The stand-in forms under the real middleware stack: the handler tests have
// no CSRF layer, so this is where a form that forgot its token, or a revoke
// that does not need one, shows.
func TestDelegationFormsUnderCSRF(t *testing.T) {
	e := newE2E(t)
	b := e.browser()
	b.signIn("headops", "")

	var kids int64
	if err := e.db.QueryRow(`SELECT id FROM users WHERE username = 'headkids'`).Scan(&kids); err != nil {
		t.Fatal(err)
	}
	_, page := b.get("/delegations")
	f := fields(page)
	if f.Get("csrf_token") == "" {
		t.Fatalf("the form has no CSRF token:\n%s", page)
	}
	form := url.Values{"delegate": {fmt.Sprint(kids)}, "start": {"2026-10-05"}, "end": {"2026-10-07"}}

	// Without the token the post is refused, and nothing is saved.
	if res, _ := b.post("/delegations", form); res.StatusCode != http.StatusForbidden {
		t.Fatalf("create without a token: %d", res.StatusCode)
	}
	form.Set("csrf_token", f.Get("csrf_token"))
	res, _ := b.post("/delegations", form)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("create with a token: %d", res.StatusCode)
	}

	// The revoke button is a form with a token of its own.
	_, page = b.get("/delegations")
	m := regexp.MustCompile(`(?s)<form method="post" action="(/delegations/\d+/revoke)">(.*?)</form>`).FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("no revoke form on the page:\n%s", page)
	}
	revoke := fields(m[2])
	if revoke.Get("csrf_token") == "" {
		t.Fatalf("the revoke form has no CSRF token:\n%s", m[2])
	}
	if res, _ := b.post(m[1], url.Values{}); res.StatusCode != http.StatusForbidden {
		t.Fatalf("revoke without a token: %d", res.StatusCode)
	}
	if res, _ := b.post(m[1], revoke); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("revoke with the form's own fields: %d", res.StatusCode)
	}
}
