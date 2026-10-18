package leave

import (
	"errors"
	"testing"

	"github.com/17xande-dev/leave/internal/directory"
)

// The December shutdown the tests use: a week charged to annual leave, then a
// week of company closure that deducts nothing.
func (f *fixture) december(auto bool) int64 {
	f.t.Helper()
	id, err := f.svc.CreateShutdown(f.ctx, f.people["hr"], ShutdownInput{
		Name: "December 2026", AutoApprove: auto,
		Parts: []PartInput{
			{TypeID: f.types["annual"].ID, Start: "2026-12-07", End: "2026-12-11"},  // 5 working days
			{TypeID: f.types["closure"].ID, Start: "2026-12-21", End: "2026-12-31"}, // 8: 25 Dec is a holiday
		},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}

func (f *fixture) drafts(who string) []Request {
	f.t.Helper()
	ds, err := f.svc.Drafts(f.ctx, f.people[who].ID)
	if err != nil {
		f.t.Fatal(err)
	}
	return ds
}

func TestShutdownCreatesDraftsForEveryone(t *testing.T) {
	f := newFixture(t)
	f.events = nil
	f.december(false)

	for name := range f.people {
		ds := f.drafts(name)
		if len(ds) != 2 || ds[0].Days != 5 || ds[1].Days != 8 || ds[1].TypeName != "Company closure" {
			t.Fatalf("%s's drafts = %+v", name, ds)
		}
	}
	// One email-worthy event per person, not one per draft.
	if len(f.events) != len(f.people) {
		t.Fatalf("%d events for %d people", len(f.events), len(f.people))
	}
	// A draft reserves nothing...
	if got := f.available("staff", "annual"); got != 15 {
		t.Fatalf("annual available with an unsubmitted draft = %d, want 15", got)
	}
	// ...but booking leave over it would charge the same days twice.
	if _, err := f.submit("staff", "annual", "2026-12-10", "2026-12-15"); !fieldErr(err, "start") {
		t.Fatalf("leave over a shutdown draft: %v", err)
	}
}

func TestGenerateDraftsIsIdempotentAndCatchesNewStarters(t *testing.T) {
	f := newFixture(t)
	id := f.december(false)
	if n, err := f.svc.GenerateDrafts(f.ctx, id); err != nil || n != 0 {
		t.Fatalf("second generate: %d, %v", n, err)
	}
	newbie, _ := f.us.Upsert(f.ctx, directory.User{ID: "CN=newbie", DN: "CN=newbie", Username: "newbie", Name: "New Starter", ManagerDN: "CN=head,OU=Staff"})
	if n, err := f.svc.GenerateDrafts(f.ctx, id); err != nil || n != 2 {
		t.Fatalf("generate after a new starter: %d, %v", n, err)
	}
	if ds, _ := f.svc.Drafts(f.ctx, newbie.ID); len(ds) != 2 {
		t.Fatalf("new starter has %d drafts", len(ds))
	}
}

func TestSubmitDraftFollowsTheChain(t *testing.T) {
	f := newFixture(t)
	f.december(false)
	annual := f.drafts("staff")[0]

	if _, err := f.svc.SubmitDraft(f.ctx, f.people["hr"], annual.ID, ""); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("submitting someone else's draft: %v", err)
	}
	if err := f.svc.Withdraw(f.ctx, f.people["staff"], annual.ID); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("withdrawing a draft: %v", err)
	}
	r, err := f.svc.SubmitDraft(f.ctx, f.people["staff"], annual.ID, "noted")
	if err != nil || r.Status != StatusPending || r.ApproverID != f.people["head"].ID {
		t.Fatalf("submitted = %+v, %v", r, err)
	}
	if got := f.available("staff", "annual"); got != 10 {
		t.Fatalf("annual available after submitting 5 = %d, want 10", got)
	}
	if _, err := f.svc.SubmitDraft(f.ctx, f.people["staff"], annual.ID, ""); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("submitting twice: %v", err)
	}
	// Mandatory: no withdrawing it, and no cancelling it once approved.
	if err := f.svc.Withdraw(f.ctx, f.people["staff"], annual.ID); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("withdrawing a shutdown request: %v", err)
	}
	f.svc.Decide(f.ctx, f.people["head"], annual.ID, true, "")
	if _, err := f.svc.RequestCancellation(f.ctx, f.people["staff"], annual.ID, ""); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("cancelling a shutdown request: %v", err)
	}

	// The closure part deducts nothing, although its type is not on the
	// apply form.
	closure := f.drafts("staff")[0]
	if _, err := f.svc.SubmitDraft(f.ctx, f.people["staff"], closure.ID, ""); err != nil {
		t.Fatalf("submitting the closure part: %v", err)
	}
	if got := f.available("staff", "annual"); got != 10 {
		t.Fatalf("closure days touched annual leave: %d", got)
	}
}

