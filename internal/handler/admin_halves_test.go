package handler

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestAdminHalfDaysForm(t *testing.T) {
	hs := newHarness(t)
	staff, hr := hs.signIn("staff1"), hs.signIn("hr")
	res, body := hs.post(staff, "/leave", url.Values{"type": {hs.typeID("annual")}, "start": {"2026-10-12"}, "end": {"2026-10-14"}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("apply: %d %s", res.StatusCode, body)
	}
	page, _, _ := strings.Cut(res.Header.Get("Location"), "?")
	id := strings.TrimPrefix(page, "/leave/")
	action := fmt.Sprintf("/admin/requests/%s/halves", id)
	_, body = hs.get(staff, page)
	if strings.Contains(body, action) {
		t.Fatal("owner sees admin form")
	}
	_, body = hs.get(hr, page)
	if !strings.Contains(body, action) || !strings.Contains(body, `name="half_start"`) {
		t.Fatal("admin form missing with setting off")
	}
	for _, values := range []url.Values{{"half_start": {"0"}}, {"half_end": {"yes"}}, {"half_start": {""}}, {"half_start": {"1", "1"}}} {
		res, body = hs.post(hr, action, values)
		if res.StatusCode != http.StatusBadRequest {
			t.Fatalf("bad checkbox: %d %s", res.StatusCode, body)
		}
	}
	res, body = hs.post(hr, action, url.Values{"half_end": {"1"}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("save: %d %s", res.StatusCode, body)
	}
	_, body = hs.get(hr, res.Header.Get("Location"))
	for _, want := range []string{"2.5 working days", "Half days set by Hannah HR on", "Half days saved."} {
		if !strings.Contains(body, want) {
			t.Fatalf("page lacks %q: %s", want, body)
		}
	}
	res, body = hs.post(hr, action, url.Values{"half_end": {"1"}})
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "The half days are unchanged.") {
		t.Fatalf("no-op: %d %s", res.StatusCode, body)
	}
	res, body = hs.post(hr, action, nil)
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("clear: %d %s", res.StatusCode, body)
	}
	_, body = hs.get(hr, page)
	if !strings.Contains(body, "3 working days") {
		t.Fatalf("clear page: %s", body)
	}
	// Single-day half_end is a domain error visible outside any hidden block.
	res, _ = hs.post(staff, "/leave", url.Values{"type": {hs.typeID("annual")}, "start": {"2026-10-20"}, "end": {"2026-10-20"}})
	p, _, _ := strings.Cut(res.Header.Get("Location"), "?")
	res, body = hs.post(hr, "/admin/requests/"+strings.TrimPrefix(p, "/leave/")+"/halves", url.Values{"half_end": {"1"}})
	if res.StatusCode != 422 || !strings.Contains(body, `<p class="error">A one-day request has only a first day:`) {
		t.Fatalf("inline field error: %d %s", res.StatusCode, body)
	}
}
