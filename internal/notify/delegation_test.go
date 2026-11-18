package notify

import (
	"strings"
	"testing"
	"time"

	"github.com/17xande-dev/leave/internal/leave"
)

func recipients(t *testing.T, jobs []job) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, j := range jobs {
		if j.kind != "email" {
			continue
		}
		m := email(t, j)
		out[m.To[0]] = m.Text
	}
	return out
}

// While a delegation is active the stand-in hears of what waits for the
// approver, with a line saying whom they are approving for; outside it nobody
// extra does.
func TestStandInIsToldWhileActive(t *testing.T) {
	for _, tc := range []struct {
		name       string
		start, end string
		told       bool
		until      string
	}{
		{"active", "2026-10-05", "2026-10-09", true, "Fri 9 Oct 2026"},
		{"last day", "2026-10-02", "2026-10-05", true, "Mon 5 Oct 2026"},
		{"not started", "2026-10-06", "2026-10-09", false, ""},
		{"over", "2026-10-02", "2026-10-04", false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t)
			// Created a few days before "today" (5 October), as a real one would be.
			e.svc.Now = func() time.Time { return time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC) }
			if _, err := e.svc.Delegate(e.ctx, e.people["head"], e.people["head"].ID, e.people["hr"].ID, tc.start, tc.end, ""); err != nil {
				t.Fatal(err)
			}
			e.svc.Now = func() time.Time { return time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC) }
			e.drain(t)
			if _, err := e.svc.Submit(e.ctx, e.people["staff"], leave.Input{TypeID: e.annual, Start: "2026-10-12", End: "2026-10-12"}); err != nil {
				t.Fatal(err)
			}
			got := recipients(t, e.drain(t))
			if !strings.Contains(got["head@x.test"], "Sam Staff has applied") || strings.Contains(got["head@x.test"], "You are approving for") {
				t.Fatalf("the approver's copy: %q", got["head@x.test"])
			}
			if !tc.told {
				if len(got) != 1 {
					t.Fatalf("recipients = %v", got)
				}
				return
			}
			if len(got) != 2 || !strings.Contains(got["hr@x.test"], "Sam Staff has applied") ||
				!strings.Contains(got["hr@x.test"], "You are approving for Olivia Head until "+tc.until+".") {
				t.Fatalf("recipients = %v", got)
			}
		})
	}
}

// The stand-in is also told of changes to what they may decide, and never of
// their own request.
func TestStandInHearsOfChangesAndNotOwnRequest(t *testing.T) {
	e := setup(t)
	if _, err := e.svc.Delegate(e.ctx, e.people["head"], e.people["head"].ID, e.people["hr"].ID, "2026-10-05", "2026-10-09", ""); err != nil {
		t.Fatal(err)
	}
	r, err := e.svc.Submit(e.ctx, e.people["staff"], leave.Input{TypeID: e.annual, Start: "2026-10-12", End: "2026-10-12"})
	if err != nil {
		t.Fatal(err)
	}
	e.drain(t)
	// The requester edits it: both hear.
	if _, err := e.svc.Edit(e.ctx, e.people["staff"], r.ID, leave.Input{TypeID: e.annual, Start: "2026-10-13", End: "2026-10-13"}); err != nil {
		t.Fatal(err)
	}
	got := recipients(t, e.drain(t))
	if len(got) != 2 || !strings.Contains(got["hr@x.test"], "You are approving for Olivia Head until Fri 9 Oct 2026.") {
		t.Fatalf("edited recipients = %v", got)
	}
	// An admin moves it to another approver: the new one hears; the old
	// approver's stand-in is no longer involved.
	if _, err := e.svc.Reassign(e.ctx, e.people["hr"], r.ID, e.people["pastor"].ID); err == nil {
		t.Fatal("the fixture's pastor is not an administrator")
	}
	if _, err := e.svc.Reassign(e.ctx, e.people["hr"], r.ID, e.people["hr"].ID); err != nil {
		t.Fatal(err)
	}
	got = recipients(t, e.drain(t))
	if len(got) != 1 || got["hr@x.test"] == "" || strings.Contains(got["hr@x.test"], "You are approving for") {
		t.Fatalf("reassigned recipients = %v", got)
	}
	// The stand-in's own request, waiting with head, is not sent to them.
	if _, err := e.svc.Submit(e.ctx, e.people["hr"], leave.Input{TypeID: e.annual, Start: "2026-10-14", End: "2026-10-14"}); err != nil {
		t.Fatal(err)
	}
	got = recipients(t, e.drain(t))
	if len(got) != 1 || got["head@x.test"] == "" {
		t.Fatalf("own-request recipients = %v", got)
	}
}

