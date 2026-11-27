package notify

import (
	"reflect"
	"strings"
	"testing"

	"github.com/17xande-dev/leave/internal/leave"
)

func TestRangesText(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ranges []leave.Range
		want   string
	}{
		{"two", []leave.Range{{Start: "2026-12-21", End: "2026-12-26"}, {Start: "2026-12-28", End: "2026-12-31"}}, "Mon 21 – Sat 26 Dec and Mon 28 – Thu 31 Dec 2026"},
		{"three", []leave.Range{{Start: "2026-10-12", End: "2026-10-12"}, {Start: "2026-10-14", End: "2026-10-14"}, {Start: "2026-11-02", End: "2026-11-03"}}, "Mon 12 Oct and Wed 14 Oct and Mon 2 – Tue 3 Nov 2026"},
		{"months", []leave.Range{{Start: "2026-10-30", End: "2026-11-02"}, {Start: "2026-11-04", End: "2026-11-06"}}, "Fri 30 Oct – Mon 2 Nov and Wed 4 – Fri 6 Nov 2026"},
		{"years", []leave.Range{{Start: "2026-12-28", End: "2026-12-29"}, {Start: "2027-01-04", End: "2027-01-05"}}, "Mon 28 Dec – Tue 29 Dec 2026 and Mon 4 Jan – Tue 5 Jan 2027"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := leave.Request{Start: tc.ranges[0].Start, End: tc.ranges[len(tc.ranges)-1].End, Ranges: tc.ranges}
			if got := RangesText(r); got != tc.want {
				t.Fatalf("RangesText = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSingleRangeMessageUnchanged(t *testing.T) {
	for _, dates := range [][2]string{{"2026-10-12", "2026-10-12"}, {"2026-10-12", "2026-10-16"}, {"2026-12-28", "2027-01-04"}} {
		r := leave.Request{Start: dates[0], End: dates[1], Ranges: []leave.Range{{Start: dates[0], End: dates[1]}},
			UserName: "Sam Staff", TypeName: "Annual leave", TypeUnit: leave.UnitWorkdays, Days: leave.WholeDays(2)}
		wantDates := Dates(r.Start, r.End)
		if got := RangesText(r); got != wantDates {
			t.Fatalf("single-range dates changed: %q != %q", got, wantDates)
		}
		d := msgData{To: "Olivia", Request: r, Dates: wantDates, Days: days(r)}
		want, err := New("https://leave.test", nil).render("submitted", d, "https://leave.test/leave/1", "Review the request")
		if err != nil {
			t.Fatal(err)
		}
		d.Dates = RangesText(r)
		got, err := New("https://leave.test", nil).render("submitted", d, "https://leave.test/leave/1", "Review the request")
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("single-range message changed: got %+v, want %+v", got, want)
		}
	}
}

func TestMultiRangeNotificationsAndChanges(t *testing.T) {
	e := setup(t)
	r, err := e.svc.Submit(e.ctx, e.people["staff"], leave.Input{TypeID: e.annual,
		Ranges: []leave.Range{{Start: "2026-10-12", End: "2026-10-13"}, {Start: "2026-10-15", End: "2026-10-16"}}})
	if err != nil {
		t.Fatal(err)
	}
	jobs := e.drain(t)
	if len(jobs) != 1 {
		t.Fatalf("submit jobs = %+v", jobs)
	}
	m := email(t, jobs[0])
	oldText := "Mon 12 – Tue 13 Oct and Thu 15 – Fri 16 Oct 2026"
	if !strings.Contains(m.Subject, oldText) || !strings.Contains(m.Text, oldText) || !strings.Contains(m.HTML, oldText) {
		t.Fatalf("submission does not describe ranges: %+v", m)
	}
	_, err = e.svc.Edit(e.ctx, e.people["staff"], r.ID, leave.Input{TypeID: e.annual,
		Ranges: []leave.Range{{Start: "2026-10-12", End: "2026-10-12"}, {Start: "2026-10-14", End: "2026-10-16"}}})
	if err != nil {
		t.Fatal(err)
	}
	jobs = e.drain(t)
	if len(jobs) != 1 {
		t.Fatalf("edit jobs = %+v", jobs)
	}
	m = email(t, jobs[0])
	newText := "Mon 12 Oct and Wed 14 – Fri 16 Oct 2026"
	if !strings.Contains(m.Subject, newText) || !strings.Contains(m.Text, "Was: Annual leave, "+oldText) ||
		!strings.Contains(m.Text, "Now: Annual leave, "+newText) {
		t.Fatalf("edit diff does not describe ranges: %+v", m)
	}
}
