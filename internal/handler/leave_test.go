package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/17xande-dev/leave/internal/leave"
)

func (hs *harness) typeID(code string) string {
	hs.t.Helper()
	ts, err := hs.svc.Types(context.Background(), true)
	if err != nil {
		hs.t.Fatal(err)
	}
	for _, t := range ts {
		if t.Code == code {
			return strconv.FormatInt(t.ID, 10)
		}
	}
	hs.t.Fatalf("no type %s", code)
	return ""
}

// apply submits the form and returns the new request's path.
func (hs *harness) apply(c *http.Client, code, start, end string) string {
	hs.t.Helper()
	res, body := hs.post(c, "/leave", url.Values{"type": {hs.typeID(code)}, "start": {start}, "end": {end}, "note": {"n"}})
	if res.StatusCode != http.StatusSeeOther {
		hs.t.Fatalf("apply: %d\n%s", res.StatusCode, body)
	}
	loc, _, _ := strings.Cut(res.Header.Get("Location"), "?")
	return loc
}

// The whole path through the pages: a staff member applies, their head sees
// it in the queue and approves it from there, the balance moves.
func TestApplyAndApproveThroughPages(t *testing.T) {
	hs := newHarness(t)
	staff := hs.signIn("staff1")
	path := hs.apply(staff, "annual", "2026-10-12", "2026-10-14")

	code, body := hs.get(staff, path)
	if code != 200 || !strings.Contains(body, "Olivia Operations") || !strings.Contains(body, "pending") {
		t.Fatalf("request page: %d\n%s", code, body)
	}

	head := hs.signIn("headops")
	if _, body := hs.get(head, "/approvals"); !strings.Contains(body, "Sam Staff") || !strings.Contains(body, `class="count">1<`) {
		t.Fatalf("head's queue lacks the request or the badge:\n%s", body)
	}
	id := strings.TrimPrefix(path, "/leave/")
	res, _ := hs.post(head, "/leave/"+id+"/decide", url.Values{"decision": {"approve"}, "from": {"queue"}})
	if res.StatusCode != http.StatusSeeOther || res.Header.Get("Location") != "/approvals?notice=approved" {
		t.Fatalf("approve: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	if _, body := hs.get(staff, "/"); !strings.Contains(body, `<td class="num">12</td>`) {
		t.Fatalf("home balance after approving 3 days:\n%s", body)
	}
	if _, body := hs.get(head, "/team"); !strings.Contains(body, "Sam Staff") {
		t.Fatalf("team view lacks the approved leave")
	}
}

// Someone else's request is a 404: its existence is not confirmed.
func TestRequestNotVisibleToOthers(t *testing.T) {
	hs := newHarness(t)
	path := hs.apply(hs.signIn("staff1"), "annual", "2026-10-12", "2026-10-12")
	if code, _ := hs.get(hs.signIn("staff2"), path); code != http.StatusNotFound {
		t.Fatalf("another staff member viewing: %d, want 404", code)
	}
	// An admin can see everything.
	if code, _ := hs.get(hs.signIn("hr"), path); code != 200 {
		t.Fatalf("admin viewing: %d", code)
	}
}

// The decision must be explicit: an empty or unknown value is a 400, never a
// silent approve or decline.
func TestDecideRequiresExplicitDecision(t *testing.T) {
	hs := newHarness(t)
	path := hs.apply(hs.signIn("staff1"), "annual", "2026-10-12", "2026-10-12")
	head := hs.signIn("headops")
	for _, v := range []string{"", "yes", "1"} {
		if res, _ := hs.post(head, path+"/decide", url.Values{"decision": {v}}); res.StatusCode != http.StatusBadRequest {
			t.Errorf("decision=%q: %d, want 400", v, res.StatusCode)
		}
	}
	// Declining needs a reason, and the form comes back with it asked for.
	res, body := hs.post(head, path+"/decide", url.Values{"decision": {"decline"}})
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "Say why") {
		t.Fatalf("decline without note: %d", res.StatusCode)
	}
	// A staff member cannot decide their colleague's leave by posting directly.
	if res, _ := hs.post(hs.signIn("staff2"), path+"/decide", url.Values{"decision": {"approve"}}); res.StatusCode != http.StatusNotFound {
		t.Fatalf("outsider deciding: %d, want 404", res.StatusCode)
	}
}

