package handler

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// Operations has three people in the fixture directory (headops, staff1, hr).
// At 34% one may be away; a second is over.
func (hs *harness) limitOperations() {
	hs.t.Helper()
	if err := hs.svc.SetCoverageRule(context.Background(), "Operations", 34); err != nil {
		hs.t.Fatal(err)
	}
}

func (hs *harness) form(code, start, end string, extra url.Values) url.Values {
	v := url.Values{"type": {hs.typeID(code)}, "start": {start}, "end": {end}, "note": {"n"}}
	for k, vs := range extra {
		v[k] = vs
	}
	return v
}

func TestOverLimitOffersSpecialPermissionThroughTheChain(t *testing.T) {
	hs := newHarness(t)
	hs.limitOperations()
	hs.apply(hs.signIn("headops"), "annual", "2026-10-12", "2026-10-14")
	staff := hs.signIn("staff1")

	// The live preview carries the warning and the button.
	_, body := hs.get(staff, "/leave/quote?type="+hs.typeID("annual")+"&start=2026-10-12&end=2026-10-14")
	if !strings.Contains(body, "2 of 3") || !strings.Contains(body, "Request special permission") {
		t.Fatalf("quote partial:\n%s", body)
	}

	// A normal submit is refused with the warning, and the page offers the override.
	res, body := hs.post(staff, "/leave", hs.form("annual", "2026-10-12", "2026-10-14", nil))
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("over the limit: %d", res.StatusCode)
	}
	for _, want := range []string{"67% of Operations (2 of 3) would be away on Mon 12 Oct. The limit is 34%.",
		`name="override" value="1"`, "Request special permission"} {
		if !strings.Contains(body, want) {
			t.Fatalf("apply page lacks %q:\n%s", want, body)
		}
	}

	// Posting with override=1 creates an escalated request.
	res, _ = hs.post(staff, "/leave", hs.form("annual", "2026-10-12", "2026-10-14", url.Values{"override": {"1"}}))
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("override: %d", res.StatusCode)
	}
	path, _, _ := strings.Cut(res.Header.Get("Location"), "?")
	if _, body := hs.get(staff, path); !strings.Contains(body, "needs special permission") {
		t.Fatalf("request page does not say it needs special permission:\n%s", body)
	}

	// The head's queue shows the badge, the warning and where approving sends it.
	head := hs.signIn("headops")
	_, body = hs.get(head, "/approvals")
	for _, want := range []string{"Special permission", "2 of 3", "Override and send to Senior Pastor"} {
		if !strings.Contains(body, want) {
			t.Fatalf("head's queue lacks %q:\n%s", want, body)
		}
	}
	res, _ = hs.post(head, path+"/decide", url.Values{"decision": {"approve"}, "from": {"queue"}})
	if res.Header.Get("Location") != "/approvals?notice=escalated" {
		t.Fatalf("first approval: %s", res.Header.Get("Location"))
	}
	if _, body := hs.get(head, "/approvals"); strings.Contains(body, "Sam Staff") {
		t.Fatal("the request is still in the head's queue after they passed it on")
	}

	pastor := hs.signIn("pastor")
	if _, body := hs.get(pastor, path); !strings.Contains(body, "Override and approve") ||
		!strings.Contains(body, "Approved (override) by Olivia Operations") {
		t.Fatalf("pastor's request page:\n%s", body)
	}
	res, _ = hs.post(pastor, path+"/decide", url.Values{"decision": {"approve"}})
	if res.Header.Get("Location") != strings.TrimSuffix(path, "")+"?notice=approved" {
		t.Fatalf("final approval: %s", res.Header.Get("Location"))
	}
	if _, body := hs.get(staff, path); !strings.Contains(body, "approved") {
		t.Fatalf("staff's request page after approval:\n%s", body)
	}
}

