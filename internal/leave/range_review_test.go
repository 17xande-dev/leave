package leave

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestLongAllowedDraftSubmission(t *testing.T) {
	for _, override := range []bool{false, true} {
		t.Run(map[bool]string{false: "submit", true: "override"}[override], func(t *testing.T) {
			f := newFixture(t)
			// Emulate an HR-allowed draft created before the span guard existed.
			f.insertAway("staff", "annual", "2026-11-02", "2026-12-11", StatusDraft)
			var id int64
			if err := f.db.QueryRow(`SELECT id FROM requests WHERE user_id = ?`, f.people["staff"].ID).Scan(&id); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.Exec(`UPDATE requests SET allow_negative = 1 WHERE id = ?`, id); err != nil {
				t.Fatal(err)
			}
			var r Request
			var err error
			if override {
				r, err = f.svc.SubmitDraftOverride(f.ctx, f.people["staff"], id, "", true)
			} else {
				r, err = f.svc.SubmitDraft(f.ctx, f.people["staff"], id, "")
			}
			if err != nil || r.Status != StatusPending || !r.AllowNegative || r.Start != "2026-11-02" || r.End != "2026-12-11" {
				t.Fatalf("request=%+v err=%v", r, err)
			}
		})
	}
}

func TestLongPendingUnchangedDatesEdit(t *testing.T) {
	for _, flags := range []bool{false, true} {
		t.Run(map[bool]string{false: "note", true: "halves"}[flags], func(t *testing.T) {
			f := newHalfFixture(t)
			f.insertAway("staff", "unpaid", "2026-11-02", "2026-12-11", StatusPending)
			var id int64
			if err := f.db.QueryRow(`SELECT id FROM requests WHERE user_id = ?`, f.people["staff"].ID).Scan(&id); err != nil {
				t.Fatal(err)
			}
			in := Input{TypeID: f.types["unpaid"].ID, Ranges: []Range{{"2026-11-20", "2026-12-11"}, {"2026-11-02", "2026-11-19"}}, Note: "updated", HalfStart: flags}
			if _, editing, err := f.svc.PreviewEdit(f.ctx, f.people["staff"], id, in); err != nil || !editing {
				t.Fatalf("preview editing=%v err=%v", editing, err)
			}
			r, err := f.svc.Edit(f.ctx, f.people["staff"], id, in)
			if err != nil || r.Note != "updated" || r.HalfStart != flags || len(r.Ranges) != 1 {
				t.Fatalf("edit=%+v err=%v", r, err)
			}
			in.Ranges = []Range{{"2026-11-03", "2026-12-11"}}
			if _, _, err := f.svc.PreviewEdit(f.ctx, f.people["staff"], id, in); !fieldErr(err, "end") || !strings.Contains(err.Error(), "31") {
				t.Fatalf("changed preview: %v", err)
			}
			if _, err := f.svc.Edit(f.ctx, f.people["staff"], id, in); !fieldErr(err, "end") || !strings.Contains(err.Error(), "31") {
				t.Fatalf("changed edit: %v", err)
			}
		})
	}
}

func TestRangeOverlapInterleave(t *testing.T) {
	f := newFixture(t)
	a := Input{TypeID: f.types["annual"].ID, Ranges: []Range{{"2026-11-02", "2026-11-04"}, {"2026-11-11", "2026-11-13"}}}
	if _, err := f.svc.Submit(f.ctx, f.people["staff"], a); err != nil {
		t.Fatal(err)
	}
	b := Input{TypeID: a.TypeID, Ranges: []Range{{"2026-11-05", "2026-11-06"}, {"2026-11-12", "2026-11-12"}}}
	if _, err := f.svc.Submit(f.ctx, f.people["staff"], b); !fieldErr(err, "start") {
		t.Fatalf("overlap in second range: %v", err)
	}
	b.Ranges = b.Ranges[:1]
	if _, err := f.svc.Submit(f.ctx, f.people["staff"], b); err != nil {
		t.Fatalf("gap allowed: %v", err)
	}
}

