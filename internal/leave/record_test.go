package leave

import (
	"errors"
	"strings"
	"testing"
)

func (f *fixture) record(admin, who, code, start, end, note string) (Request, error) {
	return f.svc.RecordLeave(f.ctx, f.people[admin], f.people[who].ID,
		Input{TypeID: f.types[code].ID, Start: start, End: end, Note: note})
}

func (f *fixture) mustRecord(who, code, start, end string) Request {
	f.t.Helper()
	r, err := f.record("hr", who, code, start, end, "Taken before the system went live")
	if err != nil {
		f.t.Fatalf("record %s %s..%s: %v", who, start, end, err)
	}
	return r
}

// Recorded leave is an ordinary approved request: counted like one (the
// Tuesday 16 June holiday is free), deducted from the cycle it starts in, and
// carrying who recorded it. 120 days back is far past any backdating limit.
func TestRecordLeaveIsApprovedAndCounted(t *testing.T) {
	f := newFixture(t)
	f.events = nil
	r := f.mustRecord("staff", "annual", "2026-06-15", "2026-06-19")
	if r.Status != StatusApproved || r.Kind != KindLeave || r.Days != 4 {
		t.Fatalf("recorded = %+v, want approved leave of 4 days (holiday not counted)", r)
	}
	hr := f.people["hr"]
	if r.ApproverID != hr.ID || r.DecidedByID != hr.ID || r.DecidedAt == "" || r.DecisionNote != "Recorded by hr" ||
		r.Note != "Taken before the system went live" || r.CycleStart != "2026-01-01" || r.AllowNegative {
		t.Fatalf("recorded = %+v", r)
	}
	if !r.IsRecorded() {
		t.Fatal("IsRecorded is false")
	}
	if got := f.available("staff", "annual"); got != 11 {
		t.Fatalf("available = %d, want 11", got)
	}
	if len(f.events) != 1 || f.events[0].Kind != EventRecorded || f.events[0].Request.ID != r.ID || f.events[0].Actor.ID != hr.ID {
		t.Fatalf("events = %+v", f.events)
	}
}

func TestRecordLeaveRules(t *testing.T) {
	f := newFixture(t)

	// Any date: a year ago, and the max span still applies.
	if _, err := f.record("hr", "staff", "annual", "2025-03-03", "2025-03-04", "x"); err != nil {
		t.Fatalf("a year back: %v", err)
	}
	if _, err := f.record("hr", "staff", "annual", "2026-01-01", "2026-12-31", "x"); !fieldErr(err, "end") {
		t.Fatalf("over the max span: %v", err)
	}
	// Days still to come would bypass approval and coverage: they go through an
	// application, so the last day must not be after today (5 October), even
	// when the leave began earlier.
	for _, c := range [][2]string{{"2026-12-07", "2026-12-07"}, {"2026-10-06", "2026-10-06"}, {"2026-10-05", "2026-10-06"}} {
		if _, err := f.record("hr", "staff", "annual", c[0], c[1], "x"); !fieldErr(err, "end") ||
			err.(FieldErrors)["end"] != MsgRecordFuture {
			t.Fatalf("%v: %v", c, err)
		}
	}
	if _, err := f.record("hr", "staff", "annual", "2026-10-05", "2026-10-05", "x"); err != nil {
		t.Fatalf("ending today: %v", err)
	}

	// A reason is required, for a type that does not ask for one too.
	if _, err := f.record("hr", "staff", "annual", "2026-02-02", "2026-02-02", "  "); !fieldErr(err, "note") {
		t.Fatalf("no note: %v", err)
	}
	// The same checks as a request: dates, a type, nothing to take.
	if _, err := f.record("hr", "staff", "annual", "2026-02-07", "2026-02-08", "x"); !fieldErr(err, "end") {
		t.Fatalf("weekend only: %v", err)
	}
	if _, err := f.svc.RecordLeave(f.ctx, f.people["hr"], f.people["staff"].ID, Input{Start: "2026-02-02", End: "2026-02-02", Note: "x"}); !fieldErr(err, "type") {
		t.Fatalf("no type: %v", err)
	}

	// Overlap with their own leave of any live status is refused.
	f.mustSubmit("staff", "annual", "2026-09-14", "2026-09-15")
	if _, err := f.record("hr", "staff", "sick", "2026-09-15", "2026-09-16", "x"); !fieldErr(err, "start") ||
		!strings.Contains(err.(FieldErrors)["start"], "already have leave") {
		t.Fatalf("overlap with pending: %v", err)
	}
	f.mustRecord("staff", "annual", "2026-03-02", "2026-03-02")
	if _, err := f.record("hr", "staff", "annual", "2026-03-02", "2026-03-02", "x"); !fieldErr(err, "start") {
		t.Fatalf("overlap with recorded: %v", err)
	}

	// Only another admin, never for yourself.
	if _, err := f.record("hr", "hr", "annual", "2026-03-09", "2026-03-09", "x"); !fieldErr(err, "person") {
		t.Fatalf("self: %v", err)
	}
	if _, err := f.record("head", "staff", "annual", "2026-03-09", "2026-03-09", "x"); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("non-admin: %v", err)
	}
}

