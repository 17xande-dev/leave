package handler

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/17xande-dev/leave/internal/leave"
)

func TestAdminOnlyTypePages(t *testing.T) {
	hs := newHarness(t)
	staff, hr := hs.signIn("staff1"), hs.signIn("hr")
	_, body := hs.get(staff, "/leave/new")
	for _, code := range []string{"annual", "parental", "maternity", "compassionate", "unpaid"} {
		option := regexp.MustCompile(`<option value="` + hs.typeID(code) + `"[^>]*>`)
		if option.MatchString(body) != (code != "compassionate" && code != "unpaid") {
			t.Errorf("apply picker wrong for %s", code)
		}
	}
	id := hs.typeID("compassionate")
	ty, err := hs.svc.Type(context.Background(), mustAtoi(t, id))
	if err != nil {
		t.Fatal(err)
	}
	res, body := hs.post(staff, "/leave", url.Values{"type": {id}, "start": {"2026-10-12"}, "end": {"2026-10-12"}, "note": {"reason"}})
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, leave.MsgAdminOnlyType(ty)) {
		t.Fatalf("forged compassionate POST: %d %s", res.StatusCode, body)
	}
	ty.StaffCanApply = true
	if _, err := hs.svc.SaveType(context.Background(), ty); err != nil {
		t.Fatal(err)
	}
	path := hs.apply(staff, "compassionate", "2026-10-12", "2026-10-12")
	ty.StaffCanApply = false
	if _, err := hs.svc.SaveType(context.Background(), ty); err != nil {
		t.Fatal(err)
	}
	_, body = hs.get(staff, path+"/edit")
	if !regexp.MustCompile(`<option value="`+id+`"[^>]* selected>Compassionate leave</option>`).MatchString(body) || regexp.MustCompile(`<option value="`+hs.typeID("unpaid")+`"[^>]*>`).MatchString(body) {
		t.Fatalf("grandfathered edit picker:\n%s", body)
	}
	_, body = hs.get(hr, "/admin/users/"+itoa(hs.staffID("staff1")))
	for _, code := range []string{"compassionate", "unpaid"} {
		if !regexp.MustCompile(`<option value="` + hs.typeID(code) + `"[^>]*>`).MatchString(body) {
			t.Errorf("admin record picker lacks %s", code)
		}
	}
}

func TestAdminTypeStaffCanApplyToggle(t *testing.T) {
	hs := newHarness(t)
	staff, hr := hs.signIn("staff1"), hs.signIn("hr")
	const checked = `name="staff_can_apply" value="1" checked`
	if _, body := hs.get(hr, "/admin/types/new"); !strings.Contains(body, checked) {
		t.Fatal("new type must default to self-service")
	}
	id := hs.typeID("compassionate")
	for _, allowed := range []bool{true, false} {
		form := url.Values{"id": {id}, "name": {"Compassionate leave"}, "cycle_months": {"12"}, "unit": {"workdays"}, "restricted": {"any"}, "coverage_rule": {"count"}, "backdate_days": {"92"}, "active": {"1"}}
		if allowed {
			form.Set("staff_can_apply", "1")
		}
		if res, body := hs.post(hr, "/admin/types", form); res.StatusCode != http.StatusSeeOther {
			t.Fatalf("toggle: %d %s", res.StatusCode, body)
		}
		got, err := hs.svc.Type(context.Background(), mustAtoi(t, id))
		if err != nil || got.StaffCanApply != allowed {
			t.Fatalf("flag=%+v %v", got, err)
		}
		_, body := hs.get(hr, "/admin/types/"+id)
		if strings.Contains(body, checked) != allowed {
			t.Errorf("checkbox != %v", allowed)
		}
		_, body = hs.get(hr, "/admin/types")
		row := regexp.MustCompile(`(?s)<tr>\s*<td><a href="/admin/types/` + id + `".*?</tr>`).FindString(body)
		if row == "" || strings.Contains(row, "admin only") == allowed {
			t.Errorf("badge != %v: %s", !allowed, row)
		}
		_, body = hs.get(staff, "/leave/new")
		if regexp.MustCompile(`<option value="`+id+`"[^>]*>`).MatchString(body) != allowed {
			t.Errorf("picker did not follow flag %v", allowed)
		}
	}
}
