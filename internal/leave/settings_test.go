package leave

import (
	"testing"
	"time"
)

// Each bad value is refused by name, so the page can say which field is wrong,
// and nothing is stored: a rejected save leaves the previous settings in force.
func TestSaveSettingsRejectsBadInput(t *testing.T) {
	f := newFixture(t)
	mon, sat := time.Monday, time.Saturday
	f.setSettings(func(st *Settings) { st.SickBackdateDays = 20 })
	f.db.Exec(`UPDATE users SET disabled = 1 WHERE username = 'orphan'`)

	cases := []struct {
		name   string
		change func(*Settings)
		field  string
	}{
		{"no working days", func(s *Settings) { s.Week = Week{} }, "days"},
		{"restricted day not worked", func(s *Settings) { s.RestrictedDay = &sat }, "restricted_day"},
		{"restricted min zero", func(s *Settings) { s.RestrictedMinDays = 0 }, "restricted_min"},
		{"restricted min 32", func(s *Settings) { s.RestrictedMinDays = 32 }, "restricted_min"},
		{"backdate negative", func(s *Settings) { s.SickBackdateDays = -1 }, "sick_backdate"},
		{"backdate 366", func(s *Settings) { s.SickBackdateDays = 366 }, "sick_backdate"},
		{"cycle year 1999", func(s *Settings) { s.SickCycleAnchor = 1999 }, "sick_cycle"},
		{"cycle year 2101", func(s *Settings) { s.SickCycleAnchor = 2101 }, "sick_cycle"},
		{"team default 101", func(s *Settings) { s.TeamDefaultPercent = 101 }, "team_default"},
		{"team default negative", func(s *Settings) { s.TeamDefaultPercent = -1 }, "team_default"},
		{"final approver unknown", func(s *Settings) { s.FinalApproverID = 9999 }, "final_approver"},
		{"final approver disabled", func(s *Settings) { s.FinalApproverID = f.people["orphan"].ID }, "final_approver"},
	}
	for _, c := range cases {
		st, _ := f.svc.Settings(f.ctx)
		c.change(&st)
		if err := f.svc.SaveSettings(f.ctx, f.people["hr"], st); !fieldErr(err, c.field) {
			t.Errorf("%s: err = %v, want a %s error", c.name, err, c.field)
		}
	}
	// The restricted day passes once it is a working day.
	st, _ := f.svc.Settings(f.ctx)
	st.RestrictedDay = &mon
	if err := f.svc.SaveSettings(f.ctx, f.people["hr"], st); err != nil {
		t.Errorf("restricted Monday on a Monday week: %v", err)
	}
	got, _ := f.svc.Settings(f.ctx)
	if got.SickBackdateDays != 20 || got.FinalApproverID != f.people["pastor"].ID {
		t.Errorf("a rejected save changed the stored settings: %+v", got)
	}
	// Only an admin may save.
	if err := f.svc.SaveSettings(f.ctx, f.people["staff"], st); err != ErrNotAllowed {
		t.Errorf("non-admin save: %v", err)
	}
}

