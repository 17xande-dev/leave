package handler

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

// delegate has the delegator's stand-in set up through the service, as the
// page for it (a later phase) will.
func (hs *harness) delegate(delegator, delegate, start, end string) {
	hs.t.Helper()
	d, s := mustUser(hs.t, hs, delegator), mustUser(hs.t, hs, delegate)
	if _, err := hs.svc.Delegate(hs.t.Context(), d, d.ID, s.ID, start, end, ""); err != nil {
		hs.t.Fatal(err)
	}
}

// A stand-in sees what waits for the approver on the approvals page, can open
// it, approve it singly or in bulk, and the request page then says who decided
// for whom. When the delegation is over it is gone from their queue and a 404.
func TestStandInApprovesFromThePages(t *testing.T) {
	hs := newHarness(t)
	staff, hr := hs.signIn("staff1"), hs.signIn("hr")
	one := hs.apply(staff, "annual", "2026-10-12", "2026-10-14")
	two := hs.apply(hr, "annual", "2026-10-19", "2026-10-20")
	three := hs.apply(staff, "annual", "2026-11-02", "2026-11-02")
	kids := hs.signIn("headkids") // not an administrator, and not headops's manager

	if status, _ := hs.get(kids, one); status != http.StatusNotFound {
		t.Fatalf("before the delegation: %d", status)
	}
	hs.delegate("headops", "headkids", "2026-10-05", "2026-10-09")

	status, body := hs.get(kids, "/approvals")
	if status != http.StatusOK || !strings.Contains(body, `<a href="`+one+`?from=approvals">`) || !strings.Contains(body, `<a href="`+two+`?from=approvals">`) {
		t.Fatalf("the queue lacks the approver's requests:\n%s", body)
	}
	if status, body := hs.get(kids, one); status != http.StatusOK || !strings.Contains(body, "Decide") && !strings.Contains(body, "Approve") {
		t.Fatalf("request page: %d\n%s", status, body)
	}
	// The navigation badge counts them.
	if n, err := hs.svc.PendingCount(t.Context(), mustUser(t, hs, "headkids")); err != nil || n != 3 {
		t.Fatalf("badge = %d %v", n, err)
	}

	hs.approveAs(kids, one)
	res, _ := hs.post(kids, "/approvals/approve", url.Values{"id": {idOf(two)}})
	if res.StatusCode != http.StatusSeeOther || hs.status(one) != "approved" || hs.status(two) != "approved" {
		t.Fatalf("approving: %d %s %s", res.StatusCode, hs.status(one), hs.status(two))
	}
	// Anyone who can see it reads who decided for whom.
	_, body = hs.get(staff, one)
	if !strings.Contains(body, "Kagiso Kids for Olivia Operations") {
		t.Fatalf("request page lacks 'by B for A':\n%s", body)
	}
	// The stand-in decided it, so it stays openable to them afterwards.
	if status, _ := hs.get(kids, one); status != http.StatusOK {
		t.Fatalf("the stand-in lost their own decision: %d", status)
	}

	// Once the days are over: not in the queue, not openable, not decidable.
	hs.clock(2026, time.October, 10)
	if _, body := hs.get(kids, "/approvals"); strings.Contains(body, three) {
		t.Fatalf("still queued after the end:\n%s", body)
	}
	if status, _ := hs.get(kids, three); status != http.StatusNotFound {
		t.Fatalf("after the end: %d", status)
	}
	res, _ = hs.post(kids, three+"/decide", url.Values{"decision": {"approve"}, "from": {"queue"}})
	if hs.status(three) != "pending" || res.StatusCode == http.StatusSeeOther {
		t.Fatalf("decided after the end: %d %s", res.StatusCode, hs.status(three))
	}
	res, body = hs.post(kids, "/approvals/approve", url.Values{"id": {idOf(three)}})
	if res.StatusCode != http.StatusUnprocessableEntity || hs.status(three) != "pending" {
		t.Fatalf("bulk after the end: %d %s", res.StatusCode, hs.status(three))
	}
	// The approver still sees it, and the history says nothing of a stand-in
	// for the one they decided themselves.
	head := hs.signIn("headops")
	hs.approveAs(head, three)
	if _, body := hs.get(staff, three); strings.Contains(body, " for Olivia") {
		t.Fatalf("direct approval shown as for someone:\n%s", body)
	}
}

// An escalated request's history says who approved for whom.
func TestHistoryNamesTheApproverStoodInFor(t *testing.T) {
	hs := newHarness(t)
	hs.limitOperations()
	hs.apply(hs.signIn("headops"), "annual", "2026-10-12", "2026-10-14")
	staff := hs.signIn("staff1")
	res, _ := hs.post(staff, "/leave", hs.form("annual", "2026-10-12", "2026-10-14", url.Values{"override": {"1"}}))
	if res.StatusCode != http.StatusSeeOther {
		t.Fatalf("override: %d", res.StatusCode)
	}
	path, _, _ := strings.Cut(res.Header.Get("Location"), "?")
	// headops is away: hr stands in for them and approves the first level.
	hs.delegate("headops", "hr", "2026-10-05", "2026-10-09")
	hs.approveAs(hs.signIn("hr"), path)
	if _, body := hs.get(staff, path); !strings.Contains(body, "Approved (override) by Hannah HR for Olivia Operations") {
		t.Fatalf("history lacks 'for':\n%s", body)
	}
}
