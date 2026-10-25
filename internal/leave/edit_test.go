package leave

import (
	"errors"
	"strings"
	"testing"
)

func (f *fixture) edit(who string, id int64, code, start, end, note string) (Request, error) {
	return f.svc.Edit(f.ctx, f.people[who], id, Input{TypeID: f.types[code].ID, Start: start, End: end, Note: note})
}

func (f *fixture) mustEdit(who string, id int64, code, start, end, note string) Request {
	f.t.Helper()
	r, err := f.edit(who, id, code, start, end, note)
	if err != nil {
		f.t.Fatalf("edit: %v", err)
	}
	return r
}

func TestEditEligibility(t *testing.T) {
	f := newFixture(t)
	pending := f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-14")
	for who, want := range map[string]bool{"staff": true, "head": false, "hr": false, "pastor": false} {
		got, err := f.svc.CanEdit(f.ctx, f.people[who], pending)
		if err != nil || got != want {
			t.Errorf("CanEdit(%s, pending) = %v, %v; want %v", who, got, err, want)
		}
		if who != "staff" {
			// The approver and an admin can see it, and still cannot edit it.
			if _, err := f.edit(who, pending.ID, "annual", "2026-10-13", "2026-10-14", "n"); !errors.Is(err, ErrNotAllowed) {
				t.Errorf("%s edited someone else's request: %v", who, err)
			}
		}
	}

	// Every other state refuses, and nothing changes.
	set := func(id int64, q string, args ...any) {
		t.Helper()
		if _, err := f.db.Exec(`UPDATE requests SET `+q+` WHERE id = ?`, append(args, id)...); err != nil {
			t.Fatal(err)
		}
	}
	for _, status := range []string{StatusApproved, StatusDeclined, StatusDraft, StatusAwaitingHR, StatusWithdrawn, StatusCancelled} {
		r := f.mustSubmit("staff", "annual", "2026-11-02", "2026-11-03")
		set(r.ID, "status = ?", status)
		if _, err := f.edit("staff", r.ID, "annual", "2026-11-04", "2026-11-04", "n"); !errors.Is(err, ErrNotAllowed) {
			t.Errorf("edit of a %s request: %v", status, err)
		}
		got, _ := f.svc.Get(f.ctx, r.ID)
		if got.Start != "2026-11-02" {
			t.Errorf("%s request changed: %+v", status, got)
		}
		set(r.ID, "status = 'withdrawn'") // out of the way of the next one
	}

	rec := f.mustSubmit("staff", "annual", "2026-11-09", "2026-11-10")
	set(rec.ID, "recorded_by_id = ?", f.people["hr"].ID)
	if ok, _ := f.svc.CanEdit(f.ctx, f.people["staff"], mustGet(f, rec.ID)); ok {
		t.Error("recorded leave can be edited")
	}
	set(rec.ID, "status = 'withdrawn'")

	// A cancellation request is not leave to edit.
	appr := f.mustSubmit("staff", "annual", "2026-11-16", "2026-11-17")
	if _, err := f.svc.Decide(f.ctx, f.people["head"], appr.ID, true, ""); err != nil {
		t.Fatal(err)
	}
	c, err := f.svc.RequestCancellation(f.ctx, f.people["staff"], appr.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if ok, _ := f.svc.CanEdit(f.ctx, f.people["staff"], c); ok {
		t.Error("a cancellation request can be edited")
	}

	// A shutdown request: its dates are mandatory.
	f.december(false)
	if ds := f.drafts("staff"); len(ds) > 0 {
		set(ds[0].ID, "status = 'pending'")
		if _, err := f.edit("staff", ds[0].ID, "annual", "2026-12-21", "2026-12-21", "n"); !errors.Is(err, ErrNotAllowed) {
			t.Errorf("shutdown request edited: %v", err)
		}
	} else {
		t.Error("no shutdown draft to test with")
	}

	// A decision step (an escalated request one level in): withdraw and reapply.
	esc := f.mustSubmit("staff", "annual", "2026-11-23", "2026-11-24")
	set(esc.ID, "escalate = 1")
	if _, err := f.db.Exec(`INSERT INTO request_steps (request_id, approver_id, decision, decided_at)
		VALUES (?, ?, 'approved', '2026-10-05T08:00:00Z')`, esc.ID, f.people["head"].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.edit("staff", esc.ID, "annual", "2026-11-25", "2026-11-25", "n"); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("request with a decision step edited: %v", err)
	}
}

func mustGet(f *fixture, id int64) Request {
	f.t.Helper()
	r, err := f.svc.Get(f.ctx, id)
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

func TestEditKeepsApproverAndPlace(t *testing.T) {
	f := newFixture(t)
	r := f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-14")
	// The manager changes after the request was sent: the request stays put.
	if err := f.svc.SetOverride(f.ctx, f.people["staff"].ID, f.people["pastor"].ID); err != nil {
		t.Fatal(err)
	}
	f.events = nil
	got := f.mustEdit("staff", r.ID, "annual", "2026-10-13", "2026-10-16", "new note")
	if got.ApproverID != f.people["head"].ID || got.Status != StatusPending || got.CreatedAt != r.CreatedAt {
		t.Fatalf("approver/status/created_at moved: %+v", got)
	}
	if got.Start != "2026-10-13" || got.End != "2026-10-16" || got.Days != 4 || got.Note != "new note" {
		t.Fatalf("not saved: %+v", got)
	}
	if len(f.events) != 1 || f.events[0].Kind != EventEdited || f.events[0].Before.Start != "2026-10-12" ||
		f.events[0].Request.Start != "2026-10-13" || f.events[0].Actor.ID != f.people["staff"].ID {
		t.Fatalf("events = %+v", f.events)
	}
}

func TestEditOverlapAndBalanceDoNotCountItself(t *testing.T) {
	f := newFixture(t)
	// 15 days of annual leave: take 10 of them, then edit onto an overlapping
	// range that needs all 15. Counting itself twice would refuse it.
	r := f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-23") // 10 working days
	if got := f.available("staff", "annual"); got != 5 {
		t.Fatalf("available = %d", got)
	}
	// Onto its own old days, plus more.
	f.mustEdit("staff", r.ID, "annual", "2026-10-12", "2026-10-30", "n") // 15 days
	if got := f.available("staff", "annual"); got != 0 {
		t.Fatalf("available after = %d", got)
	}
	// Exactly its own old dates, unchanged but for the note.
	got := f.mustEdit("staff", r.ID, "annual", "2026-10-12", "2026-10-30", "other note")
	if got.Days != 15 || got.Note != "other note" {
		t.Fatalf("note-only edit = %+v", got)
	}
	// One more day than there is is still refused, with the HR pointer.
	_, err := f.edit("staff", r.ID, "annual", "2026-10-12", "2026-11-02", "n")
	var fe FieldErrors
	if !errors.As(err, &fe) || !strings.Contains(fe["end"], "you have 15 left") || !strings.Contains(fe["end"], "withdraw this request and ask HR") {
		t.Fatalf("over the balance: %v", err)
	}
	if mustGet(f, r.ID).End != "2026-10-30" {
		t.Fatal("a refused edit changed the request")
	}
	// Ignoring Negative: an edit never moves a pending request to HR.
	_, err = f.svc.Edit(f.ctx, f.people["staff"], r.ID, Input{TypeID: f.types["annual"].ID, Start: "2026-10-12", End: "2026-11-02", Note: "n", Negative: true})
	if !fieldErr(err, "end") {
		t.Fatalf("Negative on an edit: %v", err)
	}
	if got := mustGet(f, r.ID); got.Status != StatusPending {
		t.Fatalf("status = %s", got.Status)
	}

	// A neighbour's request (a different one of their own) still blocks.
	other := f.mustSubmit("staff", "unpaid", "2026-11-09", "2026-11-09")
	_ = other
	if _, err := f.edit("staff", r.ID, "annual", "2026-10-30", "2026-11-09", "n"); !fieldErr(err, "start") {
		t.Fatalf("onto a neighbour: %v", err)
	}
}

func TestEditCoverageAndOverride(t *testing.T) {
	f := newFixture(t)
	f.dept("Ops", "staff", "hr", "head", "orphan")
	f.rule("Ops", 50)
	f.mustSubmit("hr", "annual", "2026-10-12", "2026-10-14")
	f.mustSubmit("head", "annual", "2026-10-12", "2026-10-14")
	r := f.mustSubmit("staff", "annual", "2026-10-19", "2026-10-21")

	// Moving onto days the department is already at its limit: refused like a submit.
	_, err := f.edit("staff", r.ID, "annual", "2026-10-13", "2026-10-13", "n")
	if !fieldErr(err, "coverage") {
		t.Fatalf("over the limit: %v", err)
	}
	if got := mustGet(f, r.ID); got.Start != "2026-10-19" || got.Escalate {
		t.Fatalf("refused edit changed it: %+v", got)
	}
	// Asking for special permission, as the override button does, allows it and escalates.
	got, err := f.svc.Edit(f.ctx, f.people["staff"], r.ID, Input{TypeID: f.types["annual"].ID, Start: "2026-10-13", End: "2026-10-13", Note: "n", Override: true})
	if err != nil || !got.Escalate || got.Start != "2026-10-13" || got.ApproverID != f.people["head"].ID {
		t.Fatalf("override edit = %+v, %v", got, err)
	}
	// Back to quiet days: no longer over a limit, so no longer escalated, even
	// if override is still set.
	got, err = f.svc.Edit(f.ctx, f.people["staff"], r.ID, Input{TypeID: f.types["annual"].ID, Start: "2026-10-19", End: "2026-10-19", Note: "n", Override: true})
	if err != nil || got.Escalate {
		t.Fatalf("back under the limit = %+v, %v", got, err)
	}
}

func TestEditEscalatedClearsWithoutOverride(t *testing.T) {
	f := newFixture(t)
	f.dept("Ops", "staff", "hr", "head", "orphan")
	f.rule("Ops", 50)
	f.mustSubmit("hr", "annual", "2026-10-12", "2026-10-14")
	f.mustSubmit("head", "annual", "2026-10-12", "2026-10-14")
	r, err := f.override("staff", "annual", "2026-10-13", "2026-10-13")
	if err != nil || !r.Escalate {
		t.Fatalf("setup: %+v %v", r, err)
	}
	// Same days, no override this time: over the limit, so refused.
	if _, err := f.edit("staff", r.ID, "annual", "2026-10-13", "2026-10-13", "changed"); !fieldErr(err, "coverage") {
		t.Fatalf("no override: %v", err)
	}
	got := f.mustEdit("staff", r.ID, "annual", "2026-10-20", "2026-10-20", "quiet")
	if got.Escalate {
		t.Fatal("still escalated on a quiet day")
	}
}

func TestEditTypeBackdateAndNote(t *testing.T) {
	f := newFixture(t)
	r := f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-13")
	before := f.available("staff", "annual")

	got := f.mustEdit("staff", r.ID, "unpaid", "2026-10-12", "2026-10-13", "n")
	if got.TypeID != f.types["unpaid"].ID {
		t.Fatalf("type = %d", got.TypeID)
	}
	// The balance moved with it: annual is untouched by an unpaid request.
	if after := f.available("staff", "annual"); after <= before {
		t.Errorf("annual available %d -> %d after changing type", before, after)
	}
	// The backdating limit of the new type applies.
	if _, err := f.edit("staff", r.ID, "annual", "2026-06-01", "2026-06-02", "n"); !fieldErr(err, "start") {
		t.Fatalf("backdated: %v", err)
	}
	// A type needing a note refuses a blank one.
	if _, err := f.edit("staff", r.ID, "family", "2026-10-12", "2026-10-13", ""); !fieldErr(err, "note") {
		t.Fatalf("blank note for a type that needs one: %v", err)
	}
	// Note-only, and no change at all.
	f.events = nil
	f.mustEdit("staff", r.ID, "unpaid", "2026-10-12", "2026-10-13", "just the note")
	if len(f.events) != 1 {
		t.Fatalf("note edit events = %d", len(f.events))
	}
	f.events = nil
	f.mustEdit("staff", r.ID, "unpaid", "2026-10-12", "2026-10-13", "just the note")
	if len(f.events) != 0 {
		t.Fatal("an edit that changed nothing emailed the approver")
	}
}

// A decision landing between Edit's first read and its transaction wins: the
// edit fails and the request is as the approver decided it.
func TestEditLosesToADecision(t *testing.T) {
	f := newFixture(t)
	r := f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-14")
	f.svc.beforeEditTx = func() {
		if _, err := f.svc.Decide(f.ctx, f.people["head"], r.ID, true, ""); err != nil {
			t.Error(err)
		}
	}
	f.events = nil
	if _, err := f.edit("staff", r.ID, "annual", "2026-10-19", "2026-10-21", "n"); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("edit after a decision: %v", err)
	}
	got := mustGet(f, r.ID)
	if got.Status != StatusApproved || got.Start != "2026-10-12" {
		t.Fatalf("request = %+v", got)
	}
	for _, e := range f.events {
		if e.Kind == EventEdited {
			t.Fatal("an edited event was emitted")
		}
	}
}

func TestPreviewEditExcludesOnlyYourOwnEditableRequest(t *testing.T) {
	f := newFixture(t)
	mine := f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-14")
	theirs := f.mustSubmit("hr", "annual", "2026-10-19", "2026-10-21")
	in := Input{TypeID: f.types["annual"].ID, Start: "2026-10-12", End: "2026-10-14"}

	if _, err := f.svc.Preview(f.ctx, f.people["staff"], in); !fieldErr(err, "start") {
		t.Fatalf("plain preview should see the overlap: %v", err)
	}
	qt, editing, err := f.svc.PreviewEdit(f.ctx, f.people["staff"], mine.ID, in)
	if err != nil || !editing || qt.Days != 3 {
		t.Fatalf("own id: %+v %v %v", qt, editing, err)
	}
	// Someone else's id, a made-up one, and a non-editable one exclude nothing:
	// staff previewing hr's id still collides with their own request.
	if _, editing, err := f.svc.PreviewEdit(f.ctx, f.people["staff"], theirs.ID, in); editing || !fieldErr(err, "start") {
		t.Fatalf("someone else's id honoured: %v %v", editing, err)
	}
	f.mustSubmit("hr", "annual", "2026-10-12", "2026-10-14")
	if _, editing, err := f.svc.PreviewEdit(f.ctx, f.people["hr"], mine.ID, in); editing || !fieldErr(err, "start") {
		t.Fatalf("someone else's id excluded something: %v %v", editing, err)
	}
	if _, editing, _ := f.svc.PreviewEdit(f.ctx, f.people["staff"], 9999, in); editing {
		t.Fatal("unknown id honoured")
	}
	if _, err := f.svc.Decide(f.ctx, f.people["head"], mine.ID, true, ""); err != nil {
		t.Fatal(err)
	}
	if _, editing, err := f.svc.PreviewEdit(f.ctx, f.people["staff"], mine.ID, in); editing || !fieldErr(err, "start") {
		t.Fatalf("decided request honoured: %v %v", editing, err)
	}
}

// An approver decides what they were shown. Each of these is a way a decision
// could otherwise land on a request that changed under it.
func TestDecideSeenRefusesAChangedRequest(t *testing.T) {
	f := newFixture(t)
	head := f.people["head"]
	r := f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-12") // 1 day
	seen := r.Fingerprint()
	f.mustEdit("staff", r.ID, "annual", "2026-10-12", "2026-10-30", "n") // 15 days, after the page loaded
	if mustGet(f, r.ID).Fingerprint() == seen {
		t.Fatal("an edit left the fingerprint alone")
	}
	for _, approve := range []bool{true, false} {
		if _, err := f.svc.DecideSeen(f.ctx, head, r.ID, approve, "no", seen); !errors.Is(err, ErrChanged) {
			t.Fatalf("approve=%v on a stale fingerprint: %v", approve, err)
		}
	}
	if got := mustGet(f, r.ID); got.Status != StatusPending || got.DecidedByID != 0 {
		t.Fatalf("a refused decision changed the request: %+v", got)
	}
	if _, err := f.svc.DecideOrdinary(f.ctx, head, r.ID, seen); !errors.Is(err, ErrChanged) {
		t.Fatalf("bulk on a stale fingerprint: %v", err)
	}
	// The fresh one works, and an empty one (MCP without expected) behaves as before.
	if _, err := f.svc.DecideSeen(f.ctx, head, r.ID, true, "", mustGet(f, r.ID).Fingerprint()); err != nil {
		t.Fatal(err)
	}
	r2 := f.mustSubmit("staff", "unpaid", "2026-11-09", "2026-11-09")
	if _, err := f.svc.DecideSeen(f.ctx, head, r2.ID, true, "", ""); err != nil {
		t.Fatalf("no fingerprint: %v", err)
	}
}

// An edit that flips escalate between Decide's read and its transaction: a
// plain approval must not become an override, nor an override a final approval.
func TestDecideRefusesEscalateFlippedInTheGap(t *testing.T) {
	for _, becomes := range []bool{true, false} {
		f := newFixture(t)
		r := f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-14")
		if !becomes {
			f.exec(`UPDATE requests SET escalate = 1 WHERE id = ?`, r.ID)
		}
		f.svc.beforeDecideTx = func() { f.exec(`UPDATE requests SET escalate = ? WHERE id = ?`, becomes, r.ID) }
		// No fingerprint: this is the in-transaction check on its own.
		if _, err := f.svc.Decide(f.ctx, f.people["head"], r.ID, true, ""); !errors.Is(err, ErrChanged) {
			t.Fatalf("escalate -> %v: %v", becomes, err)
		}
		got := mustGet(f, r.ID)
		if got.Status != StatusPending || got.ApproverID != f.people["head"].ID {
			t.Fatalf("request moved: %+v", got)
		}
		if steps, _ := f.svc.Steps(f.ctx, r.ID); len(steps) != 0 {
			t.Fatalf("a step was logged: %v", steps)
		}
	}
}
