package leave

import (
	"errors"
	"strings"
	"testing"
)

// Mon 12 Oct 2026 starts a plain working week (the fixture's "today" is Mon 5
// Oct): Mon 12, Tue 13, Wed 14, Thu 15, Fri 16, Sat 17. Wed 16 Dec 2026 is the
// Day of Reconciliation, a public holiday.

func (f *fixture) submitHalf(who, code, start, end string, hs, he bool) (Request, error) {
	return f.svc.Submit(f.ctx, f.people[who], Input{TypeID: f.types[code].ID, Start: start, End: end,
		Note: "n", HalfStart: hs, HalfEnd: he})
}

func (f *fixture) mustSubmitHalf(who, code, start, end string, hs, he bool) Request {
	f.t.Helper()
	r, err := f.submitHalf(who, code, start, end, hs, he)
	if err != nil {
		f.t.Fatalf("submit %s..%s half_start=%v half_end=%v: %v", start, end, hs, he, err)
	}
	return r
}

// availableDays is the balance exactly, fractions included.
func (f *fixture) availableDays(who, code string) Days {
	f.t.Helper()
	bs, err := f.svc.Balances(f.ctx, f.people[who].ID)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, b := range bs {
		if b.Type.Code == code {
			return *b.Available()
		}
	}
	f.t.Fatalf("no %s balance", code)
	return 0
}

// The patterns HR's log uses: "1/2 day", "1 and 1/2 days", "2.5 days", and a
// flag at both ends. Each is charged to the balance exactly.
func TestHalfDayRequestsAreChargedExactly(t *testing.T) {
	for _, tc := range []struct {
		name       string
		start, end string
		hs, he     bool
		days       string
		left       string
	}{
		{"a single half day", "2026-10-12", "2026-10-12", true, false, "0.5", "14.5"},
		{"1.5 days, last day half", "2026-10-12", "2026-10-13", false, true, "1.5", "13.5"},
		{"1.5 days, first day half", "2026-10-12", "2026-10-13", true, false, "1.5", "13.5"},
		{"2.5 days", "2026-10-12", "2026-10-14", false, true, "2.5", "12.5"},
		{"both ends of three days", "2026-10-12", "2026-10-14", true, true, "2", "13"},
		{"3.5 days", "2026-10-12", "2026-10-15", false, true, "3.5", "11.5"},
		// Thu-Mon: Thu, Fri and Mon are charged, the weekend is not; the last
		// charged day is the Monday, which is the flagged one.
		{"half day on the Monday after a weekend", "2026-10-15", "2026-10-19", false, true, "2.5", "12.5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			r := f.mustSubmitHalf("staff", "annual", tc.start, tc.end, tc.hs, tc.he)
			if r.Days.String() != tc.days || r.HalfStart != tc.hs || r.HalfEnd != tc.he {
				t.Fatalf("request = %s days, half_start=%v half_end=%v; want %s, %v, %v",
					r.Days, r.HalfStart, r.HalfEnd, tc.days, tc.hs, tc.he)
			}
			if got := f.availableDays("staff", "annual"); got.String() != tc.left {
				t.Fatalf("available = %s, want %s", got, tc.left)
			}
		})
	}
}

func TestHalfDayRefusals(t *testing.T) {
	f := newFixture(t)
	for _, tc := range []struct {
		name       string
		code       string
		start, end string
		hs, he     bool
		field      string
		contains   string
	}{
		{"half day on a public holiday", "annual", "2026-12-16", "2026-12-16", true, false, "half_start", "Wed 16 Dec"},
		{"last day a public holiday", "annual", "2026-12-15", "2026-12-16", false, true, "half_end", "Wed 16 Dec"},
		{"first day a Saturday", "annual", "2026-10-17", "2026-10-19", true, false, "half_start", "Sat 17 Oct"},
		{"last day a Sunday", "annual", "2026-10-16", "2026-10-18", false, true, "half_end", "Sun 18 Oct"},
		{"half_end on a one-day request", "annual", "2026-10-12", "2026-10-12", false, true, "half_end", "First (or only) day"},
		{"a calendar-unit type", "maternity", "2026-10-12", "2026-10-14", true, false, "half_start", "not counted in working days"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.submitHalf("staff", tc.code, tc.start, tc.end, tc.hs, tc.he)
			var fe FieldErrors
			if !errors.As(err, &fe) || fe[tc.field] == "" {
				t.Fatalf("err = %v, want a %s error", err, tc.field)
			}
			if !strings.Contains(fe[tc.field], tc.contains) {
				t.Errorf("message %q does not mention %q", fe[tc.field], tc.contains)
			}
			// Nothing was saved by a refusal.
			if rs, _ := f.svc.ForUser(f.ctx, f.people["staff"].ID); len(rs) != 0 {
				t.Fatalf("a refused request was saved: %+v", rs)
			}
		})
	}
}