func TestApplyValidationRerendersForm(t *testing.T) {
	hs := newHarness(t)
	c := hs.signIn("staff1")
	res, body := hs.post(c, "/leave", url.Values{"type": {hs.typeID("annual")}, "start": {"2026-10-10"}, "end": {"2026-10-11"}})
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "all days off or public holidays") {
		t.Fatalf("weekend-only request: %d\n%s", res.StatusCode, body)
	}
	if !strings.Contains(body, `value="2026-10-10"`) {
		t.Fatal("the form lost what was entered")
	}
}

// The two dates are one <date-range-input>. It must submit start and end like the
// inputs it replaced, and the page must still work with no script: the native
// inputs inside it are the fallback, and a refused form hands the values back.
func TestApplyPageDateRange(t *testing.T) {
	hs := newHarness(t)
	c := hs.signIn("staff1")
	_, body := hs.get(c, "/leave/new")
	tag := regexp.MustCompile(`(?s)<date-range-input[^>]*>`).FindString(body)
	for _, want := range []string{`start-name="start"`, `end-name="end"`, ` required`, `months="2"`, `first-day-of-week="1"`, `with-clear`} {
		if !strings.Contains(tag, want) {
			t.Errorf("date-range-input lacks %q: %s", want, tag)
		}
	}
	if strings.Contains(tag, "value=") {
		t.Errorf("a blank form should not preset a value: %s", tag)
	}
	for _, want := range []string{`<input type="date" id="start" name="start"`, `<input type="date" id="end" name="end"`} {
		if !strings.Contains(body, want) {
			t.Errorf("no-JS fallback input missing: %s", want)
		}
	}

	// The scripts are loaded as modules from their content-hashed URLs, and
	// those URLs resolve to JavaScript (a typo would be a silent /missing/ 404).
	for _, name := range []string{"cally.js", "date-range-input.js", "apply.js"} {
		m := regexp.MustCompile(`<script type="module" src="(/static/` + regexp.QuoteMeta(name) + `\?v=[0-9a-f]+)"`).FindStringSubmatch(body)
		if m == nil {
			t.Errorf("apply page does not load %s as a module", name)
			continue
		}
		res, err := c.Get(hs.srv.URL + m[1])
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 200 || !strings.HasPrefix(res.Header.Get("Content-Type"), "text/javascript") {
			t.Errorf("%s: %d %q", name, res.StatusCode, res.Header.Get("Content-Type"))
		}
	}
	// Only the apply page needs them.
	if _, home := hs.get(c, "/"); strings.Contains(home, "date-range-input.js") {
		t.Error("the home page loads the date picker")
	}

	// A refused submission comes back with the range in the element's value and
	// in the fallback inputs.
	_, body = hs.post(c, "/leave", url.Values{"type": {hs.typeID("annual")}, "start": {"2026-10-10"}, "end": {"2026-10-11"}})
	if !strings.Contains(body, `value="2026-10-10/2026-10-11"`) || !strings.Contains(body, `name="end" value="2026-10-11"`) {
		t.Errorf("refused form lost the range:\n%s", body)
	}
	// A start with no end is not a range: the element is left blank and the
	// native inputs keep what was entered.
	_, body = hs.post(c, "/leave", url.Values{"type": {hs.typeID("annual")}, "start": {"2026-10-12"}})
	if tag := regexp.MustCompile(`(?s)<date-range-input[^>]*>`).FindString(body); strings.Contains(tag, "value=") {
		t.Errorf("partial range preset on the element: %s", tag)
	}
	if !strings.Contains(body, `name="start" value="2026-10-12"`) || !strings.Contains(body, "Enter the last day") {
		t.Errorf("partial range lost:\n%s", body)
	}
}

func TestQuotePartial(t *testing.T) {
	hs := newHarness(t)
	c := hs.signIn("staff1")
	_, body := hs.get(c, fmt.Sprintf("/leave/quote?type=%s&start=2026-10-12&end=2026-10-16", hs.typeID("annual")))
	if !strings.Contains(body, "5 working days") || !strings.Contains(body, "10 left") {
		t.Fatalf("quote:\n%s", body)
	}
	if strings.Contains(body, "<html") {
		t.Fatal("the partial came wrapped in a layout")
	}
	_, body = hs.get(c, "/leave/quote?type=&start=")
	if !strings.Contains(body, "Choose a type") {
		t.Fatalf("empty quote:\n%s", body)
	}
}

