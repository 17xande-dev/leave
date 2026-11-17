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

var seenRE = regexp.MustCompile(`name="seen" value="([0-9a-f]{24})"`)

func TestAdminCorrectRoutesAreProtected(t *testing.T) {
	hs := newHarness(t)
	want := map[string]bool{
		"GET /admin/requests/{id}/correct":       false,
		"GET /admin/requests/{id}/correct/quote": false,
		"POST /admin/requests/{id}/correct":      false,
	}
	for _, rt := range hs.h.adminRoutes() {
		if _, ok := want[rt.pattern]; ok {
			want[rt.pattern] = true
		}
	}
	// The anonymous and non-admin sweeps (TestProtectedRoutes, TestAdminRoutesRefuseStaff)
	// walk adminRoutes, so being in it is what protects them.
	for p, found := range want {
		if !found {
			t.Errorf("%s is not an admin route", p)
		}
	}
	anon, staff := hs.client(), hs.signIn("staff1")
	for _, p := range []string{"/admin/requests/1/correct", "/admin/requests/1/correct/quote"} {
		res, err := anon.Get(hs.srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusSeeOther {
			t.Errorf("anonymous GET %s: %d", p, res.StatusCode)
		}
		if code, _ := hs.get(staff, p); code != http.StatusForbidden {
			t.Errorf("staff GET %s: %d", p, code)
		}
	}
	res, err := anon.PostForm(hs.srv.URL+"/admin/requests/1/correct", url.Values{})
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther && res.StatusCode != http.StatusUnauthorized {
		t.Errorf("anonymous POST: %d", res.StatusCode)
	}
	if res, _ := hs.post(staff, "/admin/requests/1/correct", url.Values{}); res.StatusCode != http.StatusForbidden {
		t.Errorf("staff POST: %d", res.StatusCode)
	}
}

func TestAdminCorrectFlow(t *testing.T) {
	hs := newHarness(t)
	staff, hr := hs.signIn("staff1"), hs.signIn("hr")
	page := hs.apply(staff, "annual", "2026-10-12", "2026-10-14")
	id := strings.TrimPrefix(page, "/leave/")
	form := "/admin/requests/" + id + "/correct"

	if _, body := hs.get(staff, page); strings.Contains(body, "Correct this request") || strings.Contains(body, "Corrections") {
		t.Fatalf("owner sees the admin link or an empty panel:\n%s", body)
	}
	_, body := hs.get(hr, page)
	if !strings.Contains(body, `href="`+form+`"`) {
		t.Fatalf("admin has no correct link:\n%s", body)
	}

	_, body = hs.get(hr, form)
	m := seenRE.FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("form lacks the fingerprint:\n%s", body)
	}
	seen := m[1]
	for _, want := range []string{`name="reason"`, `name="half_start"`, `name="half_end"`, `Compassionate leave (admin only)`, `Unpaid leave (admin only)`,
		`hx-get="` + form + `/quote"`, `action="` + form + `"`} {
		if !strings.Contains(body, want) {
			t.Errorf("form lacks %q", want)
		}
	}
	if strings.Contains(body, "Closure") && regexp.MustCompile(`<option value="\d+"[^>]*>Company closure`).MatchString(body) {
		t.Error("the retired closure type is offered")
	}

	// The preview is computed for the owner and says what it was.
	code, preview := hs.get(hr, form+"/quote?"+url.Values{"type": {hs.typeID("compassionate")}, "start": {"2026-10-13"}, "end": {"2026-10-14"}, "note": {"x"}}.Encode())
	if code != 200 || !strings.Contains(preview, "Was 3 ") || !strings.Contains(preview, "now 2 ") {
		t.Fatalf("preview: %d\n%s", code, preview)
	}

	fields := func(reason string) url.Values {
		return url.Values{"type": {hs.typeID("compassionate")}, "start": {"2026-10-13", "2026-10-19"}, "end": {"2026-10-14", "2026-10-20"},
			"note": {"funeral"}, "reason": {reason}, "seen": {seen}}
	}
	res, body := hs.post(hr, form, fields("  "))
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, leave.MsgCorrectReason) {
		t.Fatalf("no reason: %d\n%s", res.StatusCode, body)
	}
	// The repeated pairs round-trip through the form, so nothing typed is lost.
	for _, want := range []string{`value="2026-10-13"`, `value="2026-10-14"`, `value="2026-10-19"`, `value="2026-10-20"`, `>funeral</textarea>`} {
		if !strings.Contains(body, want) {
			t.Errorf("re-rendered form lost %s", want)
		}
	}
	noSeen := fields("why")
	noSeen.Del("seen")
	if res, _ := hs.postRaw(hr, form, noSeen); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing fingerprint: %d", res.StatusCode)
	}
	if res, _ := hs.post(hr, form, func() url.Values { v := fields("why"); v.Set("seen", "zz"); return v }()); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad fingerprint: %d", res.StatusCode)
	}

	res, body = hs.post(hr, form, fields("It was a bereavement"))
	if res.StatusCode != http.StatusSeeOther || !strings.Contains(res.Header.Get("Location"), "notice=corrected") {
		t.Fatalf("correct: %d %s\n%s", res.StatusCode, res.Header.Get("Location"), body)
	}
	_, body = hs.get(hr, res.Header.Get("Location"))
	for _, want := range []string{"Request corrected. Sam Staff has been told.", "<h2>Corrections</h2>", "Changed by Hannah HR on",
		"Annual leave", "Compassionate leave", "Reason: It was a bereavement", "4 working days"} {
		if !strings.Contains(body, want) {
			t.Errorf("request page lacks %q", want)
		}
	}
	// The owner sees the history too.
	if _, body := hs.get(staff, page); !strings.Contains(body, "<h2>Corrections</h2>") || !strings.Contains(body, "Reason: It was a bereavement") || strings.Contains(body, "Correct this request") {
		t.Errorf("owner's page:\n%s", body)
	}
	// A form built before the correction is stale now.
	res, _ = hs.post(hr, form, fields("again"))
	if res.StatusCode != http.StatusSeeOther || !strings.Contains(res.Header.Get("Location"), "notice=changed") {
		t.Fatalf("stale form: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	// Approved leave is not correctable (until 11d): the form says so.
	head := hs.signIn("headops")
	hs.approveAs(head, page)
	res, _ = hs.post(hr, form, fields("late"))
	if res.StatusCode != http.StatusSeeOther || !strings.Contains(res.Header.Get("Location"), "notice=not-possible") {
		t.Fatalf("approved: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	if code, _ := hs.get(hr, form); code != http.StatusSeeOther {
		t.Fatalf("approved GET: %d", code)
	}
}

func TestAdminCorrectRefusesOwnRequest(t *testing.T) {
	hs := newHarness(t)
	hr := hs.signIn("hr")
	page := hs.apply(hr, "annual", "2026-10-12", "2026-10-14")
	form := "/admin/requests/" + strings.TrimPrefix(page, "/leave/") + "/correct"
	if _, body := hs.get(hr, page); strings.Contains(body, "Correct this request") {
		t.Fatal("an admin is offered a correction of their own request")
	}
	req, err := hs.svc.Get(context.Background(), mustAtoi(t, strings.TrimPrefix(page, "/leave/")))
	if err != nil {
		t.Fatal(err)
	}
	res, _ := hs.post(hr, form, url.Values{"type": {hs.typeID("unpaid")}, "start": {"2026-10-12"}, "end": {"2026-10-14"},
		"reason": {"x"}, "seen": {req.Fingerprint()}})
	if res.StatusCode != http.StatusSeeOther || !strings.Contains(res.Header.Get("Location"), "notice=correct-own") {
		t.Fatalf("own POST: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	if code, _ := hs.get(hr, form+"/quote"); code != http.StatusSeeOther {
		t.Fatalf("own quote: %d", code)
	}
	if _, body := hs.get(hr, "/leave/"+strings.TrimPrefix(page, "/leave/")+"?notice=correct-own"); !strings.Contains(body, leave.MsgCorrectOwn) {
		t.Fatalf("notice not shown:\n%s", body)
	}
	got, _ := hs.svc.Get(context.Background(), req.ID)
	if got.TypeID != req.TypeID {
		t.Fatal("own request was corrected")
	}
}