func TestDelegationEmails(t *testing.T) {
	e := setup(t)
	// The approver sets it up themselves: the stand-in is told, nobody else.
	d, err := e.svc.Delegate(e.ctx, e.people["head"], e.people["head"].ID, e.people["hr"].ID, "2026-10-12", "2026-10-16", "Back on the 19th")
	if err != nil {
		t.Fatal(err)
	}
	jobs := e.drain(t)
	if len(jobs) != 1 {
		t.Fatalf("jobs = %+v", jobs)
	}
	m := email(t, jobs[0])
	for _, want := range []string{"Hannah", "Olivia Head has asked you to approve leave requests for them", "Mon 12 Oct – Fri 16 Oct 2026",
		"Note: Back on the 19th", "https://leave.test/approvals"} {
		if !strings.Contains(m.Text, want) {
			t.Errorf("missing %q in:\n%s", want, m.Text)
		}
	}
	if m.To[0] != "hr@x.test" || !strings.Contains(m.Subject, "Olivia Head") {
		t.Fatalf("message = %+v", m)
	}

	// An admin revokes it: both hear.
	if _, err := e.svc.RevokeDelegation(e.ctx, e.people["hr"], d.ID); err != nil {
		t.Fatal(err)
	}
	got := recipients(t, e.drain(t))
	if len(got) != 2 || !strings.Contains(got["hr@x.test"], "Hannah HR has ended the arrangement for you to approve leave requests for Olivia Head") ||
		!strings.Contains(got["head@x.test"], "Hannah HR has ended the arrangement for Hannah HR to approve leave requests for you") {
		t.Fatalf("revoked emails = %v", got)
	}

	// An admin sets one up for someone: the stand-in and the approver hear.
	d2, err := e.svc.Delegate(e.ctx, e.people["hr"], e.people["head"].ID, e.people["pastor"].ID, "2026-10-12", "2026-10-12", "")
	if err != nil {
		t.Fatal(err)
	}
	got = recipients(t, e.drain(t))
	if len(got) != 2 || !strings.Contains(got["pastor@x.test"], "Hannah HR has asked you to approve leave requests for Olivia Head, covering Mon 12 Oct 2026") ||
		!strings.Contains(got["head@x.test"], "Hannah HR has named Senior Pastor to approve leave requests for you") {
		t.Fatalf("delegated emails = %v", got)
	}
	// The approver ends it themselves: only the stand-in hears.
	if _, err := e.svc.RevokeDelegation(e.ctx, e.people["head"], d2.ID); err != nil {
		t.Fatal(err)
	}
	got = recipients(t, e.drain(t))
	if len(got) != 1 || !strings.Contains(got["pastor@x.test"], "Olivia Head has ended the arrangement") {
		t.Fatalf("revoked by the approver = %v", got)
	}
}

// "Approved by B for A" in the person's email.
func TestDecidedEmailNamesTheApprover(t *testing.T) {
	e := setup(t)
	if _, err := e.svc.Delegate(e.ctx, e.people["head"], e.people["head"].ID, e.people["hr"].ID, "2026-10-05", "2026-10-09", ""); err != nil {
		t.Fatal(err)
	}
	r, err := e.svc.Submit(e.ctx, e.people["staff"], leave.Input{TypeID: e.annual, Start: "2026-10-12", End: "2026-10-12"})
	if err != nil {
		t.Fatal(err)
	}
	e.drain(t)
	if _, err := e.svc.Decide(e.ctx, e.people["hr"], r.ID, true, ""); err != nil {
		t.Fatal(err)
	}
	got := recipients(t, e.drain(t))
	if !strings.Contains(got["staff@x.test"], "was approved by Hannah HR for Olivia Head.") {
		t.Fatalf("decided email = %q", got["staff@x.test"])
	}
	// Directly, no "for".
	r2, err := e.svc.Submit(e.ctx, e.people["staff"], leave.Input{TypeID: e.annual, Start: "2026-10-13", End: "2026-10-13"})
	if err != nil {
		t.Fatal(err)
	}
	e.drain(t)
	if _, err := e.svc.Decide(e.ctx, e.people["head"], r2.ID, true, ""); err != nil {
		t.Fatal(err)
	}
	got = recipients(t, e.drain(t))
	if !strings.Contains(got["staff@x.test"], "was approved by Olivia Head.") {
		t.Fatalf("direct decided email = %q", got["staff@x.test"])
	}
}