// Without a flag the same dates behave as before: a request whose last day is a
// Saturday still counts only its working days.
func TestNoHalfFlagLeavesWholeDayCountingAlone(t *testing.T) {
	f := newFixture(t)
	r := f.mustSubmit("staff", "annual", "2026-10-16", "2026-10-18")
	if r.Days != WholeDays(1) || r.HalfStart || r.HalfEnd {
		t.Fatalf("request = %+v", r)
	}
}

// An edit that changes only a flag changes the days, the balance and the
// fingerprint, so an approver who read the old page cannot approve the new one.
func TestEditChangingOnlyAFlag(t *testing.T) {
	f := newFixture(t)
	staff := f.people["staff"]
	r := f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-13")
	if f.availableDays("staff", "annual") != WholeDays(13) {
		t.Fatalf("available = %s", f.availableDays("staff", "annual"))
	}
	seen := r.Fingerprint()
	f.events = nil
	e, err := f.svc.Edit(f.ctx, staff, r.ID, Input{TypeID: r.TypeID, Start: r.Start, End: r.End, Note: r.Note, HalfEnd: true})
	if err != nil {
		t.Fatal(err)
	}
	if e.Days != 150 || !e.HalfEnd || e.HalfStart {
		t.Fatalf("edited = %s days half_start=%v half_end=%v", e.Days, e.HalfStart, e.HalfEnd)
	}
	if got := f.availableDays("staff", "annual"); got.String() != "13.5" {
		t.Fatalf("available after the edit = %s, want 13.5", got)
	}
	if e.Fingerprint() == seen {
		t.Fatal("an edit that only changed a flag kept the fingerprint")
	}
	if len(f.events) != 1 || f.events[0].Kind != EventEdited || f.events[0].Before == nil || f.events[0].Before.HalfEnd {
		t.Fatalf("events = %+v", f.events)
	}
	// The approver who read the page before the edit is refused.
	if _, err := f.svc.DecideSeen(f.ctx, f.people["head"], r.ID, true, "", seen); !errors.Is(err, ErrChanged) {
		t.Fatalf("deciding on the old fingerprint: %v", err)
	}
	// Saving the same flags again is a no-op: nobody is told.
	f.events = nil
	if _, err := f.svc.Edit(f.ctx, staff, r.ID, Input{TypeID: r.TypeID, Start: r.Start, End: r.End, Note: r.Note, HalfEnd: true}); err != nil {
		t.Fatal(err)
	}
	if len(f.events) != 0 {
		t.Fatalf("an unchanged edit emitted %+v", f.events)
	}
	// And the flag can be taken off again.
	e, err = f.svc.Edit(f.ctx, staff, r.ID, Input{TypeID: r.TypeID, Start: r.Start, End: r.End, Note: r.Note})
	if err != nil || e.Days != WholeDays(2) || e.HalfEnd {
		t.Fatalf("flag removed: %+v, %v", e, err)
	}
}

// Swapping which end is the half day leaves the days at 1.5, so only the flags
// distinguish the two versions: the fingerprint must still change, or an
// approver who read "first day half" would approve "last day half" unseen.
func TestEditSwappingTheHalfDayChangesTheFingerprint(t *testing.T) {
	f := newFixture(t)
	r := f.mustSubmitHalf("staff", "annual", "2026-10-12", "2026-10-13", true, false)
	seen := r.Fingerprint()
	e, err := f.svc.Edit(f.ctx, f.people["staff"], r.ID, Input{TypeID: r.TypeID, Start: r.Start, End: r.End, Note: r.Note, HalfEnd: true})
	if err != nil {
		t.Fatal(err)
	}
	if e.Days != 150 || e.HalfStart || !e.HalfEnd {
		t.Fatalf("edited = %s days half_start=%v half_end=%v", e.Days, e.HalfStart, e.HalfEnd)
	}
	if e.Fingerprint() == seen {
		t.Fatal("swapping the half day kept the fingerprint")
	}
	if _, err := f.svc.DecideSeen(f.ctx, f.people["head"], r.ID, true, "", seen); !errors.Is(err, ErrChanged) {
		t.Fatalf("deciding on the old fingerprint: %v", err)
	}
}