// A coverage limit exists to stop a request; what happened cannot be stopped.
func TestRecordLeaveIgnoresCoverage(t *testing.T) {
	f := newFixture(t)
	f.dept("Ops", "staff", "hr", "head", "orphan")
	f.rule("Ops", 50)
	f.insertAway("hr", "annual", "2026-09-14", "2026-09-16", "approved")
	f.insertAway("head", "annual", "2026-09-14", "2026-09-16", "approved")
	f.insertAway("orphan", "annual", "2026-09-14", "2026-09-16", "approved")
	if _, err := f.submit("staff", "annual", "2026-09-14", "2026-09-16"); !fieldErr(err, "coverage") {
		t.Fatalf("setup: an ordinary request should be over the limit: %v", err)
	}
	r, err := f.record("hr", "staff", "annual", "2026-09-14", "2026-09-16", "x")
	if err != nil || r.Escalate {
		t.Fatalf("record over the limit: %+v, %v", r, err)
	}
}

func TestRecordLeaveMayGoNegative(t *testing.T) {
	f := newFixture(t)
	f.exhaustAnnual()
	r := f.mustRecord("staff", "annual", "2026-06-01", "2026-06-03")
	if r.Status != StatusApproved || !r.AllowNegative {
		t.Fatalf("recorded = %+v, want approved and flagged", r)
	}
	// The earlier cycle is not the one exhausted: this one is 2026, so -3.
	if got := f.available("staff", "annual"); got != -3 {
		t.Fatalf("available = %d, want -3", got)
	}
}

// Recorded leave has started, so the owner's cancellation flow (future leave
// only) cannot reach it; an admin removes it: days back, event fired, and who
// and when kept for the audit.
func TestUnrecord(t *testing.T) {
	f := newFixture(t)
	past := f.mustRecord("staff", "annual", "2026-06-15", "2026-06-19")
	if _, err := f.svc.RequestCancellation(f.ctx, f.people["staff"], past.ID, ""); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("owner cancelling started leave: %v", err)
	}
	if _, err := f.svc.Unrecord(f.ctx, f.people["staff"], past.ID); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("owner removing: %v", err)
	}
	if _, err := f.svc.Unrecord(f.ctx, f.people["head"], past.ID); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("non-admin removing: %v", err)
	}
	n := f.mustSubmit("staff", "annual", "2026-11-02", "2026-11-02")
	f.svc.Decide(f.ctx, f.people["head"], n.ID, true, "")
	if _, err := f.svc.Unrecord(f.ctx, f.people["hr"], n.ID); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("removing ordinary leave: %v", err)
	}

	f.events = nil
	got, err := f.svc.Unrecord(f.ctx, f.people["hr"], past.ID)
	if err != nil || got.Status != StatusCancelled {
		t.Fatalf("unrecord = %+v, %v", got, err)
	}
	if got.RemovedByID != f.people["hr"].ID || got.RemovedBy != "hr" || got.RemovedAt == "" ||
		got.RecordedBy != "hr" || !got.IsRecorded() {
		t.Fatalf("audit lost: %+v", got)
	}
	if len(f.events) != 1 || f.events[0].Kind != EventRevoked {
		t.Fatalf("events = %+v", f.events)
	}
	if _, err := f.svc.Unrecord(f.ctx, f.people["hr"], past.ID); !fieldErr(err, "request") ||
		err.(FieldErrors)["request"] != MsgUnrecordGone {
		t.Fatalf("removing twice: %v", err)
	}
	if got := f.available("staff", "annual"); got != 14 { // only the ordinary day remains
		t.Fatalf("available = %d, want 14", got)
	}
}

// An approver's decision note is free text. One that reads "Recorded by ..."
// must not make ordinary leave look recorded or let an admin remove it.
func TestApprovalNoteCannotImpersonateRecording(t *testing.T) {
	f := newFixture(t)
	r := f.mustSubmit("staff", "annual", "2026-11-02", "2026-11-02")
	r, err := f.svc.Decide(f.ctx, f.people["head"], r.ID, true, "Recorded by me in the team diary too")
	if err != nil {
		t.Fatal(err)
	}
	if r.IsRecorded() {
		t.Fatal("an approval with a lookalike note is recorded")
	}
	if _, err := f.svc.Unrecord(f.ctx, f.people["hr"], r.ID); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("unrecord: %v", err)
	}
	if got, _ := f.svc.Get(f.ctx, r.ID); got.Status != StatusApproved {
		t.Fatalf("status = %s", got.Status)
	}
}

