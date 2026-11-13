package leave

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"
)

func TestSetHalvesApprovedBalanceAuditAndClear(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "off", true: "on"}[enabled], func(t *testing.T) {
			f := newFixture(t)
			f.setSettings(func(st *Settings) { st.HalfDaysEnabled = enabled })
			r := f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-14")
			if _, err := f.svc.Decide(f.ctx, f.people["head"], r.ID, true, ""); err != nil {
				t.Fatal(err)
			}
			before := f.availableDays("staff", "annual")
			f.events = nil
			out, err := f.svc.SetHalves(f.ctx, f.people["hr"], r.ID, true, true)
			if err != nil {
				t.Fatal(err)
			}
			if out.Days != 200 || out.Status != StatusApproved || !out.HalfStart || !out.HalfEnd || out.HalvesSetByID != f.people["hr"].ID || out.HalvesSetBy != "hr" || out.HalvesSetAt != stampNow(f.svc.Now()) {
				t.Fatalf("changed: %+v", out)
			}
			if got := f.availableDays("staff", "annual"); got-before != r.Days-out.Days {
				t.Fatalf("balance delta=%s, want %s", got-before, r.Days-out.Days)
			}
			if len(f.events) != 1 || f.events[0].Kind != EventHalvesChanged || f.events[0].Before.Days != r.Days {
				t.Fatalf("events: %+v", f.events)
			}
			cleared, err := f.svc.SetHalves(f.ctx, f.people["hr"], r.ID, false, false)
			if err != nil || cleared.Days != r.Days || cleared.HalfStart || cleared.HalfEnd || f.availableDays("staff", "annual") != before {
				t.Fatalf("clear: %+v, %v", cleared, err)
			}
		})
	}
}

func TestSetHalvesNoOpAndStaleDecision(t *testing.T) {
	f := newFixture(t)
	r := f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-14")
	f.events = nil
	// An UPDATE, even one writing the same values, must fail this trigger.
	if _, err := f.db.Exec(`CREATE TRIGGER no_noop_write BEFORE UPDATE ON requests BEGIN SELECT RAISE(ABORT, 'unexpected write'); END`); err != nil {
		t.Fatal(err)
	}
	_, err := f.svc.SetHalves(f.ctx, f.people["hr"], r.ID, false, false)
	var fe FieldErrors
	if !errors.As(err, &fe) || fe["halves"] != MsgHalvesUnchanged || len(f.events) != 0 || !reflect.DeepEqual(r, mustGet(f, r.ID)) {
		t.Fatalf("no-op: %v, events=%v", err, f.events)
	}
	f.db.Exec(`DROP TRIGGER no_noop_write`)
	out, err := f.svc.SetHalves(f.ctx, f.people["hr"], r.ID, false, true)
	if err != nil || out.Fingerprint() == r.Fingerprint() {
		t.Fatalf("change: %+v, %v", out, err)
	}
	if _, err := f.svc.DecideSeen(f.ctx, f.people["head"], r.ID, true, "", r.Fingerprint()); !errors.Is(err, ErrChanged) {
		t.Fatalf("stale decision: %v", err)
	}
}

func TestSetHalvesRefusals(t *testing.T) {
	for _, name := range []string{"own", "non-admin", "disabled", "calendar", "cancelled", "declined", "withdrawn", "cancellation kind", "cancellation pending", "shutdown"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			r := f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-14")
			admin := f.people["hr"]
			switch name {
			case "own":
				admin = f.people["staff"]
				admin.Admin = true
			case "non-admin":
				admin = f.people["head"]
			case "disabled":
				admin.Disabled = true
			case "calendar":
				_, err := f.db.Exec(`UPDATE requests SET type_id=? WHERE id=?`, f.types["maternity"].ID, r.ID)
				if err != nil {
					t.Fatal(err)
				}
			case "cancelled", "declined", "withdrawn":
				if _, err := f.db.Exec(`UPDATE requests SET status=? WHERE id=?`, name, r.ID); err != nil {
					t.Fatal(err)
				}
			case "cancellation kind", "cancellation pending":
				if _, err := f.svc.Decide(f.ctx, f.people["head"], r.ID, true, ""); err != nil {
					t.Fatal(err)
				}
				c, err := f.svc.RequestCancellation(f.ctx, f.people["staff"], r.ID, "changed")
				if err != nil {
					t.Fatal(err)
				}
				if name == "cancellation kind" {
					r = c
				}
			case "shutdown":
				if _, err := f.db.Exec(`INSERT INTO shutdowns(id,name,created_by_id,created_at) VALUES(1,'Closure',?,'')`, admin.ID); err != nil {
					t.Fatal(err)
				}
				if _, err := f.db.Exec(`INSERT INTO shutdown_parts(id,shutdown_id,type_id,start_date,end_date) VALUES(1,1,?,'2026-10-12','2026-10-14')`, r.TypeID); err != nil {
					t.Fatal(err)
				}
				if _, err := f.db.Exec(`UPDATE requests SET shutdown_part_id=1 WHERE id=?`, r.ID); err != nil {
					t.Fatal(err)
				}
			}
			before := mustGet(f, r.ID)
			f.events = nil
			_, err := f.svc.SetHalves(f.ctx, admin, r.ID, true, false)
			if err == nil || len(f.events) != 0 || !reflect.DeepEqual(before, mustGet(f, r.ID)) {
				t.Fatalf("refusal=%v events=%v", err, f.events)
			}
			if name == "own" {
				var fe FieldErrors
				if !errors.As(err, &fe) || fe["halves"] != MsgHalvesOwn {
					t.Fatalf("own: %v", err)
				}
			}
		})
	}
}