// A refusal about the request itself (not one box) is reported once, under the
// first ticked box.
func TestHalfRefusalAboutTheTypeIsReportedOnce(t *testing.T) {
	f := newFixture(t)
	_, err := f.submitHalf("staff", "maternity", "2026-10-12", "2026-10-14", true, true)
	var fe FieldErrors
	if !errors.As(err, &fe) || fe["half_start"] == "" || fe["half_end"] != "" || len(fe) != 1 {
		t.Fatalf("both boxes on a calendar-unit type: %v", err)
	}
	_, err = f.submitHalf("staff", "maternity", "2026-10-12", "2026-10-14", false, true)
	if !errors.As(err, &fe) || fe["half_end"] == "" || fe["half_start"] != "" {
		t.Fatalf("only half_end on a calendar-unit type: %v", err)
	}
}

// An edit that shrinks the request to one day cannot keep half_end.
func TestEditToOneDayRefusesHalfEnd(t *testing.T) {
	f := newFixture(t)
	r := f.mustSubmitHalf("staff", "annual", "2026-10-12", "2026-10-13", false, true)
	_, err := f.svc.Edit(f.ctx, f.people["staff"], r.ID, Input{TypeID: r.TypeID, Start: "2026-10-12", End: "2026-10-12", Note: "n", HalfEnd: true})
	if !fieldErr(err, "half_end") {
		t.Fatalf("err = %v", err)
	}
}

// A cancellation copies the flags of the leave it cancels, so approving it
// gives back exactly what was taken: 0.5, 1.5.
func TestCancellationGivesBackExactlyTheHalfDays(t *testing.T) {
	for _, tc := range []struct {
		name       string
		start, end string
		hs, he     bool
		days       string
	}{
		{"half a day", "2026-10-12", "2026-10-12", true, false, "0.5"},
		{"a day and a half", "2026-10-12", "2026-10-13", false, true, "1.5"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			r := f.mustSubmitHalf("staff", "annual", tc.start, tc.end, tc.hs, tc.he)
			if _, err := f.svc.Decide(f.ctx, f.people["head"], r.ID, true, ""); err != nil {
				t.Fatal(err)
			}
			c, err := f.svc.RequestCancellation(f.ctx, f.people["staff"], r.ID, "plans changed")
			if err != nil {
				t.Fatal(err)
			}
			if c.Days.String() != tc.days || c.HalfStart != tc.hs || c.HalfEnd != tc.he {
				t.Fatalf("cancellation = %s days half_start=%v half_end=%v", c.Days, c.HalfStart, c.HalfEnd)
			}
			if _, err := f.svc.Decide(f.ctx, f.people["head"], c.ID, true, ""); err != nil {
				t.Fatal(err)
			}
			if got := f.availableDays("staff", "annual"); got != WholeDays(15) {
				t.Fatalf("available after cancelling = %s, want 15", got)
			}
		})
	}
}

func TestRecordLeaveWithHalfDays(t *testing.T) {
	f := newFixture(t)
	// Mon 22 - Tue 23 June 2026, in the past: the log's "1 and 1/2 days".
	r, err := f.svc.RecordLeave(f.ctx, f.people["hr"], f.people["staff"].ID, Input{
		TypeID: f.types["annual"].ID, Start: "2026-06-22", End: "2026-06-23", Note: "from the log", HalfEnd: true})
	if err != nil {
		t.Fatal(err)
	}
	if r.Days != 150 || !r.HalfEnd || r.HalfStart || r.Status != StatusApproved {
		t.Fatalf("recorded = %s days, %+v", r.Days, r)
	}
	if got := f.availableDays("staff", "annual"); got.String() != "13.5" {
		t.Fatalf("available = %s, want 13.5", got)
	}
	// A recorded half day on a day that costs nothing (Tue 16 June, Youth Day)
	// is refused like any other.
	_, err = f.svc.RecordLeave(f.ctx, f.people["hr"], f.people["staff"].ID, Input{
		TypeID: f.types["annual"].ID, Start: "2026-06-16", End: "2026-06-16", Note: "x", HalfStart: true})
	if !fieldErr(err, "half_start") {
		t.Fatalf("half day on Youth Day: %v", err)
	}
}