// What happened was decided under the rules of the day: a type since retired
// and a restricted-day rule since introduced do not apply, though days are
// still counted the same way.
func TestRecordLeaveIgnoresRetiredTypeAndRestrictedDay(t *testing.T) {
	f := newSundayWeek(t)
	// Sunday alone is refused for a request...
	if _, err := f.submit("staff", "annual", "2026-09-27", "2026-09-27"); !fieldErr(err, "start") {
		t.Fatalf("setup: %v", err)
	}
	// ...but may be recorded.
	r, err := f.record("hr", "staff", "annual", "2026-09-27", "2026-09-27", "x")
	if err != nil || r.Days != 1 {
		t.Fatalf("restricted day: %+v, %v", r, err)
	}
	// Retire a type: requests refuse it, recording does not.
	if _, err := f.db.Exec(`UPDATE leave_types SET active = 0 WHERE id = ?`, f.types["parental"].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.submit("staff", "parental", "2026-09-15", "2026-09-15"); !fieldErr(err, "type") {
		t.Fatalf("setup: retired type accepted: %v", err)
	}
	if r, err := f.record("hr", "staff", "parental", "2026-09-15", "2026-09-15", "x"); err != nil || r.TypeName == "" {
		t.Fatalf("retired type: %+v, %v", r, err)
	}
}

// Leave from an earlier cycle is charged to that cycle, and flagged against
// it: this year's balance is untouched.
func TestRecordLeaveInPreviousCycle(t *testing.T) {
	f := newFixture(t)
	f.events = nil
	// 3 to 28 Feb 2025 is 20 working days against an allowance of 15.
	r, err := f.record("hr", "staff", "annual", "2025-02-03", "2025-02-28", "x")
	if err != nil {
		t.Fatal(err)
	}
	if r.CycleStart != "2025-01-01" || r.Days != 20 || !r.AllowNegative {
		t.Fatalf("recorded = %+v", r)
	}
	cycle, _ := ParseDate("2025-01-01")
	b, err := f.svc.balance(f.ctx, f.db, f.people["staff"].ID, f.types["annual"], cycle)
	if err != nil || *b.Available() != -5 {
		t.Fatalf("2025 balance = %+v, %v", b, err)
	}
	if got := f.available("staff", "annual"); got != 15 {
		t.Fatalf("this year's balance = %d, want 15", got)
	}
	if len(f.events) != 1 || f.events[0].Shortfall != 5 {
		t.Fatalf("events = %+v, want a shortfall of 5", f.events)
	}
}

func TestRecordWeekendOff(t *testing.T) {
	f := newSundayWeek(t)
	r, err := f.record("hr", "staff", "weekend", "2026-09-12", "2026-09-13", "x")
	if err != nil || r.Days != 1 || r.Status != StatusApproved {
		t.Fatalf("weekend off: %+v, %v", r, err)
	}
	if _, err := f.record("hr", "staff", "weekend", "2026-09-12", "2026-09-14", "x"); !fieldErr(err, "start") {
		t.Fatalf("badly shaped weekend: %v", err)
	}
	if got := f.available("staff", "weekend"); got != 3 {
		t.Fatalf("weekends left = %d, want 3", got)
	}
}

// Nobody adjusts their own balance, and only an admin adjusts anyone's: the same
// rule as recording leave.
func TestAdjustmentIsAnotherAdminsJob(t *testing.T) {
	f := newFixture(t)
	adj := func(who string) Adjustment {
		return Adjustment{UserID: f.people[who].ID, TypeID: f.types["annual"].ID, CycleStart: "2026-01-01", Days: 1, Reason: "x"}
	}
	if err := f.svc.AddAdjustment(f.ctx, f.people["hr"], adj("hr")); !fieldErr(err, "person") ||
		err.(FieldErrors)["person"] != MsgAdjustOwn {
		t.Fatalf("own balance: %v", err)
	}
	if err := f.svc.AddAdjustment(f.ctx, f.people["staff"], adj("staff")); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("non-admin: %v", err)
	}
	if err := f.svc.AddAdjustment(f.ctx, f.people["head"], adj("staff")); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("non-admin on someone else: %v", err)
	}
	if as, _ := f.svc.Adjustments(f.ctx, f.people["hr"].ID); len(as) != 0 {
		t.Fatalf("adjustments = %v", as)
	}
	if err := f.svc.AddAdjustment(f.ctx, f.people["hr"], adj("staff")); err != nil {
		t.Fatal(err)
	}
}
