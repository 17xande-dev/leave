package notify

import (
	"strings"
	"testing"

	"github.com/17xande-dev/leave/internal/leave"
)

const privateNote = "SECRET-PRIVATE-NOTE"

// sentTo is who the drained emails went to, and the raw payload of each.
func sentTo(t *testing.T, jobs []job) map[string]string {
	t.Helper()
	return recipients(t, jobs)
}

// Whoever creates a delegation cannot use it on their own leave, so the
// stand-in must not be emailed the details (private note included) of that
// leave: an admin names someone as a stand-in for the head, then applies for
// leave themselves, and the head's stand-in is not told.
func TestStandInIsNotToldOfTheDelegationCreatorsLeave(t *testing.T) {
	// hr is an admin who reports to head; pastor is named as head's stand-in
	// by hr, hr2 acts on hr's requests (admins cannot correct their own).
	own := func(t *testing.T, e *env) leave.Request {
		t.Helper()
		r, err := e.svc.Submit(e.ctx, e.people["hr"], leave.Input{TypeID: e.annual, Start: "2026-10-12", End: "2026-10-14", Note: privateNote})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	for _, tc := range []struct {
		name string
		do   func(t *testing.T, e *env) (stand string)
	}{
		{"submitted", func(t *testing.T, e *env) string {
			if _, err := e.svc.Delegate(e.ctx, e.people["hr"], e.people["head"].ID, e.people["pastor"].ID, "2026-10-05", "2026-10-09", ""); err != nil {
				t.Fatal(err)
			}
			e.drain(t)
			own(t, e)
			return "pastor@x.test"
		}},
		{"edited", func(t *testing.T, e *env) string {
			if _, err := e.svc.Delegate(e.ctx, e.people["hr"], e.people["head"].ID, e.people["pastor"].ID, "2026-10-05", "2026-10-09", ""); err != nil {
				t.Fatal(err)
			}
			r := own(t, e)
			e.drain(t)
			if _, err := e.svc.Edit(e.ctx, e.people["hr"], r.ID, leave.Input{TypeID: e.annual, Start: "2026-10-19", End: "2026-10-21", Note: privateNote}); err != nil {
				t.Fatal(err)
			}
			return "pastor@x.test"
		}},
		{"corrected", func(t *testing.T, e *env) string {
			if _, err := e.svc.Delegate(e.ctx, e.people["hr"], e.people["head"].ID, e.people["pastor"].ID, "2026-10-05", "2026-10-09", ""); err != nil {
				t.Fatal(err)
			}
			r := own(t, e)
			e.drain(t)
			if _, err := e.svc.Correct(e.ctx, e.people["hr2"], r.ID,
				leave.Input{TypeID: e.typeID(t, "compassionate"), Start: "2026-10-13", End: "2026-10-14", Note: privateNote}, "It was a bereavement", ""); err != nil {
				t.Fatal(err)
			}
			return "pastor@x.test"
		}},
		{"halves changed", func(t *testing.T, e *env) string {
			if _, err := e.svc.Delegate(e.ctx, e.people["hr"], e.people["head"].ID, e.people["pastor"].ID, "2026-10-05", "2026-10-09", ""); err != nil {
				t.Fatal(err)
			}
			r := own(t, e)
			e.drain(t)
			if _, err := e.svc.SetHalves(e.ctx, e.people["hr2"], r.ID, true, false); err != nil {
				t.Fatal(err)
			}
			return "pastor@x.test"
		}},
		{"reassigned", func(t *testing.T, e *env) string {
			// hr names pastor to stand in for hr2, then hr's own request is
			// moved to hr2 (who has no address in the fixture, so give them
			// one: the scenario must still send something to show it ran).
			if _, err := e.db.Exec(`UPDATE users SET email = 'hr2@x.test' WHERE id = ?`, e.people["hr2"].ID); err != nil {
				t.Fatal(err)
			}
			if _, err := e.svc.Delegate(e.ctx, e.people["hr"], e.people["hr2"].ID, e.people["pastor"].ID, "2026-10-05", "2026-10-09", ""); err != nil {
				t.Fatal(err)
			}
			r := own(t, e)
			e.drain(t)
			if _, err := e.svc.Reassign(e.ctx, e.people["hr2"], r.ID, e.people["hr2"].ID); err != nil {
				t.Fatal(err)
			}
			return "pastor@x.test"
		}},
		{"escalated", func(t *testing.T, e *env) string {
			// hr and staff cover each other; hr's overlapping request needs
			// special permission, and when head approves it, it moves to the
			// pastor, whose stand-in (named by hr) is staff.
			e.dept(t, "Pair", "staff", "hr")
			if err := e.svc.SetCoverageRule(e.ctx, "Pair", 50); err != nil {
				t.Fatal(err)
			}
			if _, err := e.svc.Submit(e.ctx, e.people["staff"], leave.Input{TypeID: e.annual, Start: "2026-10-12", End: "2026-10-14"}); err != nil {
				t.Fatal(err)
			}
			r, err := e.svc.Submit(e.ctx, e.people["hr"], leave.Input{TypeID: e.annual, Start: "2026-10-12", End: "2026-10-14", Note: privateNote, Override: true})
			if err != nil || !r.Escalate {
				t.Fatalf("setup: %+v %v", r, err)
			}
			if _, err := e.svc.Delegate(e.ctx, e.people["hr"], e.people["pastor"].ID, e.people["staff"].ID, "2026-10-05", "2026-10-09", ""); err != nil {
				t.Fatal(err)
			}
			e.drain(t)
			if got, err := e.svc.Decide(e.ctx, e.people["head"], r.ID, true, ""); err != nil || got.ApproverID != e.people["pastor"].ID {
				t.Fatalf("escalation: %+v %v", got, err)
			}
			return "staff@x.test"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t)
			stand := tc.do(t, e)
			jobs := e.drain(t)
			got := sentTo(t, jobs)
			if len(got) == 0 {
				t.Fatal("nobody was emailed at all: the scenario did not reach its event")
			}
			if text, told := got[stand]; told {
				t.Fatalf("%s was emailed the creator's own leave:\n%s", stand, text)
			}
			for _, j := range jobs {
				if strings.Contains(j.payload, stand) {
					t.Fatalf("an outbox row mentions %s: %+v", stand, j)
				}
			}
		})
	}
}