// Settings are read by every operation and cached by none, so an admin's
// change applies to the very next quote.
func TestSettingsChangeAppliesImmediately(t *testing.T) {
	f := newFixture(t)
	in := Input{TypeID: f.types["annual"].ID, Start: "2026-10-12", End: "2026-10-12", Note: "n"} // a Monday
	q, err := f.svc.Preview(f.ctx, f.people["staff"], in)
	if err != nil || q.Days != 1 {
		t.Fatalf("Monday on the default week: %d days, %v", q.Days, err)
	}
	f.setSettings(func(st *Settings) { st.Week = WeekOf(time.Tuesday, time.Wednesday, time.Thursday, time.Friday) })
	if _, err := f.svc.Preview(f.ctx, f.people["staff"], in); !fieldErr(err, "end") {
		t.Fatalf("Monday after it stopped being a working day: %v", err)
	}
	f.setSettings(func(st *Settings) { st.Week = MonFri })
	if q, err = f.svc.Preview(f.ctx, f.people["staff"], in); err != nil || q.Days != 1 {
		t.Fatalf("Monday after switching back: %d days, %v", q.Days, err)
	}

	// Sick-leave backdating is read the same way: 10 days back is fine at 14
	// and refused at 7.
	sick := Input{TypeID: f.types["sick"].ID, Start: "2026-09-25", End: "2026-09-25", Note: "n"}
	if _, err := f.svc.Preview(f.ctx, f.people["staff"], sick); err != nil {
		t.Fatalf("sick 10 days back at 14: %v", err)
	}
	f.setSettings(func(st *Settings) { st.SickBackdateDays = 7 })
	if _, err := f.svc.Preview(f.ctx, f.people["staff"], sick); !fieldErr(err, "start") {
		t.Fatalf("sick 10 days back at 7: %v", err)
	}
}

// With no final approver the last step of the chain is skipped: people with
// no manager go to the admins' queue (approver 0), and people with a manager
// are unaffected.
func TestFinalApproverUnsetAndSet(t *testing.T) {
	f := newFixture(t)
	approver := func(who string) int64 {
		id, err := f.svc.ResolveApprover(f.ctx, f.people[who])
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	pastor := f.people["pastor"].ID
	if approver("orphan") != pastor || approver("staff") != f.people["head"].ID {
		t.Fatalf("with the pastor set: orphan -> %d, staff -> %d", approver("orphan"), approver("staff"))
	}
	if approver("pastor") != 0 {
		t.Fatal("the final approver's own leave must go to the admins")
	}

	f.setSettings(func(st *Settings) { st.FinalApproverID = 0 })
	if approver("orphan") != 0 {
		t.Fatalf("with none set: orphan -> %d, want the admins' queue", approver("orphan"))
	}
	if approver("staff") != f.people["head"].ID {
		t.Fatal("a manager in the directory still wins")
	}
	r := f.mustSubmit("orphan", "annual", "2026-10-12", "2026-10-12")
	if r.ApproverID != 0 {
		t.Fatalf("orphan's request went to %d, want the admins", r.ApproverID)
	}

	f.setSettings(func(st *Settings) { st.FinalApproverID = pastor })
	if approver("orphan") != pastor {
		t.Fatal("setting the approver again did not apply")
	}

	// A disabled final approver is skipped rather than stranding requests.
	f.db.Exec(`UPDATE users SET disabled = 1 WHERE id = ?`, pastor)
	if approver("orphan") != 0 {
		t.Fatal("a disabled final approver must be skipped")
	}
}

// Escalation follows the chain by user id and ends when nobody is above the
// approver.
func TestEscalationWithFinalApproverByID(t *testing.T) {
	f := newFixture(t)
	r := f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-12")
	f.db.Exec(`UPDATE requests SET escalate = 1 WHERE id = ?`, r.ID)
	r, _ = f.svc.Get(f.ctx, r.ID)
	next, ok, err := f.svc.NextApprover(f.ctx, f.people["head"], r)
	if err != nil || !ok || next.ID != f.people["pastor"].ID {
		t.Fatalf("head -> %v %v %v, want the pastor", next.Username, ok, err)
	}
	// Unsetting the final approver does not cut the directory chain: the head's
	// manager is still next. Nobody above the pastor means it ends there.
	f.setSettings(func(st *Settings) { st.FinalApproverID = 0 })
	if next, ok, err = f.svc.NextApprover(f.ctx, f.people["head"], r); err != nil || !ok || next.ID != f.people["pastor"].ID {
		t.Fatalf("head with no final approver -> %v %v %v", next.Username, ok, err)
	}
	if _, ok, _ = f.svc.NextApprover(f.ctx, f.people["pastor"], r); ok {
		t.Fatal("nobody sits above the pastor")
	}
}
