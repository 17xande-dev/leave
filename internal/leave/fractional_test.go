package leave

import "testing"

// Payroll balances are fractional, so adjustments are too; the sums must be
// exact, which is why they are integer hundredths and never floats.
func TestFractionalAdjustmentsGiveAnExactBalance(t *testing.T) {
	f := newFixture(t)
	adjust := func(d Days) {
		t.Helper()
		err := f.svc.AddAdjustment(f.ctx, f.people["hr"], Adjustment{UserID: f.people["staff"].ID,
			TypeID: f.types["annual"].ID, CycleStart: "2026-01-01", Days: d, Reason: "payroll"})
		if err != nil {
			t.Fatal(err)
		}
	}
	adjust(250)  // +2.5
	adjust(-144) // -1.44
	adjust(1)    // +0.01: a float sum would drift here

	bs, err := f.svc.Balances(f.ctx, f.people["staff"].ID)
	if err != nil {
		t.Fatal(err)
	}
	var b Balance
	for _, x := range bs {
		if x.Type.Code == "annual" {
			b = x
		}
	}
	// 15 + 2.5 - 1.44 + 0.01
	if got := *b.Available(); got != 1607 || got.String() != "16.07" {
		t.Fatalf("available = %d (%s), want 1607 (16.07)", int64(got), got)
	}
	if b.Adjustments != 107 || b.Adjustments.String() != "1.07" {
		t.Fatalf("adjustments = %s, want 1.07", b.Adjustments)
	}

	// A whole-day request comes off the fractional balance exactly.
	r := f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-16") // 5 days
	if r.Days != 500 {
		t.Fatalf("request days = %s, want 5", r.Days)
	}
	bs, _ = f.svc.Balances(f.ctx, f.people["staff"].ID)
	for _, x := range bs {
		if x.Type.Code == "annual" {
			b = x
		}
	}
	if got := *b.Available(); got.String() != "11.07" {
		t.Fatalf("available after a 5 day request = %s, want 11.07", got)
	}

	// The availability read for the people list agrees with the balance.
	av, err := f.svc.AvailabilityByCode(f.ctx, AnnualCode)
	if err != nil {
		t.Fatal(err)
	}
	if got := av.For(f.people["staff"].ID); got == nil || got.String() != "11.07" {
		t.Fatalf("availability = %v, want 11.07", got)
	}
}

// Compassionate leave is seeded unlimited: no balance to exhaust, so recording
// a death in the family is never refused by a limit.
func TestCompassionateTypeIsSeeded(t *testing.T) {
	f := newFixture(t)
	ct, ok := f.types["compassionate"]
	if !ok {
		t.Fatal("compassionate type not seeded")
	}
	if !ct.Active || ct.Allowance != nil || ct.Unit != UnitWorkdays || ct.CycleMonths != 12 ||
		ct.CoverageRule != CoverageCount || ct.Restricted != RestrictAny || !ct.RequiresNote {
		t.Fatalf("compassionate = %+v", ct)
	}
	if ct.Sort != f.types["parental"].Sort+1 {
		t.Errorf("compassionate sorts at %d, want straight after parental (%d)", ct.Sort, f.types["parental"].Sort)
	}
}