func TestCancellationThroughPages(t *testing.T) {
	hs := newHarness(t)
	staff := hs.signIn("staff1")
	head := hs.signIn("headops")
	path := hs.apply(staff, "annual", "2026-10-12", "2026-10-12")
	hs.post(head, path+"/decide", url.Values{"decision": {"approve"}})

	if _, body := hs.get(staff, path); !strings.Contains(body, "Request cancellation") {
		t.Fatal("approved future leave offers no cancellation")
	}
	res, _ := hs.post(staff, path+"/cancel", url.Values{"note": {"plans changed"}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("request cancellation: %d", res.StatusCode)
	}
	cpath, _, _ := strings.Cut(res.Header.Get("Location"), "?")
	if _, body := hs.get(staff, path); !strings.Contains(body, "waiting for approval") || strings.Contains(body, "Request cancellation") {
		t.Fatal("original leave page does not show the pending cancellation")
	}
	// A second attempt (another tab) lands on a clear notice, not a 500.
	res, _ = hs.post(staff, path+"/cancel", nil)
	if !strings.Contains(res.Header.Get("Location"), "not-possible") {
		t.Fatalf("second cancellation: %d %s", res.StatusCode, res.Header.Get("Location"))
	}
	hs.post(head, cpath+"/decide", url.Values{"decision": {"approve"}})
	if _, body := hs.get(staff, path); !strings.Contains(body, "cancelled") {
		t.Fatal("original not cancelled after approval")
	}
}

// The picker's earliest day mirrors the server's backdating limit: every
// option carries its type's cut-off, the picker and the no-JS inputs carry the
// earliest across the types, and a re-render after a refusal carries the chosen
// type's own. "Today" is pinned to 5 October 2026, so 92 days back is 5 July.
func TestApplyPageCarriesBackdateMin(t *testing.T) {
	hs := newHarness(t)
	c := hs.signIn("staff1")
	ts, _ := hs.svc.Types(context.Background(), true)
	annual, _ := hs.svc.Type(context.Background(), mustAtoi(t, hs.typeID("annual")))
	sick, _ := hs.svc.Type(context.Background(), mustAtoi(t, hs.typeID("sick")))
	annual.BackdateDays, sick.BackdateDays = 10, 92
	for _, ty := range []leave.Type{annual, sick} {
		if _, err := hs.svc.SaveType(context.Background(), ty); err != nil {
			t.Fatal(err)
		}
	}

	_, body := hs.get(c, "/leave/new")
	n := 0
	for _, ty := range ts {
		want := map[string]string{"annual": "2026-09-25", "sick": "2026-07-05"}[ty.Code]
		if want == "" {
			want = "2026-07-05" // the other seeded types
		}
		if !strings.Contains(body, `<option value="`+strconv.FormatInt(ty.ID, 10)+`" data-min="`+want+`"`) {
			t.Errorf("option for %s lacks data-min=%s", ty.Code, want)
		}
		n++
	}
	if n < 4 {
		t.Fatalf("checked only %d options", n)
	}
	// Nothing chosen: the earliest of the offered types.
	if !strings.Contains(body, `months="2" first-day-of-week="1" with-clear locale="en-GB" min="2026-07-05"`) {
		t.Errorf("picker lacks the earliest min:\n%s", body)
	}
	for _, id := range []string{"start", "end"} {
		if !regexp.MustCompile(`<input type="date" id="` + id + `"[^>]* min="2026-07-05"`).MatchString(body) {
			t.Errorf("fallback input %s lacks min", id)
		}
	}

	// A refused submit re-renders with the chosen type's own limit.
	res, body := hs.post(c, "/leave", url.Values{"type": {hs.typeID("annual")}, "start": {"2026-09-01"}, "end": {"2026-09-01"}, "note": {"n"}})
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, "Annual leave can start at most 10 days ago (no earlier than Fri 25 Sep 2026).") {
		t.Fatalf("backdated beyond the limit: %d", res.StatusCode)
	}
	if !strings.Contains(body, `locale="en-GB" min="2026-09-25"`) || !strings.Contains(body, `id="start" name="start" value="2026-09-01" min="2026-09-25"`) {
		t.Error("re-render did not use the selected type's min")
	}
}

