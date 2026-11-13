package leave

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

// This test also runs unchanged in a /tmp archive of 44ce938. Compare the
// COMPAT lines from go test -count=1 -v ./internal/leave -run TestSingleRangeCompatibility
// to check the entire returned quote and field errors, including error paths.
func TestSingleRangeCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name, code, start, end string
		restricted             bool
		fields                 FieldErrors
	}{
		{name: "blank", code: "annual", fields: FieldErrors{"start": "Enter the first day of leave.", "end": "Enter the last day of leave."}},
		{name: "invalid", code: "annual", start: "bad", end: "bad", fields: FieldErrors{"start": "Enter the first day of leave.", "end": "Enter the last day of leave."}},
		{name: "unknown reversed", code: "unknown", start: "2026-11-04", end: "2026-11-02", fields: FieldErrors{"type": "Choose a type of leave."}},
		{name: "family reversed", code: "family", start: "2026-11-04", end: "2026-11-02", fields: FieldErrors{"end": "The last day cannot be before the first day.", "note": "Please add a short note for family responsibility leave."}},
		{name: "annual reversed", code: "annual", start: "2026-11-04", end: "2026-11-02", fields: FieldErrors{"end": "The last day cannot be before the first day."}},
		{name: "ordinary", code: "annual", start: "2026-11-02", end: "2026-11-06"},
		{name: "holiday", code: "annual", start: "2026-12-15", end: "2026-12-17"},
		{name: "days off", code: "annual", start: "2026-11-07", end: "2026-11-08", fields: FieldErrors{"end": "Those dates are all days off or public holidays, so there is nothing to take."}},
		{name: "restricted", code: "annual", start: "2026-12-25", end: "2026-12-27", restricted: true, fields: FieldErrors{"start": "Leave that includes a Sunday must cover at least 3 working days (this is 1). To have a Sunday off on its own, use a weekend off."}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			if tc.restricted {
				f.setSettings(func(st *Settings) {
					st.Week = WeekOf(time.Tuesday, time.Wednesday, time.Thursday, time.Friday, time.Saturday, time.Sunday)
					day := time.Sunday
					st.RestrictedDay = &day
					st.RestrictedMinDays = 3
				})
			}
			qt, err := f.svc.Preview(f.ctx, f.people["staff"], Input{TypeID: f.types[tc.code].ID, Start: tc.start, End: tc.end})
			var fe FieldErrors
			if err != nil && !errors.As(err, &fe) {
				t.Fatal(err)
			}
			got, _ := json.Marshal(fe)
			want, _ := json.Marshal(tc.fields)
			if string(got) != string(want) {
				t.Fatalf("fields=%s want=%s", got, want)
			}
			if tc.code != "unknown" && qt.Type.ID != f.types[tc.code].ID {
				t.Fatalf("lost type: %+v", qt)
			}
			if tc.name == "family reversed" && (fmtDate(qt.Start) != tc.start || fmtDate(qt.End) != tc.end) {
				t.Fatalf("lost dates: %+v", qt)
			}
			if tc.restricted && (qt.Days != 100 || len(qt.NotCharged) != 2 || qt.NotCharged[0].Text != "Fri 25 Dec: Christmas Day" || qt.NotCharged[1].Text != "Sat 26 Dec: Day of Goodwill") {
				t.Fatalf("lost figure: %+v", qt)
			}
			snapshot, _ := json.Marshal(struct {
				Quote  Quote
				Errors FieldErrors
			}{qt, fe})
			t.Logf("COMPAT %s %s", tc.name, snapshot)
		})
	}
}
