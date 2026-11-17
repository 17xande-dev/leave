package leave

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRangeQuoteCountsOnlyRequestedDays(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ranges []Range
		days   Days
		lines  []string
	}{
		{"holiday inside and holiday in gap", []Range{{"2026-12-15", "2026-12-17"}, {"2026-12-28", "2026-12-29"}}, 400, []string{"Wed 16 Dec: Day of Reconciliation"}},
		{"days off inside but not in gap", []Range{{"2026-12-11", "2026-12-14"}, {"2026-12-18", "2026-12-18"}}, 300, []string{"Saturday and Sunday are not working days"}},
		{"nothing in either range", []Range{{"2026-12-12", "2026-12-13"}, {"2026-12-16", "2026-12-16"}}, 0, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			qt, err := f.svc.Preview(f.ctx, f.people["staff"], Input{TypeID: f.types["annual"].ID, Ranges: tc.ranges})
			if tc.days == 0 {
				if !fieldErr(err, "end") || !strings.Contains(err.Error(), "nothing to take") {
					t.Fatalf("zero total: %v", err)
				}
				return
			}
			if err != nil || qt.Days != tc.days {
				t.Fatalf("quote=%+v err=%v", qt, err)
			}
			var lines []string
			for _, line := range qt.NotCharged {
				lines = append(lines, line.Text)
			}
			if !reflect.DeepEqual(lines, tc.lines) || qt.NotChargedMore != 0 {
				t.Fatalf("not charged=%v +%d, want %v", lines, qt.NotChargedMore, tc.lines)
			}
		})
	}
}

