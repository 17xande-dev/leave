package handler

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Every admin route refuses a signed-in non-admin, each by its own method.
func TestAdminRoutesRefuseStaff(t *testing.T) {
	hs := newHarness(t)
	routes := hs.h.adminRoutes()
	if len(routes) < 10 {
		t.Fatalf("only %d admin routes", len(routes))
	}
	staff := hs.signIn("staff1")
	for _, rt := range routes {
		method, path, _ := strings.Cut(rt.pattern, " ")
		path = regexp.MustCompile(`\{[a-z]+\}`).ReplaceAllString(path, "1")
		req, _ := http.NewRequest(method, hs.srv.URL+path, strings.NewReader(""))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		res, err := staff.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusForbidden {
			t.Errorf("%s as staff: %d, want 403", rt.pattern, res.StatusCode)
		}
	}
}

func TestAdminPagesRender(t *testing.T) {
	hs := newHarness(t)
	hs.apply(hs.signIn("staff1"), "annual", "2026-10-12", "2026-10-12")
	hr := hs.signIn("hr")
	for _, p := range []string{"/admin", "/admin/requests", "/admin/requests?status=pending&from=2026-01-01",
		"/admin/users", "/admin/users/1", "/admin/types", "/admin/types/new", "/admin/types/1",
		"/admin/holidays", "/admin/holidays?year=2027", "/admin/outbox", "/admin/coverage", "/admin/permissions", "/admin/settings"} {
		if code, body := hs.get(hr, p); code != 200 {
			t.Errorf("%s: %d\n%s", p, code, body)
		}
	}
}

// A note starting with = would be a live formula in Excel on an admin's machine.
func TestCSVEscapesFormulas(t *testing.T) {
	hs := newHarness(t)
	staff := hs.signIn("staff1")
	hs.post(staff, "/leave", url.Values{"type": {hs.typeID("annual")}, "start": {"2026-10-12"},
		"end": {"2026-10-12"}, "note": {`=HYPERLINK("http://evil","x")`}})
	res, err := hs.signIn("hr").Get(hs.srv.URL + "/admin/requests.csv")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b := new(strings.Builder)
	io.Copy(b, res.Body)
	if !strings.Contains(b.String(), `'=HYPERLINK`) || res.Header.Get("Content-Type") != "text/csv; charset=utf-8" {
		t.Fatalf("csv:\n%s", b)
	}
}

func TestOverrideRoutesNewRequests(t *testing.T) {
	hs := newHarness(t)
	hr := hs.signIn("hr")
	staff, _ := hs.us.ByUsername(t.Context(), "staff1")
	pastor, _ := hs.us.ByUsername(t.Context(), "pastor")
	res, _ := hs.post(hr, "/admin/users/"+itoa(staff.ID)+"/approver", url.Values{"approver": {itoa(pastor.ID)}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("set override: %d", res.StatusCode)
	}
	path := hs.apply(hs.signIn("staff1"), "annual", "2026-10-12", "2026-10-12")
	if _, body := hs.get(hs.signIn("pastor"), path); !strings.Contains(body, "Your decision") {
		t.Fatal("overridden approver cannot decide")
	}
	// An empty approver value is refused rather than read as "remove".
	if res, _ := hs.post(hr, "/admin/users/"+itoa(staff.ID)+"/approver", nil); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty approver: %d", res.StatusCode)
	}
}