func mustAtoi(t *testing.T, s string) int64 {
	t.Helper()
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// The back link on a request goes where the person came from, by a fixed map:
// an unknown value is the default and its text is never echoed.
func TestRequestBackLink(t *testing.T) {
	hs := newHarness(t)
	staff := hs.signIn("staff1")
	path := hs.apply(staff, "annual", "2026-10-12", "2026-10-12")
	for _, c := range []struct{ from, href, label string }{
		{"", `href="/"`, "← My leave"},
		{"approvals", `href="/approvals"`, "← Approvals"},
		{"team", `href="/team"`, "← Team"},
		{"admin", `href="/admin/requests"`, "← Admin"},
		{"<script>alert(1)</script>", `href="/"`, "← My leave"},
	} {
		_, body := hs.get(staff, path+"?from="+url.QueryEscape(c.from))
		want := `<a class="back" ` + c.href + `>` + c.label + `</a>`
		if !strings.Contains(body, want) {
			t.Errorf("from=%q: no %s", c.from, want)
		}
		if strings.Contains(c.from, "script") && strings.Contains(body, "alert(1)") {
			t.Errorf("from=%q was echoed", c.from)
		}
	}
}

// The list pages add the source to their links to a request.
func TestRequestLinksCarrySource(t *testing.T) {
	hs := newHarness(t)
	path := hs.apply(hs.signIn("staff1"), "annual", "2026-10-12", "2026-10-12")
	id := strings.TrimPrefix(path, "/leave/")
	hr := hs.signIn("hr")
	sam, err := hs.us.ByUsername(context.Background(), "staff1")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		client     *http.Client
		page, from string
	}{
		{hs.signIn("headops"), "/approvals", "approvals"},
		{hs.signIn("headops"), "/team", "team"},
		{hr, "/admin/requests", "admin"},
		{hr, "/admin/users/" + strconv.FormatInt(sam.ID, 10), "admin"},
	} {
		_, body := hs.get(c.client, c.page)
		if want := `href="/leave/` + id + `?from=` + c.from + `"`; !strings.Contains(body, want) {
			t.Errorf("%s: no link %s", c.page, want)
		}
	}
	_, body := hs.get(hs.signIn("staff1"), "/")
	if !strings.Contains(body, `href="/leave/`+id+`"`) {
		t.Error("home links should carry no source")
	}
}

// The back link survives the step after the page: a decline refused for want of
// a note re-renders with it, and the redirect after a decline carries it.
func TestBackLinkSurvivesDecide(t *testing.T) {
	hs := newHarness(t)
	path := hs.apply(hs.signIn("staff1"), "annual", "2026-10-12", "2026-10-12")
	head := hs.signIn("headops")
	back := `<a class="back" href="/approvals">← Approvals</a>`
	res, body := hs.post(head, path+"/decide?from=approvals", url.Values{"decision": {"decline"}})
	if res.StatusCode != http.StatusUnprocessableEntity || !strings.Contains(body, back) {
		t.Fatalf("re-render %d, back link missing", res.StatusCode)
	}
	if !strings.Contains(body, `action="`+path+`/decide?from=approvals"`) {
		t.Error("decide form lost the source")
	}
	res, _ = hs.post(head, path+"/decide?from=approvals", url.Values{"decision": {"decline"}, "note": {"no"}})
	if loc := res.Header.Get("Location"); loc != path+"?notice=declined&from=approvals" {
		t.Fatalf("redirect to %q", loc)
	}
	_, body = hs.get(head, path+"?notice=declined&from=approvals")
	if !strings.Contains(body, back) {
		t.Error("back link lost after the redirect")
	}
	// An unknown source is dropped, not carried or echoed.
	p2 := hs.apply(hs.signIn("staff1"), "annual", "2026-10-19", "2026-10-19")
	res, _ = hs.post(head, p2+"/decide?from=%3Cb%3E", url.Values{"decision": {"decline"}, "note": {"no"}})
	if loc := res.Header.Get("Location"); loc != p2+"?notice=declined" {
		t.Fatalf("redirect to %q", loc)
	}
}