// The same events do reach a stand-in named by someone else: the guard only
// removes the creator's own leave.
func TestStandInIsToldOfAnOrdinaryRequestFromAnAdminWhoDidNotCreateIt(t *testing.T) {
	e := setup(t)
	// head names pastor themselves, and hr applies for leave: hr did not
	// create the delegation, so the stand-in hears.
	if _, err := e.svc.Delegate(e.ctx, e.people["head"], e.people["head"].ID, e.people["pastor"].ID, "2026-10-05", "2026-10-09", ""); err != nil {
		t.Fatal(err)
	}
	e.drain(t)
	if _, err := e.svc.Submit(e.ctx, e.people["hr"], leave.Input{TypeID: e.annual, Start: "2026-10-12", End: "2026-10-14", Note: privateNote}); err != nil {
		t.Fatal(err)
	}
	got := sentTo(t, e.drain(t))
	if !strings.Contains(got["pastor@x.test"], "You are approving for Olivia Head") || !strings.Contains(got["head@x.test"], "Hannah HR has applied") {
		t.Fatalf("recipients = %v", got)
	}
}

// approversAndStandIn refuses on its own, whatever the event carries: the
// second line of defence behind leave.emit.
func TestApproversAndStandInRefusesTheCreatorsLeave(t *testing.T) {
	e := setup(t)
	d, err := e.svc.Delegate(e.ctx, e.people["hr"], e.people["head"].ID, e.people["pastor"].ID, "2026-10-05", "2026-10-09", "")
	if err != nil {
		t.Fatal(err)
	}
	r, err := e.svc.Submit(e.ctx, e.people["hr"], leave.Input{TypeID: e.annual, Start: "2026-10-12", End: "2026-10-14"})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := e.db.BeginTx(e.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	// Handed the delegation directly, as if emit had not filtered it.
	to, err := approversAndStandIn(e.ctx, tx, leave.Event{Kind: leave.EventSubmitted, Request: r, StandIn: &d})
	if err != nil {
		t.Fatal(err)
	}
	if len(to) != 1 || to[0].Email != "head@x.test" {
		t.Fatalf("recipients = %+v", to)
	}
}
