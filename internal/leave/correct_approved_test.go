package leave

import (
	"errors"
	"reflect"
	"testing"
)

// approvedAnnual books annual leave for staff and has head approve it, then
// gives each range a calendar event id as the worker would.
func (f *fixture) approvedAnnual(start, end string) Request {
	f.t.Helper()
	r := f.mustSubmit("staff", "annual", start, end)
	if _, err := f.svc.Decide(f.ctx, f.people["head"], r.ID, true, ""); err != nil {
		f.t.Fatal(err)
	}
	f.stampEvents(r.ID)
	r, err := f.svc.Get(f.ctx, r.ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

func (f *fixture) stampEvents(id int64) {
	f.t.Helper()
	rs, err := f.svc.CalendarRanges(f.ctx, id)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, r := range rs {
		if err := f.svc.SetRangeEventID(f.ctx, id, r.Start, "ev-"+r.Start); err != nil {
			f.t.Fatal(err)
		}
	}
}

// taken is what the person's approved leave of this type counts against them.
func (f *fixture) taken(who, code string) Days {
	f.t.Helper()
	bs, err := f.svc.Balances(f.ctx, f.people[who].ID)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, b := range bs {
		if b.Type.Code == code {
			return b.Taken
		}
	}
	f.t.Fatalf("no %s balance", code)
	return 0
}

func (f *fixture) eventIDs(id int64) []string {
	f.t.Helper()
	rs, err := f.svc.CalendarRanges(f.ctx, id)
	if err != nil {
		f.t.Fatal(err)
	}
	var out []string
	for _, r := range rs {
		out = append(out, r.EventID)
	}
	return out
}

func TestCorrectApprovedAnnualToCompassionate(t *testing.T) {
	f := newFixture(t)
	r := f.approvedAnnual("2026-10-12", "2026-10-14")
	if f.taken("staff", "annual") != WholeDays(3) || f.taken("staff", "compassionate") != 0 {
		t.Fatalf("before: annual %v compassionate %v", f.taken("staff", "annual"), f.taken("staff", "compassionate"))
	}
	f.events = nil
	out, err := f.svc.Correct(f.ctx, f.people["hr"], r.ID,
		Input{TypeID: f.types["compassionate"].ID, Start: "2026-10-12", End: "2026-10-14", Note: "funeral"}, "bereavement", r.Fingerprint())
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != StatusApproved || out.TypeName != f.types["compassionate"].Name || out.Days != WholeDays(3) ||
		out.DecidedByID != r.DecidedByID || out.DecidedAt != r.DecidedAt || out.ApproverID != r.ApproverID {
		t.Fatalf("corrected: %+v (was %+v)", out, r)
	}
	if got := f.taken("staff", "annual"); got != 0 {
		t.Fatalf("annual taken %v, want 0 (days handed back)", got)
	}
	if got := f.taken("staff", "compassionate"); got != WholeDays(3) {
		t.Fatalf("compassionate taken %v, want 3 (days counted)", got)
	}
	if cs := f.corrections(r.ID); len(cs) != 1 || cs[0].Reason != "bereavement" {
		t.Fatalf("corrections: %+v", cs)
	}
	if len(f.events) != 1 || f.events[0].Kind != EventCorrected || !reflect.DeepEqual(f.events[0].StaleEventIDs, []string{"ev-2026-10-12"}) {
		t.Fatalf("events: %+v", f.events)
	}
	if ids := f.eventIDs(r.ID); !reflect.DeepEqual(ids, []string{""}) {
		t.Fatalf("range event ids after correction: %q", ids)
	}
}

func TestCorrectApprovedTwoRangesToOne(t *testing.T) {
	f := newFixture(t)
	r, err := f.svc.Submit(f.ctx, f.people["staff"], Input{TypeID: f.types["annual"].ID,
		Ranges: []Range{{"2026-10-12", "2026-10-12"}, {"2026-10-14", "2026-10-14"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.Decide(f.ctx, f.people["head"], r.ID, true, ""); err != nil {
		t.Fatal(err)
	}
	f.stampEvents(r.ID)
	f.events = nil
	out := f.mustCorrect("hr", r.ID, "annual", "2026-10-12", "2026-10-13", "", "merge")
	if len(out.Ranges) != 1 || out.Days != WholeDays(2) {
		t.Fatalf("corrected: %+v", out)
	}
	if len(f.events) != 1 || !reflect.DeepEqual(f.events[0].StaleEventIDs, []string{"ev-2026-10-12", "ev-2026-10-14"}) {
		t.Fatalf("stale ids: %+v", f.events)
	}
	if ids := f.eventIDs(r.ID); !reflect.DeepEqual(ids, []string{""}) {
		t.Fatalf("ids: %q", ids)
	}
}

// Dates already over are still correctable: payroll may need the fix.
func TestCorrectApprovedLeaveAlreadyOver(t *testing.T) {
	f := newFixture(t)
	r := f.mustRecord("staff", "annual", "2026-09-14", "2026-09-15") // recorded: not correctable
	if _, err := f.correct("hr", r.ID, "unpaid", "2026-09-14", "2026-09-15", "n", "why"); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("recorded: %v", err)
	}
	f.exec(`UPDATE requests SET recorded_by_id = NULL WHERE id = ?`, r.ID)
	out, err := f.correct("hr", r.ID, "unpaid", "2026-09-14", "2026-09-15", "n", "why")
	if err != nil || out.Status != StatusApproved || out.TypeName != f.types["unpaid"].Name {
		t.Fatalf("past approved leave: %+v, %v", out, err)
	}
}

func TestCorrectApprovedRefusedWithPendingCancellation(t *testing.T) {
	f := newFixture(t)
	r := f.approvedAnnual("2026-10-12", "2026-10-14")
	if _, err := f.svc.RequestCancellation(f.ctx, f.people["staff"], r.ID, ""); err != nil {
		t.Fatal(err)
	}
	f.events = nil
	if _, err := f.correct("hr", r.ID, "unpaid", "2026-10-12", "2026-10-14", "n", "why"); !fieldErr(err, "correct") {
		t.Fatalf("got %v", err)
	}
	if len(f.events) != 0 || len(f.corrections(r.ID)) != 0 {
		t.Fatalf("refused correction left traces: %+v", f.events)
	}
	if ids := f.eventIDs(r.ID); !reflect.DeepEqual(ids, []string{"ev-2026-10-12"}) {
		t.Fatalf("refused correction touched event ids: %q", ids)
	}
}

func TestCancellationAfterCorrectionCopiesCorrectedRanges(t *testing.T) {
	f := newFixture(t)
	r := f.approvedAnnual("2026-10-12", "2026-10-14")
	f.mustCorrect("hr", r.ID, "annual", "2026-10-13", "2026-10-15", "", "moved")
	c, err := f.svc.RequestCancellation(f.ctx, f.people["staff"], r.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.Ranges, []Range{{"2026-10-13", "2026-10-15"}}) || c.Days != WholeDays(3) {
		t.Fatalf("cancellation copied %+v (%v days)", c.Ranges, c.Days)
	}
}

// Not-yet-approved corrections never carry calendar ids (nothing is on the
// calendar yet), and the event says so for the hook.
func TestCorrectPendingCarriesNoStaleEvents(t *testing.T) {
	f := newFixture(t)
	r := f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-14")
	f.events = nil
	f.mustCorrect("hr", r.ID, "unpaid", "2026-10-12", "2026-10-14", "n", "why")
	if len(f.events) != 1 || f.events[0].StaleEventIDs != nil {
		t.Fatalf("events: %+v", f.events)
	}
}

func TestCorrectionCount(t *testing.T) {
	f := newFixture(t)
	r := f.approvedAnnual("2026-10-12", "2026-10-14")
	for want := 0; want < 3; want++ {
		if n, err := f.svc.CorrectionCount(f.ctx, r.ID); err != nil || n != want {
			t.Fatalf("count %d, %v; want %d", n, err, want)
		}
		typ := "unpaid"
		if want%2 == 1 {
			typ = "annual"
		}
		f.mustCorrect("hr", r.ID, typ, "2026-10-12", "2026-10-14", "n", "r")
	}
}
