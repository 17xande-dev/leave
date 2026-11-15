package handler

import (
	"context"
	"html"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestAdminSetSex(t *testing.T) {
	hs := newHarness(t)
	hr := hs.signIn("hr")
	id := hs.staffID("staff1")
	path := "/admin/users/" + itoa(id) + "/sex"
	sex := func() string {
		u, err := hs.us.ByID(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		return u.Sex
	}
	for _, v := range []string{"m", "f", ""} {
		res, _ := hs.post(hr, path, url.Values{"sex": {v}})
		if res.StatusCode != http.StatusSeeOther || sex() != v {
			t.Fatalf("sex=%q: status %d, stored %q", v, res.StatusCode, sex())
		}
	}
	hs.post(hr, path, url.Values{"sex": {"m"}})
	for name, form := range map[string]url.Values{"bad value": {"sex": {"x"}}, "wrong case": {"sex": {"M"}}, "missing": {}} {
		if res, _ := hs.post(hr, path, form); res.StatusCode != http.StatusBadRequest || sex() != "m" {
			t.Errorf("%s: status %d, stored %q", name, res.StatusCode, sex())
		}
	}
	if res, _ := hs.post(hr, "/admin/users/99999/sex", url.Values{"sex": {"f"}}); res.StatusCode != http.StatusNotFound {
		t.Errorf("unknown person: %d", res.StatusCode)
	}
	if res, _ := hs.post(hs.signIn("staff1"), path, url.Values{"sex": {"f"}}); res.StatusCode != http.StatusForbidden {
		t.Errorf("non-admin: %d", res.StatusCode)
	}
	if _, body := hs.get(hr, "/admin/users/"+itoa(id)); !strings.Contains(body, `<option value="m" selected>Male</option>`) {
		t.Errorf("the admin page does not show the stored value:\n%s", body)
	}
}

// Maternity is not offered to a man on the apply form or the admin's record
// form, and is offered to everyone else.
func TestMaternityPickerHiddenFromMaleStaff(t *testing.T) {
	hs := newHarness(t)
	hr := hs.signIn("hr")
	opt := `<option value="` + hs.anyTypeID("maternity") + `"`
	annual := `<option value="` + hs.anyTypeID("annual") + `"`
	staff := hs.signIn("staff1")
	page := "/admin/users/" + itoa(hs.staffID("staff1"))

	check := func(label string, wantMaternity bool) {
		t.Helper()
		_, apply := hs.get(staff, "/leave/new")
		_, record := hs.get(hr, page)
		for form, body := range map[string]string{"apply": apply, "record": record} {
			if got := strings.Contains(body, opt); got != wantMaternity {
				t.Errorf("%s, %s form: maternity offered = %v, want %v", label, form, got, wantMaternity)
			}
			if !strings.Contains(body, annual) {
				t.Errorf("%s, %s form: annual missing", label, form)
			}
		}
	}
	check("unset", true)
	hs.post(hr, page+"/sex", url.Values{"sex": {"f"}})
	check("female", true)
	hs.post(hr, page+"/sex", url.Values{"sex": {"m"}})
	check("male", false)
	// Someone else is unaffected.
	if _, body := hs.get(hs.signIn("staff2"), "/leave/new"); !strings.Contains(body, opt) {
		t.Error("maternity hidden from a person who is not marked male")
	}
}

// A maternity request made while the person's sex was unset is not approved
// once they are marked male: the single decide (from the request page and from
// the queue row) and the bulk approve each refuse it, say why, and leave it
// pending.
func TestApprovingMaternityForSinceMarkedMaleIsRefused(t *testing.T) {
	hs := newHarness(t)
	hr, head := hs.signIn("hr"), hs.signIn("headops")
	apply := func() (string, int64) {
		p := hs.apply(hs.signIn("staff1"), "maternity", "2026-10-12", "2026-10-16")
		return p, mustAtoi(t, idOf(p))
	}
	p, id := apply()
	hs.post(hr, "/admin/users/"+itoa(hs.staffID("staff1"))+"/sex", url.Values{"sex": {"m"}})
	r, _ := hs.svc.Get(context.Background(), id)
	staff := mustUser(t, hs, "staff1")
	want := html.EscapeString("Maternity leave is not available to " + staff.Name + ": they are recorded as male")
	pending := func(label string) {
		t.Helper()
		if x, _ := hs.svc.Get(context.Background(), id); x.Status != "pending" {
			t.Errorf("%s: approved anyway: %s", label, x.Status)
		}
	}

	// From the request page.
	res, body := hs.postRaw(head, p+"/decide", url.Values{"decision": {"approve"}, "seen": {r.Fingerprint()}})
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, want) {
		t.Errorf("request page: %d, message missing:\n%s", res.StatusCode, body)
	}
	pending("request page")

	// From a queue row: the error opens that row.
	res, body = hs.postRaw(head, p+"/decide", url.Values{"decision": {"approve"}, "from": {"queue"}, "seen": {r.Fingerprint()}})
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, want) {
		t.Errorf("queue row: %d, message missing:\n%s", res.StatusCode, body)
	}
	pending("queue row")

	// Bulk: the refusal is listed against the row, with the message itself.
	res, body = hs.postRaw(head, "/approvals/approve", url.Values{"id": {idOf(p) + ":" + r.Fingerprint()}})
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, want) || !strings.Contains(body, staff.Name+": ") {
		t.Errorf("bulk: %d, message missing:\n%s", res.StatusCode, body)
	}
	pending("bulk")

	// Declining is still possible.
	res, _ = hs.postRaw(head, p+"/decide", url.Values{"decision": {"decline"}, "note": {"policy"}, "seen": {r.Fingerprint()}})
	if res.StatusCode != http.StatusSeeOther {
		t.Errorf("decline: %d", res.StatusCode)
	}
}

// The staff balance stays hidden regardless of sex. The admin still sees it
// for a person who is offered maternity leave.
func TestMaternityBalanceHiddenFromMaleStaff(t *testing.T) {
	hs := newHarness(t)
	hr := hs.signIn("hr")
	staff := hs.signIn("staff1")
	page := "/admin/users/" + itoa(hs.staffID("staff1"))
	const name = "Maternity leave"

	check := func(label string, want bool) {
		t.Helper()
		_, home := hs.get(staff, "/")
		_, admin := hs.get(hr, page)
		for where, body := range map[string]string{"home": home, "admin person": admin} {
			if !strings.Contains(body, "Annual leave") {
				t.Errorf("%s, %s page: annual missing", label, where)
			}
			wantHere := want && where == "admin person"
			if got := strings.Contains(body, name); got != wantHere {
				t.Errorf("%s, %s page: maternity shown = %v, want %v", label, where, got, wantHere)
			}
		}
	}
	check("unset", true)
	hs.post(hr, page+"/sex", url.Values{"sex": {"m"}})
	check("male", false)
	hs.post(hr, page+"/sex", url.Values{"sex": {"f"}})
	check("female", true)
}
