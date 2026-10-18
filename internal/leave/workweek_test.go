package leave

import (
	"testing"
	"time"
)

// A Tuesday-to-Sunday week with Monday off, and Sunday restricted: it cannot
// be taken off alone, only inside a run of at least 3 working days or as a
// weekend off. "Today" in the fixture is Monday 5 October 2026.
func newChurchWeek(t *testing.T) *fixture {
	f := newFixture(t)
	sun := time.Sunday
	f.svc.cfg.Week = WeekOf(time.Tuesday, time.Wednesday, time.Thursday, time.Friday, time.Sunday)
	f.svc.cfg.RestrictedDay = &sun
	f.svc.cfg.RestrictedMinDays = 3
	for _, code := range []string{"floating", "weekend"} {
		ty := f.types[code]
		ty.Active, ty.Allowance = true, intp(4)
		if _, err := f.svc.SaveType(f.ctx, ty); err != nil {
			t.Fatal(err)
		}
		f.types[code] = ty
	}
	return f
}

func TestWorkWeekCounting(t *testing.T) {
	f := newChurchWeek(t)
	cases := []struct {
		name, code, start, end string
		days                   int    // when allowed
		field                  string // when refused
	}{
		{"Tue-Fri", "annual", "2026-10-06", "2026-10-09", 4, ""},
		{"Monday alone is free", "annual", "2026-10-12", "2026-10-12", 0, "end"},
		{"Thu-Mon: Thu, Fri, Sun", "annual", "2026-10-08", "2026-10-12", 3, ""},
		{"Fri-Mon: only 2 with the Sunday", "annual", "2026-10-09", "2026-10-12", 0, "start"},
		{"Sunday alone", "annual", "2026-10-11", "2026-10-11", 0, "start"},
		{"floating holiday on a Sunday", "floating", "2026-10-08", "2026-10-11", 0, "start"},
		{"floating holiday midweek", "floating", "2026-10-13", "2026-10-14", 2, ""},
		{"sick on a Sunday", "sick", "2026-10-04", "2026-10-04", 1, ""},
	}
	for _, c := range cases {
		q, err := f.svc.Preview(f.ctx, f.people["staff"], Input{TypeID: f.types[c.code].ID, Start: c.start, End: c.end, Note: "n"})
		if c.field != "" {
			if !fieldErr(err, c.field) {
				t.Errorf("%s: err = %v, want a %s error", c.name, err, c.field)
			}
			continue
		}
		if err != nil || q.Days != c.days {
			t.Errorf("%s: days = %d, err = %v; want %d", c.name, q.Days, err, c.days)
		}
	}
}

func TestWeekendsOff(t *testing.T) {
	f := newChurchWeek(t)
	r := f.mustSubmit("staff", "weekend", "2026-10-10", "2026-10-11")
	if r.Days != 1 {
		t.Fatalf("a weekend off costs %d, want 1", r.Days)
	}
	if got := f.available("staff", "weekend"); got != 3 {
		t.Fatalf("weekends left = %d, want 3", got)
	}
	for _, bad := range [][2]string{{"2026-10-17", "2026-10-17"}, {"2026-10-16", "2026-10-18"}, {"2026-10-17", "2026-10-25"}} {
		if _, err := f.submit("staff", "weekend", bad[0], bad[1]); !fieldErr(err, "start") {
			t.Errorf("weekend off %v: %v", bad, err)
		}
	}
	// On long weekends the Friday comes too, for the same one weekend.
	f.us.SetLongWeekend(f.ctx, f.people["staff"].ID, true)
	staff, _ := f.us.ByID(f.ctx, f.people["staff"].ID)
	r, err := f.svc.Submit(f.ctx, staff, Input{TypeID: f.types["weekend"].ID, Start: "2026-10-16", End: "2026-10-18"})
	if err != nil || r.Days != 1 {
		t.Fatalf("long weekend: %+v, %v", r, err)
	}
}

// A shutdown is mandatory: its parts may cover a Sunday whatever the rule.
func TestShutdownIgnoresRestrictedDay(t *testing.T) {
	f := newChurchWeek(t)
	if _, err := f.svc.CreateShutdown(f.ctx, f.people["hr"], ShutdownInput{Name: "x",
		Parts: []PartInput{{TypeID: f.types["annual"].ID, Start: "2026-12-12", End: "2026-12-13"}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.svc.SubmitDraft(f.ctx, f.people["staff"], f.drafts("staff")[0].ID, ""); err != nil {
		t.Fatalf("shutdown part with a lone Sunday: %v", err)
	}
}