func TestNegativeBalanceThroughPages(t *testing.T) {
	hs := newHarness(t)
	staff := hs.signIn("staff1")
	head := hs.signIn("headops")
	hr := hs.signIn("hr")
	big := hs.apply(staff, "annual", "2026-10-12", "2026-10-30")
	hs.post(head, big+"/decide", url.Values{"decision": {"approve"}})

	res, body := hs.post(staff, "/leave", hs.form("annual", "2026-11-02", "2026-11-04", nil))
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "Ask HR to allow going 3 working days below zero") {
		t.Fatalf("overdrawn apply: %d\n%s", res.StatusCode, body)
	}
	if strings.Contains(body, "Request special permission") {
		t.Fatal("offered special permission with no coverage problem")
	}

	res, _ = hs.post(staff, "/leave", hs.form("annual", "2026-11-02", "2026-11-04",
		url.Values{"negative": {"1"}, "note": {"wedding abroad"}}))
	if res.StatusCode != http.StatusSeeOther || !strings.Contains(res.Header.Get("Location"), "notice=negative-requested") {
		t.Fatalf("ask HR: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	path, _, _ := strings.Cut(res.Header.Get("Location"), "?")
	id := strings.TrimPrefix(path, "/leave/")
	if _, body := hs.get(staff, "/"); !strings.Contains(body, "awaiting HR") {
		t.Fatalf("home shows no awaiting HR request:\n%s", body)
	}

	// The admin page and the overview count.
	if _, body := hs.get(hr, "/admin/permissions"); !strings.Contains(body, "Sam Staff") ||
		!strings.Contains(body, "3 working days</strong> below zero") || !strings.Contains(body, "wedding abroad") {
		t.Fatalf("permissions page:\n%s", body)
	}
	if _, body := hs.get(hr, "/admin"); !strings.Contains(body, "waiting for permission") {
		t.Fatalf("admin overview:\n%s", body)
	}
	// Staff cannot grant; refusing needs a note.
	if res, _ := hs.post(staff, "/admin/permissions/"+id+"/grant", nil); res.StatusCode != http.StatusForbidden {
		t.Fatalf("staff granting: %d", res.StatusCode)
	}
	if res, body := hs.post(hr, "/admin/permissions/"+id+"/refuse", url.Values{"note": {""}}); res.StatusCode != http.StatusUnprocessableEntity ||
		!strings.Contains(body, "Say why") {
		t.Fatalf("refusing with no note: %d", res.StatusCode)
	}
	res, _ = hs.post(hr, "/admin/permissions/"+id+"/grant", url.Values{"note": {"ok"}})
	if res.Header.Get("Location") != "/admin/permissions?notice=negative-granted" {
		t.Fatalf("grant: %d %s", res.StatusCode, res.Header.Get("Location"))
	}

	// It is now a draft for staff to submit, with copy that is not about shutdowns.
	_, body = hs.get(staff, "/")
	if !strings.Contains(body, "Waiting for you to submit") || !strings.Contains(body, "HR has allowed this to go below zero") ||
		strings.Contains(body, "company shutdown") {
		t.Fatalf("home with a granted draft:\n%s", body)
	}
	res, _ = hs.post(staff, path+"/submit", url.Values{"note": {""}})
	if res.StatusCode != http.StatusSeeOther || !strings.Contains(res.Header.Get("Location"), "notice=submitted") {
		t.Fatalf("submitting the draft: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	if res, _ := hs.post(head, path+"/decide", url.Values{"decision": {"approve"}}); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("approving: %d", res.StatusCode)
	}
}

func TestRefusedNegativeIsDeclined(t *testing.T) {
	hs := newHarness(t)
	staff, head, hr := hs.signIn("staff1"), hs.signIn("headops"), hs.signIn("hr")
	hs.post(head, hs.apply(staff, "annual", "2026-10-12", "2026-10-30")+"/decide", url.Values{"decision": {"approve"}})
	res, _ := hs.post(staff, "/leave", hs.form("annual", "2026-11-02", "2026-11-04", url.Values{"negative": {"1"}, "note": {""}}))
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("no reason: %d", res.StatusCode)
	}
	res, _ = hs.post(staff, "/leave", hs.form("annual", "2026-11-02", "2026-11-04", url.Values{"negative": {"1"}, "note": {"x"}}))
	path, _, _ := strings.Cut(res.Header.Get("Location"), "?")
	hs.post(hr, "/admin/permissions/"+strings.TrimPrefix(path, "/leave/")+"/refuse", url.Values{"note": {"not this year"}})
	if _, body := hs.get(staff, path); !strings.Contains(body, "declined") || !strings.Contains(body, "not this year") ||
		!strings.Contains(body, "Refused to go below zero by Hannah HR") {
		t.Fatalf("refused request page:\n%s", body)
	}
}

func TestCoverageAdminPage(t *testing.T) {
	hs := newHarness(t)
	hr := hs.signIn("hr")
	_, body := hs.get(hr, "/admin/coverage")
	for _, want := range []string{"Coverage limits", "Operations", "Kids", "Leadership", "All other departments"} {
		if !strings.Contains(body, want) {
			t.Fatalf("coverage page lacks %q:\n%s", want, body)
		}
	}
	ctx := context.Background()
	save := func(dept, pct string) *http.Response {
		res, _ := hs.post(hr, "/admin/coverage", url.Values{"department": {dept}, "percent": {pct}})
		return res
	}
	if res := save("Operations", "40"); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("save: %d", res.StatusCode)
	}
	if res := save("", " 25 "); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("save default: %d", res.StatusCode)
	}
	rules, _ := hs.svc.CoverageRules(ctx)
	if rules["Operations"] != 40 || rules[""] != 25 || len(rules) != 2 {
		t.Fatalf("rules = %v", rules)
	}
	// Strict: only whole numbers 1 to 100.
	for _, bad := range []string{"abc", "0", "101", "+5", "-5", "4.5", "1e1", "1000"} {
		if res := save("Operations", bad); res.StatusCode != http.StatusUnprocessableEntity {
			t.Errorf("percent %q: %d, want 422", bad, res.StatusCode)
		}
	}
	if res := save("Nowhere", "10"); res.StatusCode != http.StatusBadRequest {
		t.Errorf("unknown department: %d, want 400", res.StatusCode)
	}
	if rules, _ = hs.svc.CoverageRules(ctx); rules["Operations"] != 40 || len(rules) != 2 {
		t.Fatalf("a refused save changed the rules: %v", rules)
	}
	// Empty removes.
	if res := save("Operations", ""); res.StatusCode != http.StatusSeeOther {
		t.Fatalf("remove: %d", res.StatusCode)
	}
	if rules, _ = hs.svc.CoverageRules(ctx); len(rules) != 1 || rules[""] != 25 {
		t.Fatalf("rules after removing = %v", rules)
	}
	if res, _ := hs.post(hs.signIn("staff1"), "/admin/coverage", url.Values{"department": {"Operations"}, "percent": {"5"}}); res.StatusCode != http.StatusForbidden {
		t.Fatalf("staff saving a rule: %d", res.StatusCode)
	}
}

func TestTypeFormSavesCoverageRule(t *testing.T) {
	hs := newHarness(t)
	hr := hs.signIn("hr")
	if _, body := hs.get(hr, "/admin/types/new"); !strings.Contains(body, "Never blocked (still counts as away)") {
		t.Fatalf("type form lacks the coverage select:\n%s", body)
	}
	res, _ := hs.post(hr, "/admin/types", url.Values{"name": {"Study leave"}, "code": {"study"}, "cycle_months": {"12"},
		"unit": {"workdays"}, "restricted": {"min"}, "coverage_rule": {"count"}, "active": {"1"}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("save type: %d", res.StatusCode)
	}
	ts, _ := hs.svc.Types(context.Background(), false)
	for _, ty := range ts {
		if ty.Code == "study" && ty.CoverageRule != "count" {
			t.Fatalf("coverage rule = %q", ty.CoverageRule)
		}
	}
	res, _ = hs.post(hr, "/admin/types", url.Values{"name": {"Bad"}, "code": {"bad"}, "cycle_months": {"12"},
		"unit": {"workdays"}, "restricted": {"min"}, "coverage_rule": {"sometimes"}})
	if res.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("bad coverage rule: %d", res.StatusCode)
	}
}
