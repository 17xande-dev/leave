package leave

import (
	"errors"
	"testing"
)

func TestHalfDaysSettingRoundTrip(t *testing.T) {
	f := newFixture(t)
	st, err := f.svc.Settings(f.ctx)
	if err != nil || st.HalfDaysEnabled {
		t.Fatalf("fresh settings = %+v, %v", st, err)
	}
	for _, enabled := range []bool{true, false} {
		f.setSettings(func(st *Settings) { st.HalfDaysEnabled = enabled })
		st, err := f.svc.Settings(f.ctx)
		if err != nil || st.HalfDaysEnabled != enabled {
			t.Fatalf("settings after save %v = %+v, %v", enabled, st, err)
		}
	}
}

func TestHalfDaysOffRefusesApplicants(t *testing.T) {
	for _, flags := range []struct {
		name   string
		hs, he bool
		key    string
	}{{"first", true, false, "half_start"}, {"last", false, true, "half_end"}, {"both", true, true, "half_start"}} {
		for _, operation := range []string{"submit", "preview", "edit", "preview edit"} {
			t.Run(flags.name+"/"+operation, func(t *testing.T) {
				f := newFixture(t)
				staff := f.people["staff"]
				in := Input{TypeID: f.types["annual"].ID, Start: "2026-10-12", End: "2026-10-13", Note: "n", HalfStart: flags.hs, HalfEnd: flags.he}
				var err error
				switch operation {
				case "submit":
					_, err = f.svc.Submit(f.ctx, staff, in)
				case "preview":
					_, err = f.svc.Preview(f.ctx, staff, in)
				default:
					r := f.mustSubmit("staff", "annual", in.Start, in.End)
					if operation == "edit" {
						_, err = f.svc.Edit(f.ctx, staff, r.ID, in)
					} else {
						_, _, err = f.svc.PreviewEdit(f.ctx, staff, r.ID, in)
					}
				}
				var fe FieldErrors
				if !errors.As(err, &fe) || len(fe) != 1 || fe[flags.key] != "Half days are not available. Book whole days." {
					t.Fatalf("refusal = %v", err)
				}
			})
		}
	}
}

func TestHalfDaysOffAllowsRecordAndImport(t *testing.T) {
	for _, operation := range []string{"record", "import"} {
		t.Run(operation, func(t *testing.T) {
			f := newFixture(t)
			in := Input{TypeID: f.types["annual"].ID, Start: "2026-06-22", End: "2026-06-23", Note: "from log", HalfEnd: true}
			var r Request
			var err error
			if operation == "record" {
				r, err = f.svc.RecordLeave(f.ctx, f.people["hr"], f.people["staff"].ID, in)
			} else {
				r, err = f.svc.ImportLeave(f.ctx, f.people["hr"], f.people["staff"].ID, in, "log")
			}
			if err != nil || r.Days != 150 || !r.HalfEnd || r.Status != StatusApproved {
				t.Fatalf("%s = %+v, %v", operation, r, err)
			}
		})
	}
}

func TestTurningHalfDaysOffKeepsPendingApprovable(t *testing.T) {
	f := newHalfFixture(t)
	r := f.mustSubmitHalf("staff", "annual", "2026-10-12", "2026-10-13", true, false)
	f.setSettings(func(st *Settings) { st.HalfDaysEnabled = false })
	r, err := f.svc.Decide(f.ctx, f.people["head"], r.ID, true, "")
	if err != nil || r.Status != StatusApproved || !r.HalfStart || r.Days != 150 {
		t.Fatalf("approval = %+v, %v", r, err)
	}
}

func TestEditingHalfRequestWithSettingOffMakesWholeDays(t *testing.T) {
	f := newHalfFixture(t)
	r := f.mustSubmitHalf("staff", "annual", "2026-10-12", "2026-10-13", true, false)
	f.setSettings(func(st *Settings) { st.HalfDaysEnabled = false })
	r, err := f.svc.Edit(f.ctx, f.people["staff"], r.ID, Input{TypeID: r.TypeID, Start: r.Start, End: r.End, Note: r.Note})
	if err != nil || r.Days != WholeDays(2) || r.HalfStart || r.HalfEnd {
		t.Fatalf("edit = %+v, %v", r, err)
	}
}

func TestTurningHalfDaysOnRestoresApplicantBehavior(t *testing.T) {
	f := newFixture(t)
	f.setSettings(func(st *Settings) { st.HalfDaysEnabled = true })
	in := Input{TypeID: f.types["annual"].ID, Start: "2026-10-12", End: "2026-10-13", Note: "n", HalfEnd: true}
	q, err := f.svc.Preview(f.ctx, f.people["staff"], in)
	if err != nil || q.Days != 150 {
		t.Fatalf("preview = %+v, %v", q, err)
	}
	r, err := f.svc.Submit(f.ctx, f.people["staff"], in)
	if err != nil || r.Days != 150 || !r.HalfEnd {
		t.Fatalf("submit = %+v, %v", r, err)
	}
	in.HalfStart, in.HalfEnd = true, false
	r, err = f.svc.Edit(f.ctx, f.people["staff"], r.ID, in)
	if err != nil || r.Days != 150 || !r.HalfStart || r.HalfEnd {
		t.Fatalf("edit = %+v, %v", r, err)
	}
}

func TestWholeDaysUnchangedWithHalfDaysOffOrOn(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		f := newFixture(t)
		f.setSettings(func(st *Settings) { st.HalfDaysEnabled = enabled })
		r := f.mustSubmit("staff", "annual", "2026-10-16", "2026-10-18")
		if r.Days != WholeDays(1) || r.HalfStart || r.HalfEnd {
			t.Fatalf("half days enabled %v: request = %+v", enabled, r)
		}
	}
}