// A request HR allowed to go below zero keeps its half day when the person
// submits the draft; it is not recomputed as whole days.
func TestHalfDayRequestSurvivesTheHRDraftRoundTrip(t *testing.T) {
	f := newFixture(t)
	// Family leave allows 3 days; 3.5 is over by half a day.
	r, err := f.svc.Submit(f.ctx, f.people["staff"], Input{TypeID: f.types["family"].ID,
		Start: "2026-10-12", End: "2026-10-15", Note: "wedding abroad", HalfEnd: true, Negative: true})
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusAwaitingHR || r.Days != 350 || !r.HalfEnd {
		t.Fatalf("awaiting = %s days, %+v", r.Days, r)
	}
	if _, err := f.svc.GrantNegative(f.ctx, f.people["hr"], r.ID, "once"); err != nil {
		t.Fatal(err)
	}
	r, err = f.svc.SubmitDraft(f.ctx, f.people["staff"], r.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusPending || r.Days != 350 || !r.HalfEnd {
		t.Fatalf("submitted = %s days, %+v", r.Days, r)
	}
}

// Shutdown days are mandatory and whole: a draft that somehow carries a flag is
// refused rather than submitted with half days.
func TestShutdownDraftsNeverHaveHalfDays(t *testing.T) {
	f := newFixture(t)
	f.december(false)
	d := f.drafts("staff")[0]
	if d.HalfStart || d.HalfEnd {
		t.Fatalf("a generated draft has half days: %+v", d)
	}
	if _, err := f.db.Exec(`UPDATE requests SET half_start = 1 WHERE id = ?`, d.ID); err != nil {
		t.Fatal(err)
	}
	_, err := f.svc.SubmitDraft(f.ctx, f.people["staff"], d.ID, "")
	if !fieldErr(err, "half_start") {
		t.Fatalf("submitting a shutdown draft with a half day: %v", err)
	}
}

// Two half days on the same date are still an overlap: a half day occupies the
// date (allowing two is out of scope).
func TestTwoHalfDaysOnOneDateOverlap(t *testing.T) {
	f := newFixture(t)
	f.mustSubmitHalf("staff", "annual", "2026-10-12", "2026-10-12", true, false)
	if _, err := f.submitHalf("staff", "annual", "2026-10-12", "2026-10-12", true, false); !fieldErr(err, "start") {
		t.Fatalf("second half day on the same date: %v", err)
	}
}

func TestHalfNote(t *testing.T) {
	for _, tc := range []struct {
		r    Request
		want string
	}{
		{Request{Start: "2026-10-12", End: "2026-10-12"}, ""},
		{Request{Start: "2026-10-12", End: "2026-10-12", HalfStart: true}, "half day"},
		{Request{Start: "2026-10-12", End: "2026-10-14", HalfStart: true}, "first day a half day"},
		{Request{Start: "2026-10-12", End: "2026-10-14", HalfEnd: true}, "last day a half day"},
		{Request{Start: "2026-10-12", End: "2026-10-14", HalfStart: true, HalfEnd: true}, "first and last days are half days"},
	} {
		if got := tc.r.HalfNote(); got != tc.want {
			t.Errorf("HalfNote(%+v) = %q, want %q", tc.r, got, tc.want)
		}
	}
}

// Sanity caps: a slipped decimal point is refused, not saved.
func TestDaysSanityCaps(t *testing.T) {
	f := newFixture(t)
	adj := func(d Days) error {
		return f.svc.AddAdjustment(f.ctx, f.people["hr"], Adjustment{UserID: f.people["staff"].ID,
			TypeID: f.types["annual"].ID, CycleStart: "2026-01-01", Days: d, Reason: "payroll"})
	}
	for _, d := range []Days{100001, -100001} {
		if err := adj(d); !fieldErr(err, "days") {
			t.Errorf("adjustment of %s: %v", d, err)
		}
	}
	for _, d := range []Days{100000, -100000} {
		if err := adj(d); err != nil {
			t.Errorf("adjustment of exactly 1000 days (%s) refused: %v", d, err)
		}
	}

	ty := f.types["annual"]
	over := Days(100001)
	ty.Allowance = &over
	if _, err := f.svc.SaveType(f.ctx, ty); !fieldErr(err, "allowance") {
		t.Fatalf("allowance of 1000.01 days: %v", err)
	}
	ok := Days(100000)
	ty.Allowance = &ok
	if _, err := f.svc.SaveType(f.ctx, ty); err != nil {
		t.Fatalf("allowance of 1000 days: %v", err)
	}
}

func TestDaysUnmarshalJSONStringDecodesEscapes(t *testing.T) {
	var d Days
	// \u0031 is "1": only a real JSON string decode turns this into 1.5; code
	// that just slices off the quotes would hand ParseDays a backslash.
	if err := d.UnmarshalJSON([]byte(`"\u0031.5"`)); err != nil || d != 150 {
		t.Fatalf("escaped string: %v, %v", d, err)
	}
	if err := d.UnmarshalJSON([]byte(`"1\"5"`)); err == nil {
		t.Fatal("a string with an escaped quote inside was accepted")
	}
	if err := d.UnmarshalJSON([]byte(`"1.5`)); err == nil {
		t.Fatal("unterminated string accepted")
	}
}
