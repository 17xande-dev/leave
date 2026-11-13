package leave

import (
	"reflect"
	"testing"
)

func TestMultiRangeWritersPreserveRanges(t *testing.T) {
	for _, path := range []string{"submit", "edit", "record", "import", "cancellation", "awaiting_hr"} {
		t.Run(path, func(t *testing.T) {
			f := newFixture(t)
			want := []Range{{"2026-11-02", "2026-11-03"}, {"2026-11-05", "2026-11-06"}}
			in := Input{TypeID: f.types["annual"].ID, Ranges: want, Note: "from log"}
			var r Request
			var err error
			switch path {
			case "record":
				want = []Range{{"2026-09-02", "2026-09-03"}, {"2026-09-07", "2026-09-08"}}
				in.Ranges = want
				r, err = f.svc.RecordLeave(f.ctx, f.people["hr"], f.people["staff"].ID, in)
			case "import":
				r, err = f.svc.ImportLeave(f.ctx, f.people["hr"], f.people["staff"].ID, in, "log")
			case "edit":
				r = f.mustSubmit("staff", "annual", "2026-11-02", "2026-11-03")
				r, err = f.svc.Edit(f.ctx, f.people["staff"], r.ID, in)
			case "awaiting_hr":
				f.exhaustAnnual()
				in.Negative = true
				r, err = f.svc.Submit(f.ctx, f.people["staff"], in)
				if err == nil && r.Status != StatusAwaitingHR {
					t.Fatalf("status=%s", r.Status)
				}
			default:
				r, err = f.svc.Submit(f.ctx, f.people["staff"], in)
			}
			if err != nil {
				t.Fatal(err)
			}
			assertRangeInvariants(t, f)
			if path == "cancellation" {
				r, err = f.svc.Decide(f.ctx, f.people["head"], r.ID, true, "")
				if err != nil {
					t.Fatal(err)
				}
				for i, rg := range r.Ranges {
					if err = f.svc.SetRangeEventID(f.ctx, r.ID, rg.Start, []string{"first-event", "second-event"}[i]); err != nil {
						t.Fatal(err)
					}
				}
				r, err = f.svc.RequestCancellation(f.ctx, f.people["staff"], r.ID, "plans changed")
				if err != nil {
					t.Fatal(err)
				}
				cr, err := f.svc.CalendarRanges(f.ctx, r.ID)
				if err != nil || len(cr) != 2 {
					t.Fatalf("calendar ranges=%v err=%v", cr, err)
				}
				for _, rg := range cr {
					if rg.EventID != "" {
						t.Fatalf("cancellation copied event id: %v", cr)
					}
				}
			}
			if !reflect.DeepEqual(r.Ranges, want) || r.Start != want[0].Start || r.End != want[len(want)-1].End || r.Days != 400 {
				t.Fatalf("request=%+v want=%v", r, want)
			}
			assertRangeInvariants(t, f)
		})
	}
}

func TestRangeHalvesValidateOnlyOuterEnds(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ranges []Range
		hs, he bool
		field  string
		days   Days
	}{
		{"inner holiday permitted", []Range{{"2026-12-14", "2026-12-16"}, {"2026-12-18", "2026-12-21"}}, true, true, "", 300},
		{"outer holiday refused", []Range{{"2026-12-16", "2026-12-17"}, {"2026-12-21", "2026-12-22"}}, true, false, "half_start", 0},
		{"outer day off refused", []Range{{"2026-12-14", "2026-12-15"}, {"2026-12-18", "2026-12-20"}}, false, true, "half_end", 0},
		{"two individual days both half", []Range{{"2026-11-02", "2026-11-02"}, {"2026-11-04", "2026-11-04"}}, true, true, "", 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHalfFixture(t)
			r, err := f.svc.Submit(f.ctx, f.people["staff"], Input{TypeID: f.types["annual"].ID, Ranges: tc.ranges, HalfStart: tc.hs, HalfEnd: tc.he})
			if tc.field != "" {
				if !fieldErr(err, tc.field) {
					t.Fatalf("err=%v", err)
				}
			} else if err != nil || r.Days != tc.days {
				t.Fatalf("days=%s err=%v", r.Days, err)
			}
		})
	}
}

func TestMultiRangeSpanPolicies(t *testing.T) {
	for _, path := range []string{"preview", "submit", "edit", "record", "import"} {
		t.Run(path, func(t *testing.T) {
			f := newFixture(t)
			in := Input{TypeID: f.types["unpaid"].ID, Ranges: []Range{{"2026-11-02", "2026-11-03"}, {"2026-12-03", "2026-12-03"}}, Note: "leave"}
			var err error
			switch path {
			case "preview":
				_, err = f.svc.Preview(f.ctx, f.people["staff"], in)
			case "submit":
				_, err = f.svc.Submit(f.ctx, f.people["staff"], in)
			case "edit":
				r := f.mustSubmit("staff", "unpaid", "2026-11-02", "2026-11-03")
				if _, _, err = f.svc.PreviewEdit(f.ctx, f.people["staff"], r.ID, in); !fieldErr(err, "end") {
					t.Fatalf("preview edit: %v", err)
				}
				_, err = f.svc.Edit(f.ctx, f.people["staff"], r.ID, in)
			case "record":
				// Recording permits long spans, but every requested day must have happened.
				in.Ranges = []Range{{"2026-09-01", "2026-09-02"}, {"2026-10-06", "2026-10-06"}}
				_, err = f.svc.RecordLeave(f.ctx, f.people["hr"], f.people["staff"].ID, in)
			case "import":
				in.Ranges = []Range{{"2026-09-01", "2026-09-02"}, {"2027-10-07", "2027-10-07"}}
				_, err = f.svc.ImportLeave(f.ctx, f.people["hr"], f.people["staff"].ID, in, "log")
			}
			if !fieldErr(err, "end") {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestSetHalvesLongMultiRangeKeepsStoredCharge(t *testing.T) {
	f := newFixture(t)
	want := []Range{{"2026-11-02", "2026-11-03"}, {"2026-12-14", "2026-12-15"}}
	r, err := f.svc.ImportLeave(f.ctx, f.people["hr"], f.people["staff"].ID, Input{TypeID: f.types["annual"].ID, Ranges: want, Note: "log"}, "log")
	if err != nil {
		t.Fatal(err)
	}
	if err = f.svc.AddHoliday(f.ctx, "2026-11-03", "new holiday"); err != nil {
		t.Fatal(err)
	}
	r, err = f.svc.SetHalves(f.ctx, f.people["hr"], r.ID, true, true)
	if err != nil || r.Days != 300 || !reflect.DeepEqual(r.Ranges, want) {
		t.Fatalf("halves=%+v err=%v", r, err)
	}
	assertRangeInvariants(t, f)
}