func TestSetHalvesBelowZeroAndLiveStates(t *testing.T) {
	for _, status := range []string{StatusPending, StatusApproved, StatusDraft, StatusAwaitingHR} {
		t.Run(status, func(t *testing.T) {
			f := newHalfFixture(t)
			r := f.mustSubmitHalf("staff", "annual", "2026-10-12", "2026-10-14", true, true)
			if _, err := f.db.Exec(`UPDATE requests SET status=? WHERE id=?`, status, r.ID); err != nil {
				t.Fatal(err)
			}
			if err := f.svc.AddAdjustment(f.ctx, f.people["hr"], Adjustment{UserID: r.UserID, TypeID: r.TypeID, CycleStart: r.CycleStart, Days: -1250, Reason: "Payroll"}); err != nil {
				t.Fatal(err)
			}
			out, err := f.svc.SetHalves(f.ctx, f.people["hr"], r.ID, false, false)
			if err != nil || out.Days != 300 || !out.AllowNegative || out.Status != status {
				t.Fatalf("change: %+v %v", out, err)
			}
		})
	}
}

func TestSetHalvesRelaxesChangedPolicyAndRollsBackHook(t *testing.T) {
	f := newFixture(t)
	r := f.mustRecord("staff", "annual", "2026-03-02", "2026-03-04")
	if _, err := f.db.Exec(`UPDATE leave_types SET active=0 WHERE id=?`, r.TypeID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SetHalves(f.ctx, f.people["hr"], r.ID, true, false); err != nil {
		t.Fatalf("past retired leave: %v", err)
	}
	before := mustGet(f, r.ID)
	f.svc.OnEvent(func(context.Context, *sql.Tx, Event) error { return errors.New("mail refused") })
	if _, err := f.svc.SetHalves(f.ctx, f.people["hr"], r.ID, false, true); err == nil || !reflect.DeepEqual(before, mustGet(f, r.ID)) {
		t.Fatalf("rollback: %v", err)
	}
}

func TestSetHalvesUsesStoredSickCycleAfterAnchorChanges(t *testing.T) {
	f := newHalfFixture(t)
	r := f.mustSubmitHalf("staff", "sick", "2026-10-12", "2026-10-14", true, true)
	if _, err := f.svc.Decide(f.ctx, f.people["head"], r.ID, true, ""); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.AddAdjustment(f.ctx, f.people["hr"], Adjustment{UserID: r.UserID, TypeID: r.TypeID,
		CycleStart: r.CycleStart, Days: -2750, Reason: "Payroll"}); err != nil {
		t.Fatal(err)
	}
	cycle, err := ParseDate(r.CycleStart)
	if err != nil {
		t.Fatal(err)
	}
	before, err := f.svc.BalanceIn(f.ctx, r.UserID, f.types["sick"], cycle)
	if err != nil {
		t.Fatal(err)
	}
	if *before.Available() != 50 {
		t.Fatalf("before=%s", *before.Available())
	}
	f.setSettings(func(st *Settings) { st.SickCycleAnchor++ })
	out, err := f.svc.SetHalves(f.ctx, f.people["hr"], r.ID, false, false)
	if err != nil {
		t.Fatal(err)
	}
	after, err := f.svc.BalanceIn(f.ctx, r.UserID, f.types["sick"], cycle)
	if err != nil {
		t.Fatal(err)
	}
	if out.CycleStart != r.CycleStart || out.Days != 300 || !out.AllowNegative || *after.Available() != -50 {
		t.Fatalf("changed=%+v, stored-cycle available=%s", out, *after.Available())
	}
}
