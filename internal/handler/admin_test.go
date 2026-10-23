package handler

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
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
	// Not to the person whose request it is: a notice that says so.
	res, _ = hs.post(hr, "/admin/requests"+strings.TrimPrefix(path, "/leave")+"/reassign", url.Values{"approver": {itoa(hs.staffID("staff1"))}})
	if !strings.HasSuffix(res.Header.Get("Location"), "?notice=reassign-owner") {
		t.Fatalf("reassign to the owner: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	if _, body := hs.get(hr, res.Header.Get("Location")); !strings.Contains(body, "Choose someone else") {
		t.Fatal("the notice is not shown")
	}
}

// An admin does not adjust their own balance; the form says so, and a
// non-admin is refused outright.
func TestAdminCannotAdjustOwnBalance(t *testing.T) {
	hs := newHarness(t)
	hr := hs.signIn("hr")
	form := url.Values{"target": {hs.typeID("annual") + ":2026-01-01"}, "days": {"2"}, "reason": {"x"}}
	res, body := hs.post(hr, "/admin/users/"+itoa(hs.staffID("hr"))+"/adjustments", form)
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "Another admin adjusts your own balance.") {
		t.Fatalf("own balance: %d\n%s", res.StatusCode, body)
	}
	if res, _ = hs.post(hr, "/admin/users/"+itoa(hs.staffID("staff1"))+"/adjustments", form); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("someone else's: %d", res.StatusCode)
	}
	if res, _ = hs.post(hs.signIn("staff1"), "/admin/users/"+itoa(hs.staffID("staff1"))+"/adjustments", form); res.StatusCode != http.StatusForbidden {
		t.Fatalf("staff: %d", res.StatusCode)
	}
}

