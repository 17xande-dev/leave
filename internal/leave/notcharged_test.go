package leave

import (
	"strings"
	"testing"
	"time"
)

func (f *fixture) previewQuote(code, start, end string) Quote {
	f.t.Helper()
	q, err := f.svc.Preview(f.ctx, f.people["staff"], Input{TypeID: f.types[code].ID, Start: start, End: end, Note: "n"})
	if err != nil {
		f.t.Fatalf("%s %s-%s: %v", code, start, end, err)
	}
	return q
}

func lines(q Quote) string {
	var s []string
	for _, l := range q.NotCharged {
		s = append(s, l.Text)
	}
	return strings.Join(s, "|")
}

// "Today" in the fixture is Monday 5 October 2026.
func TestQuoteNotCharged(t *testing.T) {
	f := newFixture(t)

	// Wednesday 16 December is the Day of Reconciliation.
	q := f.previewQuote("annual", "2026-12-14", "2026-12-18")
	if got := lines(q); got != "Wed 16 Dec: Day of Reconciliation" || q.Days != 4 || q.NotChargedMore != 0 {
		t.Errorf("holiday: %q days %d more %d", got, q.Days, q.NotChargedMore)
	}

	// Two weekends are one line, not four.
	q = f.previewQuote("annual", "2026-10-09", "2026-10-19")
	if got := lines(q); got != "Saturday and Sunday are not working days" || q.Days != 7 {
		t.Errorf("weekends: %q days %d", got, q.Days)
	}

	// Nothing to explain.
	q = f.previewQuote("annual", "2026-10-12", "2026-10-16")
	if len(q.NotCharged) != 0 || q.NotChargedMore != 0 {
		t.Errorf("clean week: %q", lines(q))
	}

	// Calendar-day types charge everything, so nothing is uncharged.
	if q = f.previewQuote("parental", "2026-12-14", "2026-12-20"); len(q.NotCharged) != 0 {
		t.Errorf("calendar type: %q", lines(q))
	}

	// A weekend-unit type is counted per weekend; the list is irrelevant.
	ty := f.types["weekend"]
	ty.Active, ty.Allowance = true, intp(4)
	if _, err := f.svc.SaveType(f.ctx, ty); err != nil {
		t.Fatal(err)
	}
	if q = f.previewQuote("weekend", "2026-10-10", "2026-10-11"); len(q.NotCharged) != 0 || q.Days != 1 {
		t.Errorf("weekend type: %q days %d", lines(q), q.Days)
	}
}

// The list and the day count come from one rule: whatever is listed is not
// counted, across a long range with a changed working week.
func TestNotChargedAgreesWithCountDays(t *testing.T) {
	d := func(m time.Month, day int) time.Time { return civil(2026, m, day) }
	names := map[time.Time]string{}
	for m := time.January; m <= time.December; m++ {
		names[d(m, 15)] = "Holiday " + m.String()
	}
	week := WeekOf(time.Tuesday, time.Wednesday, time.Thursday, time.Friday, time.Sunday)
	start, end := d(time.January, 1), d(time.December, 31)
	lines, more := notCharged(start, end, UnitWorkdays, names, week)
	if len(lines) != maxNotCharged {
		t.Fatalf("lines = %d, want the cap %d", len(lines), maxNotCharged)
	}
	if lines[0].Text != "Monday and Saturday are not working days" {
		t.Errorf("first line = %q", lines[0].Text)
	}
	// 15th falls on a day off in some months; those are explained by the week.
	holidaysOnWorkDays := 0
	for day := range names {
		if week[day.Weekday()] {
			holidaysOnWorkDays++
		}
	}
	if got := len(lines) - 1 + more; got != holidaysOnWorkDays {
		t.Errorf("holiday lines %d, want %d", got, holidaysOnWorkDays)
	}
	if more == 0 {
		t.Error("expected the cap to leave some out")
	}
	total := int(end.Sub(start).Hours()/24) + 1
	var off int
	for x := start; !x.After(end); x = x.AddDate(0, 0, 1) {
		if !week[x.Weekday()] || names[x] != "" {
			off++
		}
	}
	if CountDays(start, end, UnitWorkdays, holidayDays(names), week) != total-off {
		t.Error("CountDays disagrees with the rule notCharged lists")
	}
}
