package leave

import (
	"errors"
	"testing"
)

// Older policy tests use unlimited unpaid leave to isolate another rule. Opt
// their own fixture into self-service rather than weakening the seeded policy.
func (f *fixture) enableStaffType(code string) {
	f.t.Helper()
	ty := f.types[code]
	ty.StaffCanApply = true
	if _, err := f.svc.SaveType(f.ctx, ty); err != nil {
		f.t.Fatal(err)
	}
	f.types[code] = ty
}

func TestSeedStaffCanApplyAndSaveType(t *testing.T) {
	f := newFixture(t)
	if len(f.types) != 10 {
		t.Fatalf("seeded types=%d", len(f.types))
	}
	for code, ty := range f.types {
		if ty.StaffCanApply != (code != "compassionate" && code != "unpaid") {
			t.Errorf("%s StaffCanApply=%v", code, ty.StaffCanApply)
		}
	}
	for _, allowed := range []bool{false, true} {
		ty := Type{Code: "custom-only", Name: "Custom", CycleMonths: 12, Unit: UnitWorkdays, Active: true, StaffCanApply: allowed}
		if allowed {
			ty.Code = "custom-open"
		}
		id, err := f.svc.SaveType(f.ctx, ty)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []bool{allowed, !allowed, allowed} {
			got, err := f.svc.Type(f.ctx, id)
			if err != nil || got.StaffCanApply != want {
				t.Fatalf("round trip=%+v want %v: %v", got, want, err)
			}
			got.StaffCanApply = !want
			if _, err := f.svc.SaveType(f.ctx, got); err != nil {
				t.Fatal(err)
			}
		}
	}
	ty := f.types["compassionate"]
	ty.StaffCanApply = true
	if _, err := f.svc.SaveType(f.ctx, ty); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.SeedTypes(f.ctx); err != nil {
		t.Fatal(err)
	}
	if got, err := f.svc.Type(f.ctx, ty.ID); err != nil || !got.StaffCanApply {
		t.Fatalf("seed overwrote admin flag: %+v %v", got, err)
	}
}

func assertAdminOnly(t *testing.T, err error, ty Type) {
	t.Helper()
	var fe FieldErrors
	if !errors.As(err, &fe) || fe["type"] != MsgAdminOnlyType(ty) {
		t.Fatalf("want type refusal %q, got %v", MsgAdminOnlyType(ty), err)
	}
}

func TestAdminOnlyApplicantRefusal(t *testing.T) {
	for _, code := range []string{"compassionate", "unpaid"} {
		for _, who := range []string{"staff", "hr"} {
			t.Run(code+"/"+who, func(t *testing.T) {
				f := newFixture(t)
				ty, u := f.types[code], f.people[who]
				in := Input{TypeID: ty.ID, Start: "2026-10-12", End: "2026-10-12", Note: "reason"}
				_, err := f.svc.Preview(f.ctx, u, in)
				assertAdminOnly(t, err, ty)
				for _, negative := range []bool{false, true} {
					in.Negative = negative
					_, err := f.svc.Submit(f.ctx, u, in)
					assertAdminOnly(t, err, ty)
				}
				ty.StaffCanApply = true
				if _, err := f.svc.SaveType(f.ctx, ty); err != nil {
					t.Fatal(err)
				}
				if _, err := f.svc.Preview(f.ctx, u, in); err != nil {
					t.Fatal(err)
				}
				if _, err := f.svc.Submit(f.ctx, u, in); err != nil {
					t.Fatal(err)
				}
			})
		}
		t.Run(code+"/record", func(t *testing.T) {
			f := newFixture(t)
			r, err := f.svc.RecordLeave(f.ctx, f.people["hr"], f.people["staff"].ID, Input{TypeID: f.types[code].ID, Start: "2026-10-02", End: "2026-10-02", Note: "HR record"})
			if err != nil || r.Status != StatusApproved {
				t.Fatalf("record: %+v %v", r, err)
			}
		})
	}
}