func TestRangeNormalisationAndRefusals(t *testing.T) {
	for _, tc := range []struct {
		name        string
		ranges      []Range
		want        []Range
		field, text string
	}{
		{"sort merge overlapping and touching", []Range{{"2026-11-05", "2026-11-06"}, {"2026-11-02", "2026-11-04"}, {"2026-11-03", "2026-11-05"}}, []Range{{"2026-11-02", "2026-11-06"}}, "", ""},
		{"three", []Range{{"2026-11-02", "2026-11-02"}, {"2026-11-04", "2026-11-04"}, {"2026-11-06", "2026-11-06"}}, nil, "", ""},
		{"four merge to three", []Range{{"2026-11-02", "2026-11-02"}, {"2026-11-03", "2026-11-03"}, {"2026-11-05", "2026-11-05"}, {"2026-11-09", "2026-11-09"}}, []Range{{"2026-11-02", "2026-11-03"}, {"2026-11-05", "2026-11-05"}, {"2026-11-09", "2026-11-09"}}, "", ""},
		{"four after merge", []Range{{"2026-11-02", "2026-11-02"}, {"2026-11-04", "2026-11-04"}, {"2026-11-06", "2026-11-06"}, {"2026-11-09", "2026-11-09"}}, nil, "start", "at most 3"},
		{"bad inner start", []Range{{"2026-11-02", "2026-11-03"}, {"bad", "2026-11-06"}}, nil, "start", "first day"},
		{"bad inner end", []Range{{"2026-11-02", "2026-11-03"}, {"2026-11-05", "bad"}}, nil, "end", "last day"},
		{"reversed inner", []Range{{"2026-11-02", "2026-11-03"}, {"2026-11-06", "2026-11-05"}}, nil, "end", "before"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			r, err := f.svc.Submit(f.ctx, f.people["staff"], Input{TypeID: f.types["annual"].ID, Ranges: tc.ranges})
			if tc.field != "" {
				var fe FieldErrors
				if !errors.As(err, &fe) || !strings.Contains(fe[tc.field], tc.text) {
					t.Fatalf("err=%v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := tc.want
			if want == nil {
				want = tc.ranges
			}
			if !reflect.DeepEqual(r.Ranges, want) {
				t.Fatalf("ranges=%v want=%v", r.Ranges, want)
			}
			assertRangeInvariants(t, f)
		})
	}
}

func TestRangeSpanLimits(t *testing.T) {
	for _, tc := range []struct {
		name, code, start, end, path string
		ranges                       []Range
		refused                      bool
	}{
		{"31 inclusive", "unpaid", "2026-11-02", "2026-12-02", "preview", nil, false},
		{"32 inclusive", "unpaid", "2026-11-02", "2026-12-03", "preview", nil, true},
		{"multiple 31", "unpaid", "", "", "preview", []Range{{"2026-11-02", "2026-11-03"}, {"2026-12-02", "2026-12-02"}}, false},
		{"multiple 32", "unpaid", "", "", "preview", []Range{{"2026-11-02", "2026-11-03"}, {"2026-12-03", "2026-12-03"}}, true},
		{"calendar exempt", "maternity", "2026-11-02", "2027-02-28", "preview", nil, false},
		{"calendar maxSpan", "maternity", "2026-11-02", "2027-06-02", "preview", nil, true},
		{"record exempt", "unpaid", "2026-08-01", "2026-09-15", "record", nil, false},
		{"import exempt", "unpaid", "2026-11-02", "2026-12-15", "import", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			f.enableStaffType("unpaid")
			in := Input{TypeID: f.types[tc.code].ID, Start: tc.start, End: tc.end, Ranges: tc.ranges, Note: "log"}
			var err error
			switch tc.path {
			case "record":
				_, err = f.svc.RecordLeave(f.ctx, f.people["hr"], f.people["staff"].ID, in)
			case "import":
				_, err = f.svc.ImportLeave(f.ctx, f.people["hr"], f.people["staff"].ID, in, "log")
			default:
				_, err = f.svc.Preview(f.ctx, f.people["staff"], in)
			}
			if tc.refused {
				if !fieldErr(err, "end") || (!strings.Contains(err.Error(), "31") && !strings.Contains(err.Error(), "190")) {
					t.Fatalf("err=%v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRangeTypeAndMandatoryRestrictions(t *testing.T) {
	for _, code := range []string{"maternity", "weekend", "annual"} {
		t.Run(code, func(t *testing.T) {
			f := newFixture(t)
			typ := f.types[code]
			typ.Active = true
			if _, err := f.svc.SaveType(f.ctx, typ); err != nil {
				t.Fatal(err)
			}
			_, err := f.svc.quote(f.ctx, f.db, f.people["staff"], Input{TypeID: typ.ID, Ranges: []Range{{"2026-11-07", "2026-11-08"}, {"2026-11-14", "2026-11-15"}}}, quoteOpts{mandatory: code == "annual"})
			var fe FieldErrors
			if !errors.As(err, &fe) || fe["start"] != typ.Name+" must be one continuous stretch of days." {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestRestrictedDayJudgedPerRange(t *testing.T) {
	f := newFixture(t)
	ft := f.types["floating"]
	ft.Active = true
	if _, err := f.svc.SaveType(f.ctx, ft); err != nil {
		t.Fatal(err)
	}
	f.setSettings(func(st *Settings) {
		st.Week = WeekOf(time.Tuesday, time.Wednesday, time.Thursday, time.Friday, time.Saturday, time.Sunday)
		d := time.Sunday
		st.RestrictedDay = &d
		st.RestrictedMinDays = 3
	})
	for _, tc := range []struct {
		name, code string
		ranges     []Range
		refused    bool
	}{
		{"Sunday alone despite long total", "annual", []Range{{"2026-11-04", "2026-11-06"}, {"2026-11-08", "2026-11-08"}}, true},
		{"each Sunday covered", "annual", []Range{{"2026-11-06", "2026-11-08"}, {"2026-11-13", "2026-11-15"}}, false},
		{"never rule", "floating", []Range{{"2026-11-06", "2026-11-08"}, {"2026-11-11", "2026-11-11"}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.svc.Preview(f.ctx, f.people["staff"], Input{TypeID: f.types[tc.code].ID, Ranges: tc.ranges})
			if tc.refused {
				if !fieldErr(err, "start") || !strings.Contains(err.Error(), "Sun 8 Nov") {
					t.Fatalf("err=%v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestMultiRangeHalvesAndGapOnlyEdit(t *testing.T) {
	f := newHalfFixture(t)
	in := Input{TypeID: f.types["annual"].ID, Ranges: []Range{{"2026-11-02", "2026-11-03"}, {"2026-11-05", "2026-11-06"}}, HalfStart: true, HalfEnd: true}
	r, err := f.svc.Submit(f.ctx, f.people["staff"], in)
	if err != nil {
		t.Fatal(err)
	}
	if r.Days != 300 {
		t.Fatalf("days=%s", r.Days)
	}
	seen := r.Fingerprint()
	// Same endpoints, total and flags; only the gap changes.
	in.Ranges = []Range{{"2026-11-02", "2026-11-02"}, {"2026-11-04", "2026-11-06"}}
	r, err = f.svc.Edit(f.ctx, f.people["staff"], r.ID, in)
	if err != nil {
		t.Fatal(err)
	}
	if r.Fingerprint() == seen || !reflect.DeepEqual(r.Ranges, in.Ranges) || r.Days != 300 {
		t.Fatalf("edited=%+v", r)
	}
	f.events = nil
	in.Ranges = []Range{{"2026-11-04", "2026-11-05"}, {"2026-11-06", "2026-11-06"}, {"2026-11-02", "2026-11-02"}}
	if _, err = f.svc.Edit(f.ctx, f.people["staff"], r.ID, in); err != nil || len(f.events) != 0 {
		t.Fatalf("normalised no-op: %v events=%v", err, f.events)
	}
	r, err = f.svc.SetHalves(f.ctx, f.people["hr"], r.ID, false, false)
	if err != nil || r.Days != 400 || len(r.Ranges) != 2 {
		t.Fatalf("clear=%+v err=%v", r, err)
	}
	assertRangeInvariants(t, f)
}
