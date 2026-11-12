package leave

import "testing"

// setSex records a person's sex the way the admin page does and returns the
// fresh row: the fixture's people map holds copies from before.
func (f *fixture) setSex(who, sex string) {
	f.t.Helper()
	if err := f.us.SetSex(f.ctx, f.people[who].ID, sex); err != nil {
		f.t.Fatal(err)
	}
	u, err := f.us.ByID(f.ctx, f.people[who].ID)
	if err != nil {
		f.t.Fatal(err)
	}
	f.people[who] = u
}

// Maternity leave is refused to someone recorded as male, allowed to everyone
// else, and a person with no sex recorded is never blocked by the missing data.
func TestMaternityIsRefusedOnlyToMaleStaff(t *testing.T) {
	for _, tc := range []struct {
		sex     string
		refused bool
	}{{"", false}, {"f", false}, {"m", true}} {
		f := newFixture(t)
		f.setSex("staff", tc.sex)
		_, err := f.submit("staff", "maternity", "2026-10-12", "2026-10-16")
		if got := fieldErr(err, "type"); got != tc.refused || (!tc.refused && err != nil) {
			t.Errorf("sex %q: err = %v, refused = %v, want refused = %v", tc.sex, err, got, tc.refused)
		}
	}
}

// The refusal sits in quote, so recording, importing and previewing reach it
// as well as applying.
func TestMaternityRefusalReachesEveryPathIntoQuote(t *testing.T) {
	f := newFixture(t)
	f.setSex("staff", "m")
	hr, staff := f.people["hr"], f.people["staff"]
	in := Input{TypeID: f.types["maternity"].ID, Start: "2026-10-12", End: "2026-10-16", Note: "n"}
	past := Input{TypeID: in.TypeID, Start: "2026-06-15", End: "2026-06-19", Note: "n"}

	if _, err := f.svc.Preview(f.ctx, staff, in); !fieldErr(err, "type") {
		t.Errorf("Preview: %v", err)
	}
	if _, err := f.svc.RecordLeave(f.ctx, hr, staff.ID, past); !fieldErr(err, "type") {
		t.Errorf("RecordLeave: %v", err)
	}
	if _, err := f.svc.ImportLeave(f.ctx, hr, staff.ID, in, "n"); !fieldErr(err, "type") {
		t.Errorf("ImportLeave: %v", err)
	}

	// The same leave for a woman goes through both.
	f.setSex("staff", "f")
	if _, err := f.svc.RecordLeave(f.ctx, hr, f.people["staff"].ID, past); err != nil {
		t.Errorf("RecordLeave for a woman: %v", err)
	}
}

func TestSexDoesNotAffectOtherTypes(t *testing.T) {
	f := newFixture(t)
	f.setSex("staff", "m")
	if _, err := f.submit("staff", "annual", "2026-10-12", "2026-10-13"); err != nil {
		t.Errorf("annual: %v", err)
	}
	if _, err := f.submit("staff", "sick", "2026-10-06", "2026-10-06"); err != nil {
		t.Errorf("sick: %v", err)
	}
	for code, ty := range f.types {
		if code != MaternityCode && !ty.OfferedTo(f.people["staff"]) {
			t.Errorf("%s is hidden from a male-marked person", code)
		}
	}
	if f.types[MaternityCode].OfferedTo(f.people["staff"]) || !f.types[MaternityCode].OfferedTo(f.people["head"]) {
		t.Error("OfferedTo gets maternity wrong")
	}
}

// An existing maternity request is not grandfathered: editing it after the
// person is marked male goes back through quote and is refused.
func TestEditingMaternityForSinceMarkedMaleIsRefused(t *testing.T) {
	f := newFixture(t)
	r := f.mustSubmit("staff", "maternity", "2026-10-12", "2026-10-16")
	f.setSex("staff", "m")
	_, err := f.svc.Edit(f.ctx, f.people["staff"], r.ID, Input{TypeID: r.TypeID, Start: "2026-10-12", End: "2026-10-15", Note: "n"})
	if !fieldErr(err, "type") {
		t.Fatalf("edit: %v", err)
	}
}
