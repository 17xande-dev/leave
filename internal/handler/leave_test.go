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
// type's own. "Today" is pinned to 5 October 2026, so 90 days back is 7 July.
func TestApplyPageCarriesBackdateMin(t *testing.T) {
	hs := newHarness(t)
	c := hs.signIn("staff1")
	ts, _ := hs.svc.Types(context.Background(), true)
	annual, _ := hs.svc.Type(context.Background(), mustAtoi(t, hs.typeID("annual")))
	sick, _ := hs.svc.Type(context.Background(), mustAtoi(t, hs.typeID("sick")))
	annual.BackdateDays, sick.BackdateDays = 10, 90
	for _, ty := range []leave.Type{annual, sick} {
		if _, err := hs.svc.SaveType(context.Background(), ty); err != nil {
			t.Fatal(err)
		}
	}

	_, body := hs.get(c, "/leave/new")
	n := 0
	for _, ty := range ts {
		want := map[string]string{"annual": "2026-09-25", "sick": "2026-07-07"}[ty.Code]
		if want == "" {
			want = "2026-07-07" // the other seeded types
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
	if !strings.Contains(body, `months="2" first-day-of-week="1" with-clear locale="en-GB" min="2026-07-07"`) {
		t.Errorf("picker lacks the earliest min:\n%s", body)
	}
	for _, id := range []string{"start", "end"} {
		if !regexp.MustCompile(`<input type="date" id="` + id + `"[^>]* min="2026-07-07"`).MatchString(body) {
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