func TestAdminOnlyExistingRequests(t *testing.T) {
	for _, action := range []string{"edit", "withdraw", "approve-cancel"} {
		t.Run(action, func(t *testing.T) {
			f := newFixture(t)
			ty := f.types["compassionate"]
			ty.StaffCanApply = true
			if _, err := f.svc.SaveType(f.ctx, ty); err != nil {
				t.Fatal(err)
			}
			r := f.mustSubmit("staff", "compassionate", "2026-10-12", "2026-10-12")
			ty.StaffCanApply = false
			if _, err := f.svc.SaveType(f.ctx, ty); err != nil {
				t.Fatal(err)
			}
			switch action {
			case "edit":
				in := Input{TypeID: ty.ID, Start: "2026-10-13", End: "2026-10-13", Note: "new date"}
				if _, editing, err := f.svc.PreviewEdit(f.ctx, f.people["staff"], r.ID, in); err != nil || !editing {
					t.Fatalf("preview edit: %v %v", editing, err)
				}
				if got, err := f.svc.Edit(f.ctx, f.people["staff"], r.ID, in); err != nil || got.Start != in.Start || got.TypeID != ty.ID {
					t.Fatalf("edit: %+v %v", got, err)
				}
				// A foreign or made-up edit id must not gain grandfathering.
				_, _, err := f.svc.PreviewEdit(f.ctx, f.people["hr"], r.ID, in)
				assertAdminOnly(t, err, ty)
			case "withdraw":
				if err := f.svc.Withdraw(f.ctx, f.people["staff"], r.ID); err != nil {
					t.Fatal(err)
				}
			case "approve-cancel":
				if _, err := f.svc.Decide(f.ctx, f.people["head"], r.ID, true, ""); err != nil {
					t.Fatal(err)
				}
				if _, err := f.svc.RequestCancellation(f.ctx, f.people["staff"], r.ID, "changed"); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
	f := newFixture(t)
	r := f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-12")
	in := Input{TypeID: f.types["unpaid"].ID, Start: r.Start, End: r.End, Note: "switch"}
	_, err := f.svc.Edit(f.ctx, f.people["staff"], r.ID, in)
	assertAdminOnly(t, err, f.types["unpaid"])
	_, _, err = f.svc.PreviewEdit(f.ctx, f.people["staff"], r.ID, in)
	assertAdminOnly(t, err, f.types["unpaid"])
}

func TestAdminOnlyHRDraftKeepsType(t *testing.T) {
	f := newFixture(t)
	ty := f.types["unpaid"]
	ty.StaffCanApply, ty.Allowance = true, wholeDays(0)
	if _, err := f.svc.SaveType(f.ctx, ty); err != nil {
		t.Fatal(err)
	}
	r, err := f.negative("staff", "unpaid", "2026-10-12", "2026-10-12", "HR please")
	if err != nil || r.Status != StatusAwaitingHR {
		t.Fatalf("awaiting HR: %+v %v", r, err)
	}
	ty.StaffCanApply = false
	if _, err := f.svc.SaveType(f.ctx, ty); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.GrantNegative(f.ctx, f.people["hr"], r.ID, "allowed"); err != nil {
		t.Fatal(err)
	}
	if got, err := f.svc.SubmitDraft(f.ctx, f.people["staff"], r.ID, ""); err != nil || got.TypeID != ty.ID || got.Status != StatusPending {
		t.Fatalf("submit grandfathered HR draft: %+v %v", got, err)
	}
}

func TestAdminOnlyShutdownDraftUnaffected(t *testing.T) {
	f := newFixture(t)
	f.december(false)
	draft := f.drafts("staff")[0]
	ty, err := f.svc.Type(f.ctx, draft.TypeID)
	if err != nil {
		t.Fatal(err)
	}
	ty.StaffCanApply = false
	if _, err := f.svc.SaveType(f.ctx, ty); err != nil {
		t.Fatal(err)
	}
	if got, err := f.svc.SubmitDraft(f.ctx, f.people["staff"], draft.ID, ""); err != nil || got.Status != StatusPending {
		t.Fatalf("shutdown submit: %+v %v", got, err)
	}
}
