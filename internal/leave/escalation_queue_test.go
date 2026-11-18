package leave

import (
	"fmt"
	"testing"

	"github.com/17xande-dev/leave/internal/directory"
)

// queueRequest is an escalated request, approved by head, then put in the
// admins' queue by an admin (the reviewer's scenario): it has been through one
// level and no longer has an approver.
func (f *fixture) queueRequest() Request {
	f.t.Helper()
	r := f.climbRequest()
	r = f.approve("head", r.ID)
	f.wantPendingWith(r, "pastor")
	r, err := f.svc.Reassign(f.ctx, f.people["hr"], r.ID, 0)
	if err != nil || r.ApproverID != 0 {
		f.t.Fatalf("reassign to the queue = %+v %v", r, err)
	}
	return r
}

// One admin's approval from the queue must not finish a request needing
// special permission while the final approver has yet to approve it: it goes
// to them, and only their approval finishes it.
func TestEscalationQueueAdminApprovalGoesToFinalApprover(t *testing.T) {
	f := newFixture(t)
	r := f.queueRequest()
	r = f.approve("hr", r.ID)
	f.wantPendingWith(r, "pastor")
	if r = f.approve("pastor", r.ID); r.Status != StatusApproved || r.DecidedByID != f.people["pastor"].ID {
		t.Fatalf("pastor's approval = %+v", r)
	}
}

// The final approver is an admin and takes it from the queue: that is the
// final approval, nothing is left to hand on.
func TestEscalationQueueFinalApproverFinishes(t *testing.T) {
	f := newFixture(t)
	f.makeAdmin("pastor")
	r := f.queueRequest()
	if r = f.approve("pastor", r.ID); r.Status != StatusApproved {
		t.Fatalf("pastor's approval from the queue = %+v", r)
	}
}

// Nothing to hand on to: the final approver is disabled, has already approved
// (in either capacity), or is the requester -- an admin finishes it as before.
func TestEscalationQueueWithoutUsableFinalApproverFinishes(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(f *fixture, r Request)
	}{
		{"disabled", func(f *fixture, _ Request) {
			f.exec(`UPDATE users SET disabled = 1 WHERE id = ?`, f.people["pastor"].ID)
		}},
		{"already approved", func(f *fixture, r Request) {
			f.exec(`INSERT INTO request_steps (request_id, approver_id, decision, note, decided_at)
				VALUES (?, ?, 'approved', '', '2026-10-05T08:00:00Z')`, r.ID, f.people["pastor"].ID)
		}},
		{"approved as a stand-in", func(f *fixture, r Request) {
			f.exec(`INSERT INTO request_steps (request_id, approver_id, decision, note, decided_at, on_behalf_of_id)
				VALUES (?, ?, 'approved', '', '2026-10-05T08:00:00Z', ?)`, r.ID, f.people["pastor"].ID, f.people["head"].ID)
		}},
		{"is the requester", func(f *fixture, _ Request) {
			f.exec(`UPDATE settings SET final_approver_id = ?`, f.people["staff"].ID)
		}},
		{"none set", func(f *fixture, _ Request) {
			f.exec(`UPDATE settings SET final_approver_id = NULL`)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			r := f.queueRequest()
			tc.setup(f, r)
			if r = f.approve("hr", r.ID); r.Status != StatusApproved {
				t.Fatalf("hr's approval = %+v", r)
			}
		})
	}
}

// The ordinary queue is untouched: a request that never needed special
// permission is finished by one admin's approval.
func TestQueueOrdinaryRequestStillFinishedByOneAdmin(t *testing.T) {
	f := newFixture(t)
	r := f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-12")
	r, err := f.svc.Reassign(f.ctx, f.people["hr"], r.ID, 0)
	if err != nil || r.ApproverID != 0 {
		t.Fatalf("reassign = %+v %v", r, err)
	}
	f.makeAdmin("pastor")
	if r = f.approve("pastor", r.ID); r.Status != StatusApproved {
		t.Fatalf("approval = %+v", r)
	}
}

// The climb has a cap against a directory loop or an absurdly deep chain. When
// it runs out with more than ten levels already through the request, the
// request still goes to the final approver rather than finishing without them.
func TestEscalationClimbCapHandsToFinalApprover(t *testing.T) {
	f := newFixture(t)
	r := f.climbRequest()
	dn := func(n string) string { return "CN=" + n + ",OU=Staff" }
	up := func(name, mgr string) {
		u, err := f.us.Upsert(f.ctx, directory.User{ID: dn(name), DN: dn(name), Username: name, Name: name,
			Email: name + "@example.test", ManagerDN: dn(mgr)})
		if err != nil {
			t.Fatal(err)
		}
		f.people[name] = u
	}
	// head -> p11 -> p10 -> ... -> p0 -> pastor, twelve levels above head.
	up("p0", "pastor")
	for i := 1; i <= 11; i++ {
		up(fmt.Sprintf("p%d", i), fmt.Sprintf("p%d", i-1))
	}
	up("head", "p11")
	for i := 0; i <= 11; i++ {
		f.exec(`INSERT INTO request_steps (request_id, approver_id, decision, note, decided_at)
			VALUES (?, ?, 'approved', '', '2026-10-05T08:00:00Z')`, r.ID, f.people[fmt.Sprintf("p%d", i)].ID)
	}
	r = f.approve("head", r.ID)
	f.wantPendingWith(r, "pastor")
	if r = f.approve("pastor", r.ID); r.Status != StatusApproved {
		t.Fatalf("pastor's approval = %+v", r)
	}
}

// makeAdmin promotes someone and refreshes the fixture's copy, which approve
// acts with.
func (f *fixture) makeAdmin(who string) {
	f.t.Helper()
	f.exec(`UPDATE users SET is_admin = 1 WHERE id = ?`, f.people[who].ID)
	u, err := f.us.ByID(f.ctx, f.people[who].ID)
	if err != nil {
		f.t.Fatal(err)
	}
	f.people[who] = u
}
