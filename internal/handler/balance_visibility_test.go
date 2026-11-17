package handler

import (
	"context"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/17xande-dev/leave/internal/leave"
)

func TestStaffBalanceVisibility(t *testing.T) {
	hs := newHarness(t)
	staff, hr := hs.signIn("staff1"), hs.signIn("hr")
	_, home := hs.get(staff, "/")
	for _, name := range []string{"Annual leave", "Sick leave", "Family responsibility leave"} {
		if !strings.Contains(home, "<td>"+name+"</td>") {
			t.Errorf("home lacks visible balance %s", name)
		}
	}
	_, picker := hs.get(staff, "/leave/new")
	_, admin := hs.get(hr, "/admin/users/"+itoa(hs.staffID("staff1")))
	for _, name := range []string{"Maternity leave", "Parental leave", "Compassionate leave", "Unpaid leave"} {
		if strings.Contains(home, name) {
			t.Errorf("home exposes hidden balance %s", name)
		}
		applicable := name == "Maternity leave" || name == "Parental leave"
		if strings.Contains(picker, ">"+name+"</option>") != applicable || strings.Contains(picker, ">"+name+" (") {
			t.Errorf("apply picker eligibility/bare label wrong for %s", name)
		}
		mark := ""
		if !applicable {
			mark = " <small>admin only</small>"
		}
		if !strings.Contains(admin, "<td>"+name+" <small>not shown to them</small>"+mark+"</td>") {
			t.Errorf("admin lacks marked balance %s", name)
		}
	}
	if !strings.Contains(picker, ">Annual leave (15 left this year)</option>") {
		t.Error("annual picker lost its figure")
	}
	if got := strings.Count(admin, "<small>not shown to them</small>"); got != 4 {
		t.Errorf("admin marked %d balances, want 4", got)
	}
	// Admins still choose every hidden type when recording leave; bounded
	// types also stay in the adjustment picker (unlimited types never were).
	for _, code := range []string{"maternity", "parental", "compassionate", "unpaid"} {
		id := hs.typeID(code)
		if !regexp.MustCompile(`<option value="` + id + `"[^>]*>`).MatchString(admin) {
			t.Errorf("record picker lacks %s", code)
		}
		if (code == "maternity" || code == "parental") && !strings.Contains(admin, `<option value="`+id+`:2026-01-01"`) {
			t.Errorf("adjustment picker lacks %s", code)
		}
	}
}

func TestHiddenBalancePreviewAndRefusal(t *testing.T) {
	hs := newHarness(t)
	staff := hs.signIn("staff1")
	_, parental := hs.get(staff, "/leave/quote?type="+hs.typeID("parental")+"&start=2026-10-12&end=2026-10-13")
	if !strings.Contains(parental, "2 days") || strings.Contains(parental, "afterwards") || strings.Contains(parental, " left in ") {
		t.Errorf("hidden preview must keep days without balance:\n%s", parental)
	}
	_, annual := hs.get(staff, "/leave/quote?type="+hs.typeID("annual")+"&start=2026-10-12&end=2026-10-13")
	if !strings.Contains(annual, "2 working days") || !strings.Contains(annual, "13 left in this cycle afterwards") {
		t.Errorf("shown preview lost balance:\n%s", annual)
	}
	_, refused := hs.get(staff, "/leave/quote?type="+hs.typeID("parental")+"&start=2026-10-12&end=2026-10-22")
	if !strings.Contains(refused, "This is 11 days, but you have 10 left for this cycle.") || strings.Contains(refused, "afterwards") {
		t.Errorf("hidden balance refusal lost the reason:\n%s", refused)
	}
	path := hs.apply(staff, "parental", "2026-10-12", "2026-10-13")
	_, edit := hs.get(staff, path+"/edit")
	if !strings.Contains(edit, ">Parental leave</option>") || strings.Contains(edit, "afterwards") {
		t.Errorf("edit must retain hidden picker and preview:\n%s", edit)
	}
}

func TestAdminTypeBalanceVisibilityToggle(t *testing.T) {
	hs := newHarness(t)
	staff, hr := hs.signIn("staff1"), hs.signIn("hr")
	const checked = `name="show_balance" value="1" checked`
	if _, form := hs.get(hr, "/admin/types/new"); !strings.Contains(form, checked) {
		t.Fatal("new types must default to visible")
	}
	id := hs.typeID("parental")
	for _, shown := range []bool{true, false} {
		form := url.Values{"id": {id}, "name": {"Parental leave"}, "allowance": {"10"},
			"cycle_months": {"12"}, "unit": {"calendar"}, "restricted": {"any"},
			"coverage_rule": {"count"}, "backdate_days": {"92"}, "active": {"1"}, "staff_can_apply": {"1"}, "show_balance": {"0"}}
		if shown {
			form.Add("show_balance", "1")
		}
		res, body := hs.post(hr, "/admin/types", form)
		if res.StatusCode != http.StatusSeeOther {
			t.Fatalf("save visibility %v: %d\n%s", shown, res.StatusCode, body)
		}
		typ, err := hs.svc.Type(context.Background(), mustAtoi(t, id))
		if err != nil || typ.ShowBalance != shown {
			t.Fatalf("stored visibility: %+v, %v", typ, err)
		}
		_, body = hs.get(hr, "/admin/types/"+id)
		if strings.Contains(body, checked) != shown {
			t.Errorf("form visibility != %v", shown)
		}
		_, body = hs.get(hr, "/admin/types")
		row := regexp.MustCompile(`(?s)<tr>\s*<td><a href="/admin/types/` + id + `".*?</tr>`).FindString(body)
		if row == "" || strings.Contains(row, "balance hidden from staff") == shown {
			t.Errorf("list badge visibility %v:\n%s", shown, row)
		}
		_, body = hs.get(staff, "/")
		if strings.Contains(body, "<td>Parental leave</td>") != shown {
			t.Errorf("home did not follow saved visibility %v", shown)
		}
	}
}

func TestHomeAnnualSummaryUsesFullBalances(t *testing.T) {
	allowance := leave.WholeDays(15)
	bs := []leave.Balance{{Type: leave.Type{Code: leave.AnnualCode, ShowBalance: false}, Allowance: &allowance}}
	d := newHomeData(bs, nil, time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC))
	if d.Annual == nil || *d.Annual != allowance || len(d.Balances) != 0 {
		t.Fatalf("annual summary must read the full list before filtering: %+v", d)
	}
}
