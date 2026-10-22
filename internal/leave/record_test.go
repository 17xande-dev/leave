package leave

import (
	"errors"
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
	// Even a future date works (agreed off the system).
	if _, err := f.record("hr", "staff", "annual", "2026-12-07", "2026-12-07", "x"); err != nil {
		t.Fatalf("future: %v", err)
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
	f.mustSubmit("staff", "annual", "2026-10-19", "2026-10-20")
	if _, err := f.record("hr", "staff", "sick", "2026-10-20", "2026-10-21", "x"); !fieldErr(err, "start") {
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
	f.insertAway("hr", "annual", "2026-10-12", "2026-10-14", "approved")
	f.insertAway("head", "annual", "2026-10-12", "2026-10-14", "approved")
	f.insertAway("orphan", "annual", "2026-10-12", "2026-10-14", "approved")
	if _, err := f.submit("staff", "annual", "2026-10-12", "2026-10-14"); !fieldErr(err, "coverage") {
		t.Fatalf("setup: an ordinary request should be over the limit: %v", err)
	}
	r, err := f.record("hr", "staff", "annual", "2026-10-12", "2026-10-14", "x")
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

// Recorded leave for the future can be cancelled by its owner the ordinary way.
func TestRecordedFutureLeaveCancelsLikeAnyLeave(t *testing.T) {
	f := newFixture(t)
	r := f.mustRecord("staff", "annual", "2026-10-12", "2026-10-16")
	c, err := f.svc.RequestCancellation(f.ctx, f.people["staff"], r.ID, "plans changed")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Decide(f.ctx, f.people["head"], c.ID, true, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.svc.Get(f.ctx, r.ID); got.Status != StatusCancelled {
		t.Fatalf("status = %s", got.Status)
	}
	if got := f.available("staff", "annual"); got != 15 {
		t.Fatalf("available = %d, want 15", got)
	}
}

// Leave that has started cannot go through the owner's cancellation, so an
// admin removes it: days back, event fired, a waiting cancellation closed.
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
	// Ordinary approved leave is not removable this way.
	n := f.mustSubmit("staff", "annual", "2026-11-02", "2026-11-02")
	f.svc.Decide(f.ctx, f.people["head"], n.ID, true, "")
	if _, err := f.svc.Unrecord(f.ctx, f.people["hr"], n.ID); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("removing ordinary leave: %v", err)
	}

	future := f.mustRecord("staff", "annual", "2026-10-12", "2026-10-12")
	c, err := f.svc.RequestCancellation(f.ctx, f.people["staff"], future.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	f.events = nil
	got, err := f.svc.Unrecord(f.ctx, f.people["hr"], past.ID)
	if err != nil || got.Status != StatusCancelled {
		t.Fatalf("unrecord = %+v, %v", got, err)
	}
	if len(f.events) != 1 || f.events[0].Kind != EventRevoked {
		t.Fatalf("events = %+v", f.events)
	}
	if _, err := f.svc.Unrecord(f.ctx, f.people["hr"], past.ID); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("removing twice: %v", err)
	}
	if _, err := f.svc.Unrecord(f.ctx, f.people["hr"], future.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.svc.Get(f.ctx, c.ID); got.Status != StatusWithdrawn {
		t.Fatalf("pending cancellation is %s, want withdrawn", got.Status)
	}
	if got := f.available("staff", "annual"); got != 14 { // only the ordinary day remains
		t.Fatalf("available = %d, want 14", got)
	}
}
