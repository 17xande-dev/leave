package leave

import "testing"

func (f *fixture) after(r Request) After {
	f.t.Helper()
	a, err := f.svc.AfterApproval(f.ctx, r)
	if err != nil {
		f.t.Fatal(err)
	}
	return a
}

// A pending request is already reserved, so the balance afterwards is what
// Available() says now, and equals what the quote promised before submitting.
func TestAfterApproval(t *testing.T) {
	f := newFixture(t)

	// Annual: 15 days, 3 requested. The quote said 12; so does the queue, and
	// the days are not taken off twice.
	q := f.previewQuote("annual", "2026-10-12", "2026-10-14")
	r := f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-14")
	a := f.after(r)
	if a.Left == nil || *a.Left != 12 || *q.After != 12 || a.CycleName != "" || a.Unit != UnitWorkdays {
		t.Fatalf("annual after = %+v (quote %d)", a, *q.After)
	}

	// Unlimited: nothing to say.
	if a := f.after(f.mustSubmit("staff", "unpaid", "2026-11-02", "2026-11-03")); a.Left != nil {
		t.Errorf("unlimited type: %d left", *a.Left)
	}

	// Another cycle is named, and measured in its own balance (nothing taken there yet).
	if a := f.after(f.mustSubmit("staff", "annual", "2027-01-04", "2027-01-05")); a.Left == nil || *a.Left != 13 || a.CycleName != "2027" {
		t.Errorf("next year: %+v", a)
	}

	// Below zero: take away days after the request was made.
	if err := f.svc.AddAdjustment(f.ctx, f.people["hr"], Adjustment{UserID: f.people["staff"].ID, TypeID: f.types["annual"].ID,
		CycleStart: "2026-01-01", Days: -14, Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	if a := f.after(r); a.Left == nil || *a.Left != -2 {
		t.Errorf("below zero: %+v", a)
	}

	// Weekend-unit types say so.
	ty := f.types["weekend"]
	ty.Active, ty.Allowance = true, intp(4)
	if _, err := f.svc.SaveType(f.ctx, ty); err != nil {
		t.Fatal(err)
	}
	if a := f.after(f.mustSubmit("head", "weekend", "2026-10-10", "2026-10-11")); a.Left == nil || *a.Left != 3 || a.Unit != UnitWeekend {
		t.Errorf("weekend: %+v", a)
	}
}

// Approving a cancellation gives the days back, so they are added, not subtracted.
func TestAfterApprovalOfACancellation(t *testing.T) {
	f := newFixture(t)
	r := f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-14")
	if _, err := f.svc.Decide(f.ctx, f.people["head"], r.ID, true, ""); err != nil {
		t.Fatal(err)
	}
	c, err := f.svc.RequestCancellation(f.ctx, f.people["staff"], r.ID, "plans changed")
	if err != nil {
		t.Fatal(err)
	}
	if a := f.after(c); a.Left == nil || *a.Left != 15 {
		t.Errorf("after cancelling: %+v, want 15", a)
	}
}
