package leave

import (
	"slices"
	"testing"
	"time"
)

func d(s string) time.Time {
	t, err := ParseDate(s)
	if err != nil {
		panic(err)
	}
	return t
}

// Checked against the published SA government lists for each year.
func TestSAPublicHolidays(t *testing.T) {
	cases := map[int][]string{
		2025: {"2025-01-01", "2025-03-21", "2025-04-18", "2025-04-21", "2025-04-27", "2025-04-28",
			"2025-05-01", "2025-06-16", "2025-08-09", "2025-09-24", "2025-12-16", "2025-12-25", "2025-12-26"},
		2026: {"2026-01-01", "2026-03-21", "2026-04-03", "2026-04-06", "2026-04-27", "2026-05-01",
			"2026-06-16", "2026-08-09", "2026-08-10", "2026-09-24", "2026-12-16", "2026-12-25", "2026-12-26"},
		2027: {"2027-01-01", "2027-03-21", "2027-03-22", "2027-03-26", "2027-03-29", "2027-04-27",
			"2027-05-01", "2027-06-16", "2027-08-09", "2027-09-24", "2027-12-16", "2027-12-25", "2027-12-26", "2027-12-27"},
	}
	for year, want := range cases {
		var got []string
		for _, h := range SAPublicHolidays(year) {
			got = append(got, fmtDate(h.Date))
		}
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("%d:\n got %v\nwant %v", year, got, want)
		}
	}
}

// 2022: Christmas on a Sunday. The Monday is already the Day of Goodwill, and
// the Act adds nothing further -- the Tuesday is a working day.
func TestSundayRuleDoesNotCascade(t *testing.T) {
	for _, h := range SAPublicHolidays(2022) {
		if fmtDate(h.Date) == "2022-12-27" {
			t.Fatalf("2022-12-27 marked as %q", h.Name)
		}
	}
}

func TestEaster(t *testing.T) {
	for y, want := range map[int]string{2024: "2024-03-31", 2025: "2025-04-20", 2026: "2026-04-05", 2038: "2038-04-25"} {
		if got := fmtDate(easterSunday(y)); got != want {
			t.Errorf("easter %d = %s, want %s", y, got, want)
		}
	}
}

func TestCountDays(t *testing.T) {
	hol := map[time.Time]bool{}
	for _, h := range append(SAPublicHolidays(2026), SAPublicHolidays(2027)...) {
		hol[h.Date] = true
	}
	cases := []struct {
		start, end, unit string
		want             int
	}{
		{"2026-10-05", "2026-10-09", UnitWorkdays, 5},  // Mon-Fri
		{"2026-10-03", "2026-10-04", UnitWorkdays, 0},  // a weekend
		{"2026-10-02", "2026-10-05", UnitWorkdays, 2},  // Fri + Mon over a weekend
		{"2026-04-02", "2026-04-07", UnitWorkdays, 2},  // Easter: Good Friday and Family Day skipped
		{"2026-08-10", "2026-08-10", UnitWorkdays, 0},  // Women's Day observed Monday
		{"2026-04-02", "2026-04-07", UnitCalendar, 6},  // calendar days count holidays too
		{"2026-12-21", "2027-01-08", UnitWorkdays, 13}, // across the year end: 25 Dec and 1 Jan skipped
	}
	for _, c := range cases {
		if got := CountDays(d(c.start), d(c.end), c.unit, hol, MonFri); got != c.want {
			t.Errorf("CountDays(%s..%s, %s) = %d, want %d", c.start, c.end, c.unit, got, c.want)
		}
	}
}

func TestCycleStart(t *testing.T) {
	if got := fmtDate(CycleStart(d("2026-07-01"), 12, 2025)); got != "2026-01-01" {
		t.Errorf("annual: %s", got)
	}
	for date, want := range map[string]string{
		"2025-03-01": "2025-01-01", "2027-12-31": "2025-01-01", "2028-01-01": "2028-01-01", "2024-06-01": "2022-01-01",
	} {
		if got := fmtDate(CycleStart(d(date), 36, 2025)); got != want {
			t.Errorf("sick cycle for %s = %s, want %s", date, got, want)
		}
	}
	if got := fmtDate(CycleEnd(d("2025-01-01"), 36)); got != "2027-12-31" {
		t.Errorf("sick cycle end = %s", got)
	}
}

func TestToday(t *testing.T) {
	jhb, _ := time.LoadLocation("Africa/Johannesburg")
	// 23:30 UTC on 5 Oct is already 6 Oct in Johannesburg.
	now := time.Date(2026, 10, 5, 23, 30, 0, 0, time.UTC)
	if got := fmtDate(Today(now, jhb)); got != "2026-10-06" {
		t.Fatalf("Today = %s", got)
	}
}