// approveAs has the approver decide a request (by path) and fails unless it worked.
func (hs *harness) approveAs(approver *http.Client, path string) {
	hs.t.Helper()
	res, body := hs.post(approver, path+"/decide", url.Values{"decision": {"approve"}, "from": {"queue"}})
	if res.StatusCode != http.StatusSeeOther {
		hs.t.Fatalf("approve %s: %d\n%s", path, res.StatusCode, body)
	}
}

// The home page, with today pinned to Monday 5 October 2026: the leave ending
// today is both "now" and upcoming, the one that ended yesterday is past, and
// pending requests are counted but not offered as the next leave.
func TestHomeUpNextAndSplit(t *testing.T) {
	hs := newHarness(t)
	staff := hs.signIn("staff1")
	head := hs.signIn("headops")

	_, body := hs.get(staff, "/")
	if !strings.Contains(body, "No leave booked.") || !strings.Contains(body, "You have not applied for any leave yet.") ||
		strings.Contains(body, "waiting for a decision") || !strings.Contains(body, "15</strong>") {
		t.Fatalf("empty home:\n%s", body)
	}

	yesterday := hs.apply(staff, "annual", "2026-10-02", "2026-10-04") // Fri to Sun: ended yesterday
	hs.approveAs(head, yesterday)
	_, body = hs.get(staff, "/")
	if !strings.Contains(body, "No leave booked.") || !strings.Contains(body, "Nothing upcoming.") ||
		!strings.Contains(body, "Past (1)") {
		t.Fatalf("a finished leave is past, not next:\n%s", body)
	}

	today := hs.apply(staff, "annual", "2026-10-05", "2026-10-05")
	hs.approveAs(head, today)
	_, body = hs.get(staff, "/")
	if !strings.Contains(body, "On leave now, until Mon 5 Oct 2026.") || !strings.Contains(body, "Past (1)") ||
		!strings.Contains(body, "<h3>Upcoming</h3>") {
		t.Fatalf("leave ending today is current and upcoming:\n%s", body)
	}

	later := hs.apply(staff, "annual", "2026-10-12", "2026-10-13")
	hs.approveAs(head, later)
	pending := hs.apply(staff, "annual", "2026-10-07", "2026-10-07")
	_, body = hs.get(staff, "/")
	// The current leave is still the next one until it is over.
	if !strings.Contains(body, "On leave now") || strings.Contains(body, "Starts in") ||
		!strings.Contains(body, `<a href="`+pending+`">1 request waiting for a decision`) {
		t.Fatalf("one pending, current leave first:\n%s", body)
	}
	if a, b := strings.Index(body, "Wed 7 Oct 2026"), strings.Index(body, "Mon 12 Oct 2026"); a < 0 || b < 0 || a > b {
		t.Fatalf("upcoming is not soonest first:\n%s", body)
	}

	// With today's leave gone, the next is the 12th, in 6 days.
	hs.svc.Now = func() time.Time { return time.Date(2026, 10, 6, 8, 0, 0, 0, time.UTC) }
	_, body = hs.get(staff, "/")
	if !strings.Contains(body, "Starts in 6 days.") || !strings.Contains(body, "Past (2)") {
		t.Fatalf("next leave after today's ended:\n%s", body)
	}
}

func TestNewHomeDataAnnual(t *testing.T) {
	today := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	five := 5
	annual := func(a *int) leave.Balance {
		return leave.Balance{Type: leave.Type{Code: leave.AnnualCode}, Allowance: a}
	}
	if d := newHomeData([]leave.Balance{annual(&five)}, nil, today); d.Annual == nil || *d.Annual != 5 {
		t.Errorf("annual = %v, want 5", d.Annual)
	}
	if d := newHomeData([]leave.Balance{annual(nil)}, nil, today); d.Annual != nil {
		t.Errorf("unlimited annual shows %d", *d.Annual)
	}
	other := leave.Balance{Type: leave.Type{Code: "sick"}, Allowance: &five}
	if d := newHomeData([]leave.Balance{other}, nil, today); d.Annual != nil {
		t.Errorf("a missing annual type shows %d", *d.Annual)
	}
	rs := []leave.Request{
		{ID: 4, Status: leave.StatusDraft, Start: "2026-11-01", End: "2026-11-01"},
		{ID: 3, Status: leave.StatusDeclined, Start: "2026-10-20", End: "2026-10-20"},
		{ID: 2, Status: leave.StatusAwaitingHR, Start: "2026-10-10", End: "2026-10-10"},
		{ID: 1, Status: leave.StatusPending, Start: "2026-10-05", End: "2026-10-05"},
	}
	d := newHomeData(nil, rs, today)
	if len(d.Upcoming) != 2 || len(d.Past) != 1 || d.Pending != 2 || d.PendingFirst != 1 || d.Next != nil {
		t.Errorf("split = %d upcoming, %d past, %d pending, next %v", len(d.Upcoming), len(d.Past), d.Pending, d.Next)
	}
}