// The days are mandatory, so a short balance goes negative rather than
// blocking the submit.
func TestShutdownMayTakeBalanceNegative(t *testing.T) {
	f := newFixture(t)
	f.svc.AddAdjustment(f.ctx, f.people["hr"].ID, Adjustment{UserID: f.people["staff"].ID,
		TypeID: f.types["annual"].ID, CycleStart: "2026-01-01", Days: -12, Reason: "taken earlier"})
	f.december(false)
	if _, err := f.svc.SubmitDraft(f.ctx, f.people["staff"], f.drafts("staff")[0].ID, ""); err != nil {
		t.Fatalf("submit with 3 days left: %v", err)
	}
	if got := f.available("staff", "annual"); got != -2 {
		t.Fatalf("available = %d, want -2", got)
	}
	// Ordinary leave is still held to the balance.
	if _, err := f.submit("staff", "annual", "2026-10-12", "2026-10-12"); !fieldErr(err, "end") {
		t.Fatalf("ordinary leave on a negative balance: %v", err)
	}
}

func TestAutoApproveShutdown(t *testing.T) {
	f := newFixture(t)
	f.december(true)
	f.events = nil
	r, err := f.svc.SubmitDraft(f.ctx, f.people["staff"], f.drafts("staff")[0].ID, "")
	if err != nil || r.Status != StatusApproved || r.DecidedByID != 0 {
		t.Fatalf("auto-approved = %+v, %v", r, err)
	}
	if len(f.events) != 1 || f.events[0].Kind != EventDecided {
		t.Fatalf("events = %+v, want one decision (email + calendar)", f.events)
	}
}

// A declined shutdown request can be regenerated; the admin decides what
// happens next.
func TestDeclinedShutdownRequestCanBeRegenerated(t *testing.T) {
	f := newFixture(t)
	id := f.december(false)
	d := f.drafts("staff")[0]
	f.svc.SubmitDraft(f.ctx, f.people["staff"], d.ID, "")
	f.svc.Decide(f.ctx, f.people["head"], d.ID, false, "talk to HR")
	if n, err := f.svc.GenerateDrafts(f.ctx, id); err != nil || n != 1 {
		t.Fatalf("regenerate after decline: %d, %v", n, err)
	}
}

func TestShutdownValidationAndDelete(t *testing.T) {
	f := newFixture(t)
	bad := []ShutdownInput{
		{Name: "", Parts: []PartInput{{TypeID: f.types["annual"].ID, Start: "2026-12-14", End: "2026-12-18"}}},
		{Name: "x"},
		{Name: "x", Parts: []PartInput{
			{TypeID: f.types["annual"].ID, Start: "2026-12-14", End: "2026-12-18"},
			{TypeID: f.types["closure"].ID, Start: "2026-12-18", End: "2026-12-22"}}},
		{Name: "x", Parts: []PartInput{{TypeID: f.types["annual"].ID, Start: "2026-12-19", End: "2026-12-20"}}},
	}
	for i, in := range bad {
		if _, err := f.svc.CreateShutdown(f.ctx, f.people["hr"], in); err == nil {
			t.Errorf("bad shutdown %d accepted", i)
		}
	}
	if _, err := f.svc.CreateShutdown(f.ctx, f.people["staff"], ShutdownInput{Name: "x"}); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("non-admin creating a shutdown: %v", err)
	}

	id := f.december(false)
	if err := f.svc.DeleteShutdown(f.ctx, f.people["hr"], id); err != nil {
		t.Fatalf("delete with only drafts: %v", err)
	}
	if len(f.drafts("staff")) != 0 {
		t.Fatal("drafts survived deleting their shutdown")
	}
	id = f.december(false)
	f.svc.SubmitDraft(f.ctx, f.people["staff"], f.drafts("staff")[0].ID, "")
	if err := f.svc.DeleteShutdown(f.ctx, f.people["hr"], id); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("delete with a submitted request: %v", err)
	}
}