func TestRangeCoverageLoadsLaterRanges(t *testing.T) {
	f := newFixture(t)
	f.dept("Pair", "staff", "hr")
	f.rule("Pair", 50)
	f.mustSubmit("hr", "annual", "2026-11-12", "2026-11-12")
	qt, err := f.svc.Preview(f.ctx, f.people["staff"], Input{TypeID: f.types["annual"].ID, Ranges: []Range{{"2026-11-02", "2026-11-04"}, {"2026-11-11", "2026-11-13"}}})
	if !fieldErr(err, "coverage") || len(qt.Coverage) != 1 || qt.Coverage[0].Day != "2026-11-12" || qt.Coverage[0].Away != 2 {
		t.Fatalf("quote=%+v err=%v", qt, err)
	}
}

func TestRangeCoverageOthersGaps(t *testing.T) {
	f := newFixture(t)
	f.dept("Trio", "staff", "hr", "head")
	f.rule("Trio", 67)
	// B is away either side of C. On A's requested days there are only two
	// away at a time; treating B's entire span as leave invents a third.
	if _, err := f.svc.Submit(f.ctx, f.people["hr"], Input{TypeID: f.types["annual"].ID, Ranges: []Range{{"2026-11-02", "2026-11-04"}, {"2026-11-11", "2026-11-13"}}}); err != nil {
		t.Fatal(err)
	}
	f.mustSubmit("head", "annual", "2026-11-05", "2026-11-06")
	qt, err := f.svc.Preview(f.ctx, f.people["staff"], Input{TypeID: f.types["annual"].ID, Ranges: []Range{{"2026-11-03", "2026-11-03"}, {"2026-11-05", "2026-11-06"}}})
	if err != nil || len(qt.Coverage) != 0 {
		t.Fatalf("quote=%+v err=%v", qt, err)
	}
}

func TestCoverageForWithoutRangesUsesSpan(t *testing.T) {
	f := newFixture(t)
	f.dept("Pair", "staff", "hr")
	f.rule("Pair", 50)
	f.mustSubmit("hr", "annual", "2026-11-02", "2026-11-03")
	r := Request{Kind: KindLeave, UserID: f.people["staff"].ID, TypeID: f.types["annual"].ID, Start: "2026-11-02", End: "2026-11-03"}
	got, err := f.svc.CoverageFor(f.ctx, r)
	if err != nil || len(got) != 1 {
		t.Fatalf("span coverage=%v err=%v", got, err)
	}
	r.Ranges = []Range{{r.Start, r.End}}
	want, err := f.svc.CoverageFor(f.ctx, r)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("span=%v ranges=%v err=%v", got, want, err)
	}
}

func TestRestrictedRangeKeepsWholeQuote(t *testing.T) {
	for _, sunday := range []string{"2026-12-13", "2026-12-27"} {
		t.Run(sunday, func(t *testing.T) {
			f := newFixture(t)
			f.setSettings(func(st *Settings) {
				st.Week = WeekOf(time.Tuesday, time.Wednesday, time.Thursday, time.Friday, time.Saturday, time.Sunday)
				day := time.Sunday
				st.RestrictedDay = &day
				st.RestrictedMinDays = 3
			})
			qt, err := f.svc.Preview(f.ctx, f.people["staff"], Input{TypeID: f.types["annual"].ID, Ranges: []Range{{"2026-12-15", "2026-12-17"}, {sunday, sunday}}})
			wantSuffix := " (" + d(sunday).Format("Mon 2 Jan") + " is in a stretch of 1)"
			if !fieldErr(err, "start") || !strings.HasSuffix(err.(FieldErrors)["start"], wantSuffix) || qt.Days != 300 || len(qt.NotCharged) != 1 || qt.NotCharged[0].Text != "Wed 16 Dec: Day of Reconciliation" {
				t.Fatalf("quote=%+v err=%v", qt, err)
			}
		})
	}
}