func (hs *harness) clock(y int, m time.Month, d int) {
	hs.svc.Now = func() time.Time { return time.Date(y, m, d, 8, 0, 0, 0, time.UTC) }
}

func TestHomeStartsTomorrowAndWeekend(t *testing.T) {
	hs := newHarness(t)
	staff, head := hs.signIn("staff1"), hs.signIn("headops")
	hs.approveAs(head, hs.apply(staff, "annual", "2026-10-06", "2026-10-06"))
	if _, body := hs.get(staff, "/"); !strings.Contains(body, "Starts tomorrow.") {
		t.Fatalf("starts tomorrow:\n%s", body)
	}
	// Friday to Monday, seen on the Saturday: the leave has started.
	hs.approveAs(head, hs.apply(staff, "annual", "2026-10-09", "2026-10-12"))
	hs.clock(2026, time.October, 10)
	if _, body := hs.get(staff, "/"); !strings.Contains(body, "On leave now, until Mon 12 Oct 2026.") {
		t.Fatalf("weekend-spanning leave:\n%s", body)
	}
}

// An approved cancellation is finished business: it is Past, and the leave it
// cancelled is no longer next.
func TestHomeCancelledLeaveIsNotUpcoming(t *testing.T) {
	hs := newHarness(t)
	staff, head := hs.signIn("staff1"), hs.signIn("headops")
	path := hs.apply(staff, "annual", "2026-10-12", "2026-10-13")
	hs.approveAs(head, path)
	res, _ := hs.post(staff, path+"/cancel", url.Values{"note": {"plans changed"}})
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("cancel: %d", res.StatusCode)
	}
	cpath, _, _ := strings.Cut(res.Header.Get("Location"), "?")
	hs.approveAs(head, cpath)
	_, body := hs.get(staff, "/")
	if !strings.Contains(body, "No leave booked.") || !strings.Contains(body, "Nothing upcoming.") || !strings.Contains(body, "Past (2)") {
		t.Fatalf("cancelled leave:\n%s", body)
	}
}

// Backdated requests are normal: one still waiting stays Upcoming and counted
// even though its dates have gone.
func TestHomeBackdatedPendingStaysUpcoming(t *testing.T) {
	hs := newHarness(t)
	staff := hs.signIn("staff1")
	path := hs.apply(staff, "annual", "2026-09-28", "2026-09-29")
	_, body := hs.get(staff, "/")
	if !strings.Contains(body, `<a href="`+path+`">1 request waiting for a decision`) || !strings.Contains(body, "<h3>Upcoming</h3>") ||
		strings.Contains(body, "Past (") {
		t.Fatalf("backdated pending:\n%s", body)
	}
}

func TestHomeAnnualNegativeAndInactive(t *testing.T) {
	hs := newHarness(t)
	staff := hs.signIn("staff1")
	hr, _ := hs.us.ByUsername(context.Background(), "hr")
	id := hs.staffID("staff1")
	tid, _ := strconv.ParseInt(hs.typeID("annual"), 10, 64)
	err := hs.svc.AddAdjustment(context.Background(), hr, leave.Adjustment{UserID: id, TypeID: tid, CycleStart: "2026-01-01", Days: -17, Reason: "test"})
	if err != nil {
		t.Fatal(err)
	}
	_, body := hs.get(staff, "/")
	if !strings.Contains(body, "<strong>-2</strong>") || !strings.Contains(body, "2 days below zero for annual leave") ||
		strings.Contains(body, "annual leave left") {
		t.Fatalf("negative annual:\n%s", body)
	}
	at, err := hs.svc.Type(context.Background(), tid)
	if err != nil {
		t.Fatal(err)
	}
	at.Active = false
	if _, err := hs.svc.SaveType(context.Background(), at); err != nil {
		t.Fatal(err)
	}
	if _, body := hs.get(staff, "/"); strings.Contains(body, "big-number") {
		t.Fatalf("inactive annual shows a number:\n%s", body)
	}
}

