package leave

import (
	"testing"

	"github.com/17xande-dev/leave/internal/directory"
)

// climbChain makes staff -> head -> boss [-> bigboss] -> pastor (the final
// approver, from newFixture).
func (f *fixture) climbChain(withBig bool) {
	f.t.Helper()
	up := func(name, mgr string) {
		dn := func(n string) string { return "CN=" + n + ",OU=Staff" }
		u, err := f.us.Upsert(f.ctx, directory.User{ID: dn(name), DN: dn(name), Username: name, Name: name,
			Email: name + "@example.test", ManagerDN: dn(mgr)})
		if err != nil {
			f.t.Fatal(err)
		}
		f.people[name] = u
	}
	if withBig {
		up("bigboss", "pastor")
		up("boss", "bigboss")
	} else {
		up("boss", "pastor")
	}
	up("head", "boss")
}

// climbRequest is an escalated request from staff, waiting with head.
func (f *fixture) climbRequest() Request {
	f.t.Helper()
	f.dept("Pair", "staff", "hr")
	f.rule("Pair", 50)
	f.mustSubmit("hr", "annual", "2026-10-12", "2026-10-14")
	r, err := f.override("staff", "annual", "2026-10-12", "2026-10-14")
	if err != nil || !r.Escalate || r.ApproverID != f.people["head"].ID {
		f.t.Fatalf("escalated request = %+v %v", r, err)
	}
	return r
}

func (f *fixture) approve(who string, id int64) Request {
	f.t.Helper()
	r, err := f.svc.Decide(f.ctx, f.people[who], id, true, "")
	if err != nil {
		f.t.Fatalf("%s approves: %v", who, err)
	}
	return r
}

func (f *fixture) wantPendingWith(r Request, who string) {
	f.t.Helper()
	if r.Status != StatusPending || r.ApproverID != f.people[who].ID {
		f.t.Fatalf("want pending with %s, got status %s approver %s (id %d)", who, r.Status, r.ApproverName, r.ApproverID)
	}
}

// The existing four-level walk: every level approves in turn, the final
// approver last.
func TestEscalationFourLevelsInOrder(t *testing.T) {
	f := newFixture(t)
	f.climbChain(true)
	r := f.climbRequest()
	for _, who := range []string{"head", "boss", "bigboss"} {
		r = f.approve(who, r.ID)
	}
	f.wantPendingWith(r, "pastor")
	if r = f.approve("pastor", r.ID); r.Status != StatusApproved {
		t.Fatalf("pastor's approval = %+v", r)
	}
}

// head's stand-in is bigboss, two levels up. bigboss's step counts for head's
// level and their own; boss, who sits between, still has to approve. When
// boss's next is bigboss -- already through the request -- the chain must carry
// on to pastor (the final approver), not finish with pastor never seeing it.
func TestEscalationStandInTwoUpStillReachesFinalApprover(t *testing.T) {
	f := newFixture(t)
	f.climbChain(true)
	r := f.climbRequest()
	f.mustDelegate("head", "bigboss", "2026-10-05", "2026-10-09")

	r = f.approve("bigboss", r.ID)
	f.wantPendingWith(r, "boss")
	r = f.approve("boss", r.ID)
	f.wantPendingWith(r, "pastor")
	if r = f.approve("pastor", r.ID); r.Status != StatusApproved || r.DecidedByID != f.people["pastor"].ID {
		t.Fatalf("pastor's approval = %+v", r)
	}
}

// The final approver stands in for head; head's next is boss, who is not the
// final approver. boss then approves. pastor has been through it, and is the
// final approver, so that finishes it -- with pastor in the trail, not
// finished by someone short of the final approver.
func TestEscalationFinalApproverStandsInThenBoss(t *testing.T) {
	f := newFixture(t)
	f.climbChain(false)
	r := f.climbRequest()
	f.mustDelegate("head", "pastor", "2026-10-05", "2026-10-09")

	r = f.approve("pastor", r.ID)
	f.wantPendingWith(r, "boss")
	if r = f.approve("boss", r.ID); r.Status != StatusApproved {
		t.Fatalf("after boss = %+v", r)
	}
	steps, err := f.svc.Steps(f.ctx, r.ID)
	if err != nil || len(steps) != 2 || steps[0].ApproverID != f.people["pastor"].ID || steps[0].OnBehalfOf != "head" ||
		steps[1].ApproverID != f.people["boss"].ID {
		t.Fatalf("trail = %+v %v", steps, err)
	}
}

// A directory loop among people already through the request cannot spin: head
// and boss each have the other as approver, neither is the final approver.
// The loop ends the climb, but a request needing special permission must
// still be finished by the final approver, so it goes to pastor rather than
// being approved without them (it used to finish here).
func TestEscalationDirectoryCycleTerminates(t *testing.T) {
	f := newFixture(t)
	f.climbChain(false)
	r := f.climbRequest()
	f.exec(`UPDATE users SET is_admin = 1 WHERE id = ?`, f.people["head"].ID)
	if err := f.svc.SetOverride(f.ctx, f.people["boss"].ID, f.people["head"].ID); err != nil {
		t.Fatal(err)
	}
	r = f.approve("head", r.ID)
	f.wantPendingWith(r, "boss")
	// boss's approver is head, who is through it; head's is boss: a cycle.
	r = f.approve("boss", r.ID)
	f.wantPendingWith(r, "pastor")
	if r = f.approve("pastor", r.ID); r.Status != StatusApproved {
		t.Fatalf("after pastor = %+v", r)
	}
}

// The final approver has someone above them (an override to an admin who is
// in nobody's chain), and is head's stand-in. Their one approval is the final
// one: the chain must not carry on to the person above them.
func TestEscalationFinalApproverWithSomeoneAboveStandingIn(t *testing.T) {
	f := newFixture(t)
	r := f.climbRequest()
	f.exec(`UPDATE users SET is_admin = 1 WHERE id = ?`, f.people["orphan"].ID)
	if err := f.svc.SetOverride(f.ctx, f.people["pastor"].ID, f.people["orphan"].ID); err != nil {
		t.Fatal(err)
	}
	f.mustDelegate("head", "pastor", "2026-10-05", "2026-10-09")

	next, ok, err := f.svc.NextApprover(f.ctx, f.people["pastor"], r)
	if err != nil || ok {
		t.Fatalf("pastor's approval should be last, next = %v %v %v", next.Username, ok, err)
	}
	r = f.approve("pastor", r.ID)
	if r.Status != StatusApproved || r.DecidedForID != f.people["head"].ID {
		t.Fatalf("pastor's approval = %+v", r)
	}
}
