package leave

import "testing"

// A request needing special permission must never finish without the final
// approver having approved it, as themselves or as a stand-in.

// grantedBy logs the step an admin leaves when they grant a negative balance
// before the request is submitted. It is not an approval of the request.
func (f *fixture) grantedBy(who string, id int64) {
	f.t.Helper()
	f.exec(`INSERT INTO request_steps (request_id, approver_id, decision, note, decided_at)
		VALUES (?, ?, 'granted', '', '2026-10-05T08:00:00Z')`, id, f.people[who].ID)
}

func (f *fixture) cycleBossHead() {
	f.t.Helper()
	f.exec(`UPDATE users SET is_admin = 1 WHERE id = ?`, f.people["head"].ID)
	if err := f.svc.SetOverride(f.ctx, f.people["boss"].ID, f.people["head"].ID); err != nil {
		f.t.Fatal(err)
	}
}

// pastor (the final approver) granted the negative balance; that must not stand
// for approval, or head's approval would finish it without pastor.
func TestEscalationGrantedByFinalApproverStillNeedsThem(t *testing.T) {
	f := newFixture(t)
	f.climbChain(false)
	r := f.climbRequest()
	f.grantedBy("pastor", r.ID)

	r = f.approve("head", r.ID)
	f.wantPendingWith(r, "boss")
	r = f.approve("boss", r.ID)
	f.wantPendingWith(r, "pastor")
	if r = f.approve("pastor", r.ID); r.Status != StatusApproved {
		t.Fatalf("pastor's approval = %+v", r)
	}
}

// A mid-chain boss granted it: their level must not be skipped.
func TestEscalationGrantedByMidChainBossStillNeedsThem(t *testing.T) {
	f := newFixture(t)
	f.climbChain(false)
	r := f.climbRequest()
	f.grantedBy("boss", r.ID)

	r = f.approve("head", r.ID)
	f.wantPendingWith(r, "boss")
	r = f.approve("boss", r.ID)
	f.wantPendingWith(r, "pastor")
}

// The cycle with a stand-in: pastor stands in for head, so boss, whose
// approver is head, ends the loop. pastor already approved (as stand-in), so
// that finishes it.
func TestEscalationCycleFinalApproverAlreadyStoodIn(t *testing.T) {
	f := newFixture(t)
	f.climbChain(false)
	r := f.climbRequest()
	f.cycleBossHead()
	f.mustDelegate("head", "pastor", "2026-10-05", "2026-10-09")

	r = f.approve("pastor", r.ID)
	f.wantPendingWith(r, "boss")
	if r = f.approve("boss", r.ID); r.Status != StatusApproved {
		t.Fatalf("after boss = %+v", r)
	}
	if steps, err := f.svc.Steps(f.ctx, r.ID); err != nil || steps[0].ApproverID != f.people["pastor"].ID {
		t.Fatalf("trail = %+v %v", steps, err)
	}
}

// The cycle with another stand-in: hr approves for head, boss ends the loop,
// and pastor has not approved, so it goes to them.
func TestEscalationCycleWithOtherStandInGoesToFinalApprover(t *testing.T) {
	f := newFixture(t)
	f.climbChain(false)
	r := f.climbRequest()
	f.cycleBossHead()
	f.mustDelegate("head", "hr", "2026-10-05", "2026-10-09")

	r = f.approve("hr", r.ID)
	f.wantPendingWith(r, "boss")
	r = f.approve("boss", r.ID)
	f.wantPendingWith(r, "pastor")
	if r = f.approve("pastor", r.ID); r.Status != StatusApproved {
		t.Fatalf("pastor's approval = %+v", r)
	}
}

// The final approver cannot be used: the loop ends the climb and the request
// finishes rather than sitting pending with nobody to decide it.
func TestEscalationCycleFinalApproverDisabledStillEnds(t *testing.T) {
	f := newFixture(t)
	f.climbChain(false)
	r := f.climbRequest()
	f.cycleBossHead()
	f.exec(`UPDATE users SET disabled = 1 WHERE id = ?`, f.people["pastor"].ID)

	r = f.approve("head", r.ID)
	f.wantPendingWith(r, "boss")
	if r = f.approve("boss", r.ID); r.Status != StatusApproved {
		t.Fatalf("after boss = %+v", r)
	}
}

// The final approver is the requester: they cannot approve their own leave, so
// the request ends after the last other approver.
func TestEscalationCycleFinalApproverIsTheRequesterStillEnds(t *testing.T) {
	f := newFixture(t)
	f.climbChain(false)
	r := f.climbRequest()
	f.cycleBossHead()
	f.exec(`UPDATE settings SET final_approver_id = ?`, f.people["staff"].ID)

	r = f.approve("head", r.ID)
	f.wantPendingWith(r, "boss")
	if r = f.approve("boss", r.ID); r.Status != StatusApproved {
		t.Fatalf("after boss = %+v", r)
	}
}
