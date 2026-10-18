package leave

import (
	"testing"
	"time"
)

// use up staff's annual leave: 15 working days, 12 to 30 Oct.
func (f *fixture) exhaustAnnual() {
	f.t.Helper()
	r := f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-30")
	if _, err := f.svc.Decide(f.ctx, f.people["head"], r.ID, true, ""); err != nil {
		f.t.Fatal(err)
	}
	if got := f.available("staff", "annual"); got != 0 {
		f.t.Fatalf("available after using it all = %d", got)
	}
}

func (f *fixture) negative(who, code, start, end, note string) (Request, error) {
	return f.svc.Submit(f.ctx, f.people[who], Input{TypeID: f.types[code].ID, Start: start, End: end, Note: note, Negative: true})
}

func TestNegativeBalanceFlow(t *testing.T) {
	f := newFixture(t)
	f.exhaustAnnual()

	// An ordinary request still cannot go below zero, and says by how much.
	if _, err := f.submit("staff", "annual", "2026-11-02", "2026-11-04"); !fieldErr(err, "end") {
		t.Fatalf("overdrawn request: %v", err)
	}
	qt, _ := f.svc.Preview(f.ctx, f.people["staff"], Input{TypeID: f.types["annual"].ID, Start: "2026-11-02", End: "2026-11-04"})
	if qt.Shortfall != 3 {
		t.Fatalf("shortfall = %d", qt.Shortfall)
	}

	// Asking HR needs a reason.
	if _, err := f.negative("staff", "annual", "2026-11-02", "2026-11-04", " "); !fieldErr(err, "note") {
		t.Fatalf("no reason: %v", err)
	}
	f.events = nil
	r, err := f.negative("staff", "annual", "2026-11-02", "2026-11-04", "family wedding abroad")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusAwaitingHR || r.ApproverID != 0 || r.AllowNegative {
		t.Fatalf("awaiting request = %+v", r)
	}
	if len(f.events) != 1 || f.events[0].Kind != EventNegativeRequested || f.events[0].Shortfall != 3 {
		t.Fatalf("events = %+v", f.events)
	}
	// It reserves nothing, but it does occupy the dates.
	if got := f.available("staff", "annual"); got != 0 {
		t.Fatalf("awaiting HR reserved balance: %d", got)
	}
	if _, err := f.submit("staff", "annual", "2026-11-03", "2026-11-03"); !fieldErr(err, "start") {
		t.Fatalf("overlap with a request awaiting HR: %v", err)
	}
	if short, err := f.svc.Shortfall(f.ctx, r); err != nil || short != 3 {
		t.Fatalf("shortfall of the awaiting request = %d, %v", short, err)
	}
	if hr, _ := f.svc.AwaitingHR(f.ctx); len(hr) != 1 || hr[0].ID != r.ID {
		t.Fatalf("AwaitingHR = %+v", hr)
	}

	// Nobody can decide it through the approval chain, and only an admin can grant.
	if _, err := f.svc.Decide(f.ctx, f.people["head"], r.ID, true, ""); err != ErrNotAllowed {
		t.Fatalf("head approving a request awaiting HR: %v", err)
	}
	for _, who := range []string{"staff", "head", "pastor"} {
		if _, err := f.svc.GrantNegative(f.ctx, f.people[who], r.ID, ""); err != ErrNotAllowed {
			t.Errorf("%s granting: %v", who, err)
		}
		if _, err := f.svc.RefuseNegative(f.ctx, f.people[who], r.ID, "no"); err != ErrNotAllowed {
			t.Errorf("%s refusing: %v", who, err)
		}
	}

	f.events = nil
	r, err = f.svc.GrantNegative(f.ctx, f.people["hr"], r.ID, "fine, once")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusDraft || !r.AllowNegative || r.IsShutdown() {
		t.Fatalf("granted request = %+v", r)
	}
	if len(f.events) != 1 || f.events[0].Kind != EventNegativeGranted || f.events[0].Note != "fine, once" {
		t.Fatalf("events = %+v", f.events)
	}
	if _, err := f.svc.GrantNegative(f.ctx, f.people["hr"], r.ID, ""); err != ErrNotAllowed {
		t.Fatalf("granting twice: %v", err)
	}
	if ds := f.drafts("staff"); len(ds) != 1 || ds[0].ID != r.ID {
		t.Fatalf("drafts = %+v", ds)
	}
	if got := f.available("staff", "annual"); got != 0 {
		t.Fatalf("a draft reserved balance: %d", got)
	}

	// Submitting it follows the normal chain, and the balance ends below zero.
	// The reason stays when the person adds nothing.
	f.events = nil
	r, err = f.svc.SubmitDraft(f.ctx, f.people["staff"], r.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusPending || r.ApproverID != f.people["head"].ID || r.Note != "family wedding abroad" || r.Escalate {
		t.Fatalf("submitted request = %+v", r)
	}
	if len(f.events) != 1 || f.events[0].Kind != EventSubmitted {
		t.Fatalf("events = %+v", f.events)
	}
	if got := f.available("staff", "annual"); got != -3 {
		t.Fatalf("available while pending = %d, want -3", got)
	}
	if r, err = f.svc.Decide(f.ctx, f.people["head"], r.ID, true, ""); err != nil || r.Status != StatusApproved {
		t.Fatalf("approval: %+v %v", r, err)
	}
	if got := f.available("staff", "annual"); got != -3 {
		t.Fatalf("available once approved = %d, want -3", got)
	}
	steps, _ := f.svc.Steps(f.ctx, r.ID)
	if len(steps) != 1 || steps[0].Decision != "granted" || steps[0].ApproverName != "hr" {
		t.Fatalf("steps = %+v", steps)
	}
}

func TestNegativeRefused(t *testing.T) {
	f := newFixture(t)
	f.exhaustAnnual()
	r, err := f.negative("staff", "annual", "2026-11-02", "2026-11-04", "wedding")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.RefuseNegative(f.ctx, f.people["hr"], r.ID, " "); !fieldErr(err, "note") {
		t.Fatalf("refusing without a note: %v", err)
	}
	f.events = nil
	r, err = f.svc.RefuseNegative(f.ctx, f.people["hr"], r.ID, "not this year")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusDeclined || r.DecisionNote != "not this year" || r.DecidedBy != "hr" {
		t.Fatalf("refused request = %+v", r)
	}
	// Told as for any declined request.
	if len(f.events) != 1 || f.events[0].Kind != EventDecided {
		t.Fatalf("events = %+v", f.events)
	}
	if steps, _ := f.svc.Steps(f.ctx, r.ID); len(steps) != 1 || steps[0].Decision != "refused" {
		t.Fatalf("steps = %+v", steps)
	}
	if _, err := f.svc.GrantNegative(f.ctx, f.people["hr"], r.ID, ""); err != ErrNotAllowed {
		t.Fatalf("granting a refused request: %v", err)
	}
}

// Asking HR when the balance is fine is just an ordinary submit.
func TestNegativeNotNeededSubmitsNormally(t *testing.T) {
	f := newFixture(t)
	r, err := f.negative("staff", "annual", "2026-10-12", "2026-10-12", "")
	if err != nil || r.Status != StatusPending || r.ApproverID != f.people["head"].ID {
		t.Fatalf("%+v %v", r, err)
	}
	// And it does not let a request past a coverage limit.
	f.dept("Pair", "head", "hr")
	f.rule("Pair", 50)
	f.mustSubmit("head", "annual", "2026-10-13", "2026-10-13")
	if _, err := f.negative("hr", "annual", "2026-10-13", "2026-10-13", "n"); !fieldErr(err, "coverage") {
		t.Fatalf("negative skipped coverage: %v", err)
	}
}

func TestNegativeWithdraw(t *testing.T) {
	f := newFixture(t)
	f.exhaustAnnual()
	r, _ := f.negative("staff", "annual", "2026-11-02", "2026-11-04", "wedding")
	if err := f.svc.Withdraw(f.ctx, f.people["hr"], r.ID); err != ErrNotAllowed {
		t.Fatalf("someone else withdrawing: %v", err)
	}
	if err := f.svc.Withdraw(f.ctx, f.people["staff"], r.ID); err != nil {
		t.Fatalf("withdrawing while awaiting HR: %v", err)
	}

	// A granted draft is the owner's to drop.
	r, _ = f.negative("staff", "annual", "2026-11-02", "2026-11-04", "wedding")
	r, _ = f.svc.GrantNegative(f.ctx, f.people["hr"], r.ID, "")
	if !CanWithdraw(f.people["staff"], r) {
		t.Fatal("a granted draft cannot be withdrawn")
	}
	if err := f.svc.Withdraw(f.ctx, f.people["staff"], r.ID); err != nil {
		t.Fatalf("withdrawing a granted draft: %v", err)
	}
	if got, _ := f.svc.Get(f.ctx, r.ID); got.Status != StatusWithdrawn {
		t.Fatalf("status = %s", got.Status)
	}

	// A shutdown draft is not.
	f.december(false)
	d := f.drafts("staff")[0]
	if CanWithdraw(f.people["staff"], d) || f.svc.Withdraw(f.ctx, f.people["staff"], d.ID) != ErrNotAllowed {
		t.Fatal("a shutdown draft was withdrawable")
	}
}

// A granted draft is an ordinary request apart from the balance: coverage and
// the date rules apply, and special permission is on offer if it fails.
func TestNegativeDraftStillSubjectToCoverage(t *testing.T) {
	f := newFixture(t)
	f.exhaustAnnual()
	r, _ := f.negative("staff", "annual", "2026-11-02", "2026-11-04", "wedding")
	r, _ = f.svc.GrantNegative(f.ctx, f.people["hr"], r.ID, "")

	f.dept("Pair", "staff", "hr")
	f.rule("Pair", 50)
	f.mustSubmit("hr", "annual", "2026-11-02", "2026-11-04")

	if _, err := f.svc.SubmitDraft(f.ctx, f.people["staff"], r.ID, ""); !fieldErr(err, "coverage") {
		t.Fatalf("granted draft skipped coverage: %v", err)
	}
	r, err := f.svc.SubmitDraftOverride(f.ctx, f.people["staff"], r.ID, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Escalate || r.Status != StatusPending || r.ApproverID != f.people["head"].ID {
		t.Fatalf("escalated draft = %+v", r)
	}
	// And the usual chain follows.
	r, _ = f.svc.Decide(f.ctx, f.people["head"], r.ID, true, "")
	if r.Status != StatusPending || r.ApproverID != f.people["pastor"].ID {
		t.Fatalf("not escalated: %+v", r)
	}
}

func TestNegativeDraftPastDatesRefused(t *testing.T) {
	f := newFixture(t)
	f.exhaustAnnual()
	r, _ := f.negative("staff", "annual", "2026-10-06", "2026-10-06", "wedding")
	r, _ = f.svc.GrantNegative(f.ctx, f.people["hr"], r.ID, "")
	f.svc.Now = func() time.Time { return time.Date(2026, 10, 20, 8, 0, 0, 0, time.UTC) }
	if _, err := f.svc.SubmitDraft(f.ctx, f.people["staff"], r.ID, ""); !fieldErr(err, "start") {
		t.Fatalf("a granted draft is not mandatory, so a past date is refused: %v", err)
	}
}
