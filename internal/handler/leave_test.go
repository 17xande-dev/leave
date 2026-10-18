package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
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