func TestQuoteListsUnchargedDays(t *testing.T) {
	hs := newHarness(t)
	c := hs.signIn("staff1")
	// Monday to Friday over the Day of Reconciliation (Wed 16 Dec), plus a weekend.
	_, body := hs.get(c, fmt.Sprintf("/leave/quote?type=%s&start=2026-12-14&end=2026-12-20", hs.typeID("annual")))
	for _, want := range []string{"4 working days", "Wed 16 Dec: Day of Reconciliation", "Saturday and Sunday are not working days"} {
		if !strings.Contains(body, want) {
			t.Errorf("quote lacks %q:\n%s", want, body)
		}
	}
	_, body = hs.get(c, fmt.Sprintf("/leave/quote?type=%s&start=2026-10-12&end=2026-10-16", hs.typeID("annual")))
	if !strings.Contains(body, "5 working days") || strings.Contains(body, "Not charged") {
		t.Errorf("clean week lists uncharged days:\n%s", body)
	}
}

func TestApplyTypeLabelsShowWhatIsLeft(t *testing.T) {
	hs := newHarness(t)
	c := hs.signIn("staff1")
	ctx := context.Background()
	annual, err := hs.svc.Type(ctx, mustAtoi(t, hs.typeID("annual")))
	if err != nil {
		t.Fatal(err)
	}
	_, body := hs.get(c, "/leave/new")
	if !strings.Contains(body, ">Annual leave (15 left this year)</option>") || !strings.Contains(body, ">Sick leave (30 left this cycle)</option>") {
		t.Errorf("annual and 36-month labels:\n%s", body)
	}
	if !strings.Contains(body, ">Unpaid leave</option>") {
		t.Errorf("unlimited type should be just its name:\n%s", body)
	}
	// Overdrawn by 2 reads as 0: nothing more can be requested either way.
	hr, err := hs.us.ByUsername(ctx, "hr")
	if err != nil {
		t.Fatal(err)
	}
	if err := hs.svc.AddAdjustment(ctx, hr, leave.Adjustment{UserID: hs.staffID("staff1"), TypeID: annual.ID, CycleStart: "2026-01-01", Days: -17, Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	weekend, err := hs.svc.Type(ctx, mustAtoi(t, hs.anyTypeID("weekend")))
	if err != nil {
		t.Fatal(err)
	}
	two := 2
	weekend.Active, weekend.Allowance = true, &two
	if _, err := hs.svc.SaveType(ctx, weekend); err != nil {
		t.Fatal(err)
	}
	_, body = hs.get(c, "/leave/new")
	if !strings.Contains(body, ">Annual leave (0 left this year)</option>") || !strings.Contains(body, ">Weekends off (2 weekends left)</option>") {
		t.Errorf("exhausted and weekend labels:\n%s", body)
	}
}

// In December the select counts this year's balance, so a quote for January
// must say it is measuring next year's, or the two would seem to disagree.
func TestQuoteNamesAnotherCycle(t *testing.T) {
	hs := newHarness(t)
	hs.clock(2026, time.December, 1)
	c := hs.signIn("staff1")
	ctx := context.Background()
	hr, err := hs.us.ByUsername(ctx, "hr")
	if err != nil {
		t.Fatal(err)
	}
	tid := mustAtoi(t, hs.typeID("annual"))
	if err := hs.svc.AddAdjustment(ctx, hr, leave.Adjustment{UserID: hs.staffID("staff1"), TypeID: tid, CycleStart: "2026-01-01", Days: -12, Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	_, body := hs.get(c, "/leave/new")
	if !strings.Contains(body, ">Annual leave (3 left this year)</option>") {
		t.Errorf("select:\n%s", body)
	}
	_, body = hs.get(c, "/leave/quote?type="+hs.typeID("annual")+"&start=2027-01-04&end=2027-01-08")
	if !strings.Contains(body, "10 left in 2027 afterwards") {
		t.Errorf("January quote:\n%s", body)
	}
	_, body = hs.get(c, "/leave/quote?type="+hs.typeID("annual")+"&start=2026-12-07&end=2026-12-08")
	if !strings.Contains(body, "1 left in this cycle afterwards") {
		t.Errorf("December quote:\n%s", body)
	}
}