func TestReassign(t *testing.T) {
	hs := newHarness(t)
	path := hs.apply(hs.signIn("staff1"), "annual", "2026-10-12", "2026-10-12")
	hr := hs.signIn("hr")
	if _, body := hs.get(hr, path); !strings.Contains(body, "Reassign") {
		t.Fatal("admin sees no reassign form")
	}
	pastor, _ := hs.us.ByUsername(t.Context(), "pastor")
	res, _ := hs.post(hr, "/admin/requests"+strings.TrimPrefix(path, "/leave")+"/reassign", url.Values{"approver": {itoa(pastor.ID)}})
	if !strings.Contains(res.Header.Get("Location"), "reassigned") {
		t.Fatalf("reassign: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	if _, body := hs.get(hs.signIn("pastor"), "/approvals"); !strings.Contains(body, "Sam Staff") {
		t.Fatal("reassigned request not in the new approver's queue")
	}
}

func TestTypeAndHolidayAdmin(t *testing.T) {
	hs := newHarness(t)
	hr := hs.signIn("hr")
	res, _ := hs.post(hr, "/admin/types", url.Values{"code": {"study"}, "name": {"Study leave"},
		"allowance": {"5"}, "backdate_days": {"90"}, "cycle_months": {"12"}, "unit": {"workdays"}, "active": {"1"}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("new type: %d", res.StatusCode)
	}
	if _, body := hs.get(hs.signIn("staff1"), "/leave/new"); !strings.Contains(body, "Study leave") {
		t.Fatal("new type not offered")
	}
	res, body := hs.post(hr, "/admin/types", url.Values{"code": {"study"}, "name": {"Dup"}, "backdate_days": {"90"}, "cycle_months": {"12"}, "unit": {"workdays"}})
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "already uses that code") {
		t.Fatalf("duplicate code: %d", res.StatusCode)
	}

	hs.post(hr, "/admin/holidays", url.Values{"date": {"2026-11-04"}, "name": {"Election day"}})
	if _, body := hs.get(hr, "/admin/holidays?year=2026"); !strings.Contains(body, "Election day") {
		t.Fatal("manual holiday missing")
	}
	if _, body := hs.get(hs.signIn("staff1"), "/leave/quote?type="+hs.typeID("annual")+"&start=2026-11-04&end=2026-11-04"); !strings.Contains(body, "all days off or public holidays") {
		t.Fatalf("manual holiday not excluded from working days:\n%s", body)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func (hs *harness) anyTypeID(code string) string {
	hs.t.Helper()
	ts, _ := hs.svc.Types(hs.t.Context(), false)
	for _, t := range ts {
		if t.Code == code {
			return itoa(t.ID)
		}
	}
	hs.t.Fatalf("no type %s", code)
	return ""
}

// The shutdown path through the pages: an admin creates it, a staff member
// sees the drafts on their home page and submits one, the admin sees progress.
func TestShutdownThroughPages(t *testing.T) {
	hs := newHarness(t)
	hr := hs.signIn("hr")
	res, body := hs.post(hr, "/admin/shutdowns", url.Values{"name": {"December shutdown"},
		"type0": {hs.anyTypeID("annual")}, "start0": {"2026-12-07"}, "end0": {"2026-12-11"},
		"type1": {hs.anyTypeID("closure")}, "start1": {"2026-12-21"}, "end1": {"2026-12-31"}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("create shutdown: %d\n%s", res.StatusCode, body)
	}
	sdPath, _, _ := strings.Cut(res.Header.Get("Location"), "?")

	staff := hs.signIn("staff1")
	_, home := hs.get(staff, "/")
	if !strings.Contains(home, "Waiting for you to submit") || !strings.Contains(home, "Company closure") {
		t.Fatalf("home lacks the drafts:\n%s", home)
	}
	draft := regexp.MustCompile(`href="(/leave/\d+)">December shutdown: Annual leave`).FindStringSubmatch(home)
	if draft == nil {
		t.Fatal("no link to the annual-leave draft")
	}
	if _, page := hs.get(staff, draft[1]); !strings.Contains(page, "Submit this leave") || strings.Contains(page, "Withdraw") {
		t.Fatal("draft page offers no submit, or offers a withdraw")
	}
	res, _ = hs.post(staff, draft[1]+"/submit", url.Values{"note": {"ok"}})
	if !strings.Contains(res.Header.Get("Location"), "notice=submitted") {
		t.Fatalf("submit draft: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	// Someone else cannot submit it.
	if res, _ := hs.post(hs.signIn("staff2"), draft[1]+"/submit", nil); res.StatusCode != http.StatusNotFound {
		t.Fatalf("another person submitting: %d", res.StatusCode)
	}

	if _, page := hs.get(hr, sdPath); !strings.Contains(page, "1 awaiting approval") || strings.Contains(page, "Delete this shutdown") {
		t.Fatalf("shutdown page progress, or delete still offered:\n%s", page)
	}
	if res, _ := hs.post(hr, sdPath+"/delete", nil); !strings.Contains(res.Header.Get("Location"), "not-deletable") {
		t.Fatalf("deleting a shutdown with a submitted request: %s", res.Header.Get("Location"))
	}
}

func TestShutdownFormValidation(t *testing.T) {
	hs := newHarness(t)
	res, body := hs.post(hs.signIn("hr"), "/admin/shutdowns", url.Values{"name": {"X"},
		"type0": {hs.anyTypeID("annual")}, "start0": {"2026-12-12"}, "end0": {"2026-12-13"}})
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "all days off or public holidays") {
		t.Fatalf("weekend-only part: %d\n%s", res.StatusCode, body)
	}
	if !strings.Contains(body, `value="2026-12-12"`) {
		t.Fatal("the form lost what was entered")
	}
}

// The type form carries the per-type backdating limit: it shows with the
// 90-day default, saves, and refuses anything but a whole number from 0 to 365
// beside the field, showing what was typed.
func TestTypeFormBackdateDays(t *testing.T) {
	hs := newHarness(t)
	hr := hs.signIn("hr")

	_, body := hs.get(hr, "/admin/types/new")
	if !strings.Contains(body, "Backdating (days)") || !strings.Contains(body, `name="backdate_days"`) || !strings.Contains(body, `value="90"`) {
		t.Fatalf("new type form lacks the backdating field at 90:\n%s", body)
	}
	if strings.Contains(body, "May start in the past") {
		t.Error("the old checkbox is still on the form")
	}

	form := func(days string) url.Values {
		return url.Values{"code": {"study"}, "name": {"Study leave"}, "cycle_months": {"12"}, "unit": {"workdays"},
			"active": {"1"}, "backdate_days": {days}}
	}
	if res, _ := hs.post(hr, "/admin/types", form("30")); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("save at 30: %d", res.StatusCode)
	}
	ts, _ := hs.svc.Types(context.Background(), false)
	var id int64
	for _, ty := range ts {
		if ty.Code == "study" {
			id = ty.ID
			if ty.BackdateDays != 30 {
				t.Fatalf("BackdateDays = %d, want 30", ty.BackdateDays)
			}
		}
	}
	if _, body := hs.get(hr, "/admin/types/"+itoa(id)); !strings.Contains(body, `name="backdate_days" inputmode="numeric" size="4" value="30"`) {
		t.Error("edit form does not show the saved 30")
	}
	if _, body := hs.get(hr, "/admin/types"); !strings.Contains(body, "backdate 30 days") {
		t.Error("types list lacks the backdate badge")
	}

	f := form("0")
	f.Set("id", itoa(id))
	if res, _ := hs.post(hr, "/admin/types", f); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("save at 0: %d", res.StatusCode)
	}
	if _, body := hs.get(hr, "/admin/types"); strings.Contains(body, "backdate 0 days") {
		t.Error("a zero limit should show no badge")
	}

	for _, bad := range []string{"-1", "366", "abc", "", "1.5"} {
		f := form(bad)
		f.Set("id", itoa(id))
		res, body := hs.post(hr, "/admin/types", f)
		if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "whole number of days from 0 to 365") {
			t.Errorf("backdate_days %q: %d, want 422 with a field error", bad, res.StatusCode)
		}
		if bad == "abc" && !strings.Contains(body, `value="abc"`) {
			t.Error("a rejected value did not come back as typed")
		}
	}
	got, _ := hs.svc.Type(context.Background(), id)
	if got.BackdateDays != 0 {
		t.Errorf("a rejected save changed the stored limit to %d", got.BackdateDays)
	}
}

// The sick-leave setting moved onto the types, so the settings page must not
// still offer it.
func TestSettingsPageHasNoSickBackdate(t *testing.T) {
	hs := newHarness(t)
	_, body := hs.get(hs.signIn("hr"), "/admin/settings")
	if strings.Contains(body, "sick_backdate") || strings.Contains(body, "backdating") {
		t.Error("the settings page still has the sick backdating field")
	}
	if !strings.Contains(body, "sick_cycle") {
		t.Error("the settings page lost the sick-cycle field it should keep")
	}
}