func TestTypeAndHolidayAdmin(t *testing.T) {
	hs := newHarness(t)
	hr := hs.signIn("hr")
	res, _ := hs.post(hr, "/admin/types", url.Values{"code": {"study"}, "name": {"Study leave"},
		"allowance": {"5"}, "backdate_days": {"92"}, "cycle_months": {"12"}, "unit": {"workdays"}, "active": {"1"}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("new type: %d", res.StatusCode)
	}
	if _, body := hs.get(hs.signIn("staff1"), "/leave/new"); !strings.Contains(body, "Study leave") {
		t.Fatal("new type not offered")
	}
	res, body := hs.post(hr, "/admin/types", url.Values{"code": {"study"}, "name": {"Dup"}, "backdate_days": {"92"}, "cycle_months": {"12"}, "unit": {"workdays"}})
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
// 92-day default, saves, and refuses anything but a whole number from 0 to 92
// beside the field, showing what was typed.
func TestTypeFormBackdateDays(t *testing.T) {
	hs := newHarness(t)
	hr := hs.signIn("hr")

	_, body := hs.get(hr, "/admin/types/new")
	if !strings.Contains(body, "Backdating (days)") || !strings.Contains(body, `name="backdate_days"`) || !strings.Contains(body, `value="92"`) {
		t.Fatalf("new type form lacks the backdating field at 92:\n%s", body)
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
	if _, body := hs.get(hr, "/admin/types"); !strings.Contains(body, "no backdating") || strings.Contains(body, "backdate 0 days") {
		t.Error("a zero limit should show \"no backdating\"")
	}
	// The default is the norm, so it earns no badge; one day is singular.
	one := form("1")
	one.Set("id", itoa(id))
	hs.post(hr, "/admin/types", one)
	if _, body := hs.get(hr, "/admin/types"); !strings.Contains(body, "backdate 1 day<") {
		t.Error("a limit of one day should read \"1 day\"")
	}
	if _, body := hs.get(hr, "/admin/types"); strings.Count(body, "backdate ") != 1 || strings.Contains(body, "no backdating") {
		t.Error("only the changed type should carry a backdating badge")
	}
	// One pass: a bad number does not hide a missing name.
	f = form("93")
	f.Set("id", itoa(id))
	f.Set("name", "")
	if _, body := hs.post(hr, "/admin/types", f); !strings.Contains(body, "Enter a name.") || !strings.Contains(body, "0 to 92") {
		t.Error("a rejected form did not report the name and the number together")
	}
	f = form("0")
	f.Set("id", itoa(id))
	hs.post(hr, "/admin/types", f)

	for _, bad := range []string{"-1", "93", "366", "abc", "", "1.5"} {
		f := form(bad)
		f.Set("id", itoa(id))
		res, body := hs.post(hr, "/admin/types", f)
		if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "whole number of days from 0 to 92") {
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

func (hs *harness) staffID(username string) int64 {
	hs.t.Helper()
	u, err := hs.h.users.ByUsername(context.Background(), username)
	if err != nil {
		hs.t.Fatal(err)
	}
	return u.ID
}

func TestAdminRecordLeave(t *testing.T) {
	hs := newHarness(t)
	hr := hs.signIn("hr")
	staffID := hs.staffID("staff1")
	page := "/admin/users/" + itoa(staffID)
	form := func(start, end, note string) url.Values {
		return url.Values{"type": {hs.typeID("annual")}, "start": {start}, "end": {end}, "note": {note}}
	}

	// The form is on the person's page, with the date picker's scripts.
	_, body := hs.get(hr, page)
	for _, want := range []string{`action="` + page + `/leave#record"`, `id="record"`, `<date-range-input`, "date-range-input.js", `name="note"`} {
		if !strings.Contains(body, want) {
			t.Errorf("admin page lacks %q", want)
		}
	}

	// A missing reason re-renders with what was typed (422), and the other
	// form's errors stay clear.
	res, body := hs.post(hr, page+"/leave", form("2026-06-15", "2026-06-19", ""))
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "Say why this is being recorded") ||
		!strings.Contains(body, `value="2026-06-15"`) {
		t.Fatalf("no note: %d\n%s", res.StatusCode, body)
	}
	for _, leaked := range []string{"Say why, for whoever reads this later", "Enter a number of days", "Choose a leave type.", "Choose a cycle."} {
		if strings.Contains(body, leaked) {
			t.Errorf("the adjustment form shows %q for a refused record", leaked)
		}
	}

	// 120 days before the pinned "today" works; redirect with a notice.
	res, _ = hs.post(hr, page+"/leave", form("2026-06-15", "2026-06-19", "Taken before go-live"))
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != page+"?notice=recorded" {
		t.Fatalf("record: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	_, body = hs.get(hr, page+"?notice=recorded")
	if !strings.Contains(body, "Leave recorded.") ||
		!regexp.MustCompile(`Mon 15 Jun 2026 – Fri 19 Jun 2026</a></td>\s*<td>Annual leave</td>\s*<td class="num">4</td>\s*<td><span class="badge[^"]*">approved</span>\s*<span class="badge">recorded</span>`).MatchString(body) {
		t.Fatalf("page after recording:\n%s", body)
	}

	// The person sees it on their own page, and the request page says who.
	_, body = hs.get(hs.signIn("staff1"), "/")
	if !strings.Contains(body, "Mon 15 Jun 2026 – Fri 19 Jun 2026") || !strings.Contains(body, "approved") {
		t.Fatalf("staff home lacks the recorded leave:\n%s", body)
	}

	// Overlap is refused with a message.
	res, body = hs.post(hr, page+"/leave", form("2026-06-17", "2026-06-17", "again"))
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "They already have leave booked") {
		t.Fatalf("overlap: %d\n%s", res.StatusCode, body)
	}

	// Nobody records their own leave.
	res, body = hs.post(hr, "/admin/users/"+itoa(hs.staffID("hr"))+"/leave", form("2026-06-22", "2026-06-22", "x"))
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "Another admin records your own leave.") {
		t.Fatalf("self: %d\n%s", res.StatusCode, body)
	}
	// Leave that has not started goes through an application.
	res, body = hs.post(hr, page+"/leave", form("2026-12-07", "2026-12-07", "x"))
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "already happened") {
		t.Fatalf("future: %d\n%s", res.StatusCode, body)
	}
	// An unknown person is a 404.
	if res, _ = hs.post(hr, "/admin/users/9999/leave", form("2026-06-22", "2026-06-22", "x")); res.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown person: %d", res.StatusCode)
	}
}

func TestAdminUnrecord(t *testing.T) {
	hs := newHarness(t)
	hr := hs.signIn("hr")
	staffID := hs.staffID("staff1")
	page := "/admin/users/" + itoa(staffID)
	res, _ := hs.post(hr, page+"/leave", url.Values{"type": {hs.typeID("annual")}, "start": {"2026-06-15"},
		"end": {"2026-06-19"}, "note": {"x"}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("record: %d", res.StatusCode)
	}
	// Find the request page from the admin page.
	_, body := hs.get(hr, page)
	m := regexp.MustCompile(`href="(/leave/\d+)\?from=admin"`).FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no request link:\n%s", body)
	}
	_, body = hs.get(hr, m[1])
	if !strings.Contains(body, "Recorded by") || !strings.Contains(body, "Why it was recorded") ||
		!strings.Contains(body, "Remove this recorded leave</button>") || strings.Contains(body, "<th>Approver</th>") {
		t.Fatalf("request page for an admin:\n%s", body)
	}
	// The owner sees who recorded it but has no remove button.
	_, body = hs.get(hs.signIn("staff1"), m[1])
	if !strings.Contains(body, "Recorded by") || strings.Contains(body, "Remove this recorded leave") {
		t.Fatalf("request page for the owner:\n%s", body)
	}
	res, _ = hs.post(hs.signIn("staff1"), "/admin/requests/"+strings.TrimPrefix(m[1], "/leave/")+"/unrecord", nil)
	if res.StatusCode != http.StatusForbidden {
		t.Fatalf("staff removing: %d", res.StatusCode)
	}
	res, _ = hs.post(hr, "/admin/requests/"+strings.TrimPrefix(m[1], "/leave/")+"/unrecord", nil)
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != page+"?notice=unrecorded" {
		t.Fatalf("unrecord: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	_, body = hs.get(hr, m[1])
	if !regexp.MustCompile(`Recorded by</th><td>Hannah HR <small>on \w{3} \d+ \w{3} \d{4}</small>`).MatchString(body) ||
		!regexp.MustCompile(`Removed by</th><td>Hannah HR <small>on \w{3} \d+ \w{3} \d{4}</small>`).MatchString(body) ||
		!strings.Contains(body, "cancelled") || strings.Contains(body, "Remove this recorded leave") {
		t.Fatalf("after removal:\n%s", body)
	}
	if res, _ = hs.post(hr, "/admin/requests/"+strings.TrimPrefix(m[1], "/leave/")+"/unrecord", nil); res.Header.Get("Location") != m[1]+"?notice=unrecord-gone" {
		t.Fatalf("removing twice: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
}

// An approver's note that reads "Recorded by ..." is just a reason: the request
// page still shows it as one and offers no way to remove the leave.
func TestApprovalNoteDoesNotLookRecorded(t *testing.T) {
	hs := newHarness(t)
	path := hs.apply(hs.signIn("staff1"), "annual", "2026-10-12", "2026-10-12")
	id := strings.TrimPrefix(path, "/leave/")
	hs.post(hs.signIn("headops"), "/leave/"+id+"/decide", url.Values{"decision": {"approve"}, "note": {"Recorded by me in the team diary too"}})
	_, body := hs.get(hs.signIn("hr"), path)
	if !strings.Contains(body, "Recorded by me in the team diary too") || !strings.Contains(body, "<th>Reason</th>") ||
		strings.Contains(body, "<th>Recorded by</th>") || strings.Contains(body, "Remove this recorded leave") {
		t.Fatalf("request page:\n%s", body)
	}
}

// A retired type may still be what past leave was, so the form lists it, marked.
func TestRecordFormListsInactiveTypes(t *testing.T) {
	hs := newHarness(t)
	ts, _ := hs.svc.Types(context.Background(), true)
	for _, ty := range ts {
		if ty.Code == "parental" {
			ty.Active = false
			if _, err := hs.svc.SaveType(context.Background(), ty); err != nil {
				t.Fatal(err)
			}
		}
	}
	_, body := hs.get(hs.signIn("hr"), "/admin/users/"+itoa(hs.staffID("staff1")))
	if !strings.Contains(body, "Parental leave (inactive)") || strings.Contains(body, "Annual leave (inactive)") {
		t.Fatalf("record form types:\n%s", body)
	}
}

func TestAdminPeopleSearchAndAnnual(t *testing.T) {
	hs := newHarness(t)
	// 3 pending workdays of the 15 allowed leave 12 for Sam, 15 for everyone else.
	hs.apply(hs.signIn("staff1"), "annual", "2026-10-12", "2026-10-14")
	hr := hs.signIn("hr")
	_, body := hs.get(hr, "/admin/users")
	if !strings.Contains(body, "Annual leave available") || !strings.Contains(body, `name="q"`) {
		t.Error("no annual column or search box")
	}
	row := func(body, name string) string {
		i := strings.Index(body, ">"+name+"</a>")
		if i < 0 {
			return ""
		}
		j := strings.Index(body[i:], "</tr>")
		return body[i : i+j]
	}
	if r := row(body, "Sam Staff"); !strings.Contains(r, `<td class="num">12</td>`) {
		t.Errorf("Sam's row: %s", r)
	}
	if r := row(body, "Thandi Teacher"); !strings.Contains(r, `<td class="num">15</td>`) {
		t.Errorf("Thandi's row: %s", r)
	}
	for q, want := range map[string][]string{
		"sam":            {"Sam Staff"},
		"STAFF2":         {"Thandi Teacher"},
		"headkids@":      {"Kagiso Kids"},
		"kids":           {"Kagiso Kids", "Thandi Teacher"},
		"  operations  ": {"Olivia Operations", "Sam Staff", "Hannah HR"},
		"nobody-at-all":  nil,
	} {
		_, body := hs.get(hr, "/admin/users?q="+url.QueryEscape(q))
		for _, n := range []string{"Senior Pastor", "Olivia Operations", "Kagiso Kids", "Sam Staff", "Thandi Teacher", "Hannah HR"} {
			if got, w := row(body, n) != "", slices.Contains(want, n); got != w {
				t.Errorf("q=%q: %s shown=%v, want %v", q, n, got, w)
			}
		}
	}
}
