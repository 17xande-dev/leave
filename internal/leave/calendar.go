// Package leave is the domain: leave types, public holidays, balances,
// requests and the approval state machine.
package leave

import (
	"fmt"
	"time"
)

// Dates in this package are civil dates, carried as time.Time at midnight UTC
// and stored as YYYY-MM-DD. UTC here is only a container: it keeps AddDate
// free of DST surprises. "Today" is decided in the configured timezone and
// then converted to one of these.

func civil(y int, m time.Month, d int) time.Time { return time.Date(y, m, d, 0, 0, 0, 0, time.UTC) }

// ParseDate parses a YYYY-MM-DD form value.
func ParseDate(s string) (time.Time, error) {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("not a date: %q", s)
	}
	return t, nil
}

func fmtDate(t time.Time) string { return t.Format(time.DateOnly) }

// Today is the civil date now in loc.
func Today(now time.Time, loc *time.Location) time.Time {
	y, m, d := now.In(loc).Date()
	return civil(y, m, d)
}

// Holiday is one public holiday.
type Holiday struct {
	Date time.Time
	Name string
}

// SAPublicHolidays computes South Africa's public holidays for a year under
// the Public Holidays Act 36 of 1994: the fixed dates, Good Friday and Family
// Day from Easter, and section 2(1) -- a holiday falling on a Sunday makes the
// following Monday a holiday.
//
// The Monday rule does not cascade: when Christmas is a Sunday, the Monday is
// already the Day of Goodwill and nothing further is added. Holidays declared
// ad hoc (election days, a one-off by proclamation) are not computable and
// are added by an admin as manual entries.
func SAPublicHolidays(year int) []Holiday {
	easter := easterSunday(year)
	base := []Holiday{
		{civil(year, time.January, 1), "New Year's Day"},
		{civil(year, time.March, 21), "Human Rights Day"},
		{easter.AddDate(0, 0, -2), "Good Friday"},
		{easter.AddDate(0, 0, 1), "Family Day"},
		{civil(year, time.April, 27), "Freedom Day"},
		{civil(year, time.May, 1), "Workers' Day"},
		{civil(year, time.June, 16), "Youth Day"},
		{civil(year, time.August, 9), "National Women's Day"},
		{civil(year, time.September, 24), "Heritage Day"},
		{civil(year, time.December, 16), "Day of Reconciliation"},
		{civil(year, time.December, 25), "Christmas Day"},
		{civil(year, time.December, 26), "Day of Goodwill"},
	}
	taken := map[time.Time]bool{}
	for _, h := range base {
		taken[h.Date] = true
	}
	out := append([]Holiday(nil), base...)
	for _, h := range base {
		if h.Date.Weekday() != time.Sunday {
			continue
		}
		mon := h.Date.AddDate(0, 0, 1)
		if taken[mon] {
			continue
		}
		taken[mon] = true
		out = append(out, Holiday{mon, h.Name + " (observed)"})
	}
	return out
}

// easterSunday is the anonymous Gregorian algorithm (Meeus/Jones/Butcher).
func easterSunday(y int) time.Time {
	a := y % 19
	b, c := y/100, y%100
	d, e := b/4, b%4
	f := (b + 8) / 25
	g := (b - f + 1) / 3
	h := (19*a + b - d - g + 15) % 30
	i, k := c/4, c%4
	l := (32 + 2*e + 2*i - h - k) % 7
	m := (a + 11*h + 22*l) / 451
	month := (h + l - 7*m + 114) / 31
	day := (h+l-7*m+114)%31 + 1
	return civil(y, time.Month(month), day)
}

// CountDays is how many days a request from start to end (inclusive) costs.
// Working-day types skip weekends and public holidays; calendar types count
// every day, because four months of maternity leave is four months whatever
// falls inside it.
func CountDays(start, end time.Time, unit string, holidays map[time.Time]bool) int {
	n := 0
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		if unit == UnitCalendar {
			n++
			continue
		}
		if wd := d.Weekday(); wd == time.Saturday || wd == time.Sunday || holidays[d] {
			continue
		}
		n++
	}
	return n
}

// CycleStart is the first day of the leave cycle containing d. Annual-style
// cycles are calendar years. The BCEA sick-leave cycle is 36 months, anchored
// on anchorYear.
//
// Per-employee cycles (anchored on each person's start date, which is what
// the BCEA strictly describes) are an open decision: the directory does not
// carry a start date, and a calendar anchor is what most small employers run.
func CycleStart(d time.Time, cycleMonths, anchorYear int) time.Time {
	if cycleMonths != 36 {
		return civil(d.Year(), time.January, 1)
	}
	off := (d.Year() - anchorYear) % 3
	if off < 0 {
		off += 3
	}
	return civil(d.Year()-off, time.January, 1)
}

// CycleEnd is the last day of the cycle starting at start.
func CycleEnd(start time.Time, cycleMonths int) time.Time {
	return start.AddDate(0, cycleMonths, -1)
}
