// Package leave is the domain: leave types, public holidays, balances,
// requests and the approval state machine.
package leave

import (
	"fmt"
	"strings"
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

// FormatDate is a civil date as YYYY-MM-DD, the form requests store.
func FormatDate(t time.Time) string { return fmtDate(t) }

// DaysBetween is the number of calendar days from a to b, both civil dates.
func DaysBetween(a, b time.Time) int { return int(b.Sub(a).Hours() / 24) }

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

// Week marks the days of the week people normally work, indexed by
// time.Weekday.
type Week [7]bool

// MonFri is the default working week.
var MonFri = WeekOf(time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday)

func WeekOf(days ...time.Weekday) Week {
	var w Week
	for _, d := range days {
		w[d] = true
	}
	return w
}

// Working reports whether d is a working day: in the week, and not a holiday.
func (w Week) Working(d time.Time, holidays map[time.Time]bool) bool {
	return w[d.Weekday()] && !holidays[d]
}

// CountDays is how many days a request from start to end (inclusive) costs.
// Working-day types count only the days in the working week that are not
// public holidays -- with a Tuesday-to-Sunday week a Monday is free and a
// Sunday costs a day. Calendar types count every day, because four months of
// maternity leave is four months whatever falls inside it. (Weekend-off types
// are counted by the caller: one per weekend, whatever its days.)
func CountDays(start, end time.Time, unit string, holidays map[time.Time]bool, week Week) int {
	n := 0
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		if charged(d, unit, holidays, week) {
			n++
		}
	}
	return n
}

// charged is the one rule for whether a day costs leave, shared by CountDays
// and NotCharged so the count and the explanation cannot disagree.
func charged(d time.Time, unit string, holidays map[time.Time]bool, week Week) bool {
	return unit == UnitCalendar || week.Working(d, holidays)
}

// NotCharged is one line explaining days in a request that cost nothing.
type NotCharged struct {
	Date time.Time // the first day the line is about
	Text string
}

// maxNotCharged caps the lines shown; the rest are counted in more.
const maxNotCharged = 6

// notCharged lists the days in start..end that CountDays skips, and why: one
// line for the weekdays the working week leaves out ("Saturday and Sunday
// are not working days"), then one per public holiday, soonest first. A
// holiday on a day off is already explained by the week. At most
// maxNotCharged lines; more counts the holiday lines left out.
func notCharged(start, end time.Time, unit string, names map[time.Time]string, week Week) (lines []NotCharged, more int) {
	holidays := holidayDays(names)
	var off [7]bool
	var offFirst time.Time
	var hols []NotCharged
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		if charged(d, unit, holidays, week) {
			continue
		}
		if !week[d.Weekday()] {
			if offFirst.IsZero() {
				offFirst = d
			}
			off[d.Weekday()] = true
			continue
		}
		hols = append(hols, NotCharged{Date: d, Text: d.Format("Mon 2 Jan") + ": " + names[d]})
	}
	if !offFirst.IsZero() {
		var days []string
		// Monday first, as a week reads, not Sunday.
		for i := 1; i <= 7; i++ {
			if wd := time.Weekday(i % 7); off[wd] {
				days = append(days, wd.String())
			}
		}
		text := days[0] + " is not a working day"
		if len(days) > 1 {
			text = strings.Join(days[:len(days)-1], ", ") + " and " + days[len(days)-1] + " are not working days"
		}
		lines = append(lines, NotCharged{Date: offFirst, Text: text})
	}
	for _, h := range hols {
		if len(lines) == maxNotCharged {
			more++
			continue
		}
		lines = append(lines, h)
	}
	return lines, more
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
