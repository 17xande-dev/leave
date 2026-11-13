package leave

import (
	"reflect"
	"testing"
)

func TestRangeOverlapLeavesGapsFree(t *testing.T) {
	for _, status := range []string{StatusDraft, StatusAwaitingHR, StatusPending, StatusApproved} {
		t.Run(status, func(t *testing.T) {
			f := newFixture(t)
			ranges := []Range{{"2026-10-12", "2026-10-13"}, {"2026-10-15", "2026-10-16"}}
			r, err := f.svc.Submit(f.ctx, f.people["staff"], Input{TypeID: f.types["annual"].ID, Ranges: ranges})
			if err != nil {
				t.Fatal(err)
			}
			f.exec(`UPDATE requests SET status = ? WHERE id = ?`, status, r.ID)
			if _, err := f.submit("staff", "annual", "2026-10-14", "2026-10-14"); err != nil {
				t.Fatalf("gap day refused: %v", err)
			}
			for _, day := range []string{"2026-10-12", "2026-10-13", "2026-10-15", "2026-10-16"} {
				if _, err := f.submit("staff", "annual", day, day); !fieldErr(err, "start") {
					t.Errorf("intersecting boundary %s accepted: %v", day, err)
				}
			}
		})
	}
	t.Run("new ranges leave existing day free", func(t *testing.T) {
		f := newFixture(t)
		f.mustSubmit("staff", "annual", "2026-10-14", "2026-10-14")
		_, err := f.svc.Submit(f.ctx, f.people["staff"], Input{TypeID: f.types["annual"].ID,
			Ranges: []Range{{"2026-10-12", "2026-10-13"}, {"2026-10-15", "2026-10-16"}}})
		if err != nil {
			t.Fatalf("existing request in new request's gap refused: %v", err)
		}
	})
}

func TestRangeCoverageIgnoresBothPeoplesGaps(t *testing.T) {
	for _, kind := range []string{CoverageDepartment, CoverageTeam} {
		for _, gapOwner := range []string{"other", "requester"} {
			t.Run(kind+"/"+gapOwner, func(t *testing.T) {
				f := newFixture(t)
				if kind == CoverageDepartment {
					f.dept("Pair", "staff", "hr")
					f.rule("Pair", 50)
				} else {
					f.team("Pair", "staff", "hr")
					f.teamRule("Pair", 50)
				}
				split := []Range{{"2026-10-12", "2026-10-13"}, {"2026-10-15", "2026-10-16"}}
				onlyGap := []Range{{"2026-10-14", "2026-10-14"}}
				otherRanges, ownRanges := split, onlyGap
				if gapOwner == "requester" {
					otherRanges, ownRanges = onlyGap, split
				}
				if _, err := f.svc.Submit(f.ctx, f.people["hr"], Input{TypeID: f.types["annual"].ID, Ranges: otherRanges}); err != nil {
					t.Fatal(err)
				}
				r, err := f.svc.Submit(f.ctx, f.people["staff"], Input{TypeID: f.types["annual"].ID, Ranges: ownRanges})
				if err != nil {
					t.Fatalf("coverage counted %s's gap: %v", gapOwner, err)
				}
				coverage, err := f.svc.CoverageFor(f.ctx, r)
				if err != nil || len(coverage) != 0 {
					t.Fatalf("stored range coverage: %+v, %v", coverage, err)
				}
				// The same limit must still block an actual common day.
				qt, err := f.svc.Preview(f.ctx, f.people["staff"], Input{TypeID: f.types["annual"].ID,
					Ranges: []Range{{otherRanges[0].Start, otherRanges[0].Start}}})
				if !fieldErr(err, "coverage") || len(qt.Coverage) != 1 || qt.Coverage[0].Away != 2 || qt.Coverage[0].Kind != kind {
					t.Fatalf("actual shared day not counted: %+v, %v", qt.Coverage, err)
				}
			})
		}
	}
}

func TestAwayReturnsIntersectingRangesOnly(t *testing.T) {
	f := newFixture(t)
	f.dept("Ops", "staff", "orphan")
	f.team("Band", "staff", "orphan")
	f.team("Choir", "staff", "orphan")
	ranges := []Range{{"2026-10-12", "2026-10-13"}, {"2026-10-15", "2026-10-16"}}
	if _, err := f.svc.Submit(f.ctx, f.people["staff"], Input{TypeID: f.types["annual"].ID, Ranges: ranges}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, start, end string
		want             []Range
	}{
		{"whole span", "2026-10-12", "2026-10-16", ranges},
		{"first boundary", "2026-10-13", "2026-10-13", ranges[:1]},
		{"last boundary", "2026-10-15", "2026-10-15", ranges[1:]},
		{"gap", "2026-10-14", "2026-10-14", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := f.away("orphan", tc.start, tc.end)
			var dates []Range
			for _, row := range got {
				if row.UserID != f.people["staff"].ID || row.Status != StatusPending || row.LeaveType != "" {
					t.Fatalf("wrong person/status/privacy: %+v", row)
				}
				dates = append(dates, Range{row.Start, row.End})
			}
			if !reflect.DeepEqual(dates, tc.want) {
				t.Fatalf("away ranges=%v, want %v", dates, tc.want)
			}
		})
	}
}
