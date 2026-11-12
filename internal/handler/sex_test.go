package handler

import (
	"context"
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
