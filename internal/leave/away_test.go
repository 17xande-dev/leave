package leave

import (
	"reflect"
	"testing"
)

func (f *fixture) away(viewer, start, end string) []Away {
	f.t.Helper()
	out, err := f.svc.Away(f.ctx, f.people[viewer], start, end)
	if err != nil {
		f.t.Fatal(err)
	}
	return out
}

func names(as []Away) []string {
	out := []string{}
	for _, a := range as {
		out = append(out, a.Name)
	}
	return out
}

// staff reports to head; pastor is above head; hr is an admin. orphan has no
// manager. The tests put them in departments and teams as needed.
func TestAwayHidesTypeFromColleagues(t *testing.T) {
	f := newFixture(t)
	f.dept("Ops", "staff", "orphan", "pastor", "hr")
	f.dept("Kids", "head")
	f.insertAway("staff", "sick", "2026-10-12", "2026-10-14", "approved")

	// A colleague in the same department: name and dates, never the type.
	got := f.away("orphan", "2026-10-12", "2026-10-12")
	want := []Away{{UserID: f.people["staff"].ID, Name: "staff", Start: "2026-10-12", End: "2026-10-14", Status: "approved"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("colleague sees %+v, want %+v", got, want)
	}
	// The approver (a report, though in another department) and an admin (a colleague here) see the type.
	for _, who := range []string{"head", "hr"} {
		got := f.away(who, "2026-10-12", "2026-10-12")
		if len(got) != 1 || got[0].LeaveType != "Sick leave" {
			t.Errorf("%s sees %+v", who, got)
		}
	}
	// Someone else's approver by the request's own approver_id sees it too,
	// even when the person is not their report or colleague by directory.
	f.db.Exec(`UPDATE requests SET approver_id = ?`, f.people["pastor"].ID)
	if got := f.away("pastor", "2026-10-12", "2026-10-12"); len(got) != 1 || got[0].LeaveType != "Sick leave" {
		t.Errorf("approver_id viewer sees %+v", got)
	}
}

func TestAwayScope(t *testing.T) {
	f := newFixture(t)
	f.dept("Ops", "orphan", "staff")
	f.dept("Kids", "pastor", "hr")
	f.insertAway("staff", "annual", "2026-10-12", "2026-10-12", "approved")
	f.insertAway("pastor", "annual", "2026-10-12", "2026-10-12", "pending")

	// pastor (Kids), no shared team, not a report of orphan: not shown.
	if got := names(f.away("orphan", "2026-10-12", "2026-10-12")); !reflect.DeepEqual(got, []string{"staff"}) {
		t.Fatalf("department only: %v", got)
	}
	// A shared team brings in people from another department.
	f.team("Band", "orphan", "pastor")
	if got := names(f.away("orphan", "2026-10-12", "2026-10-12")); !reflect.DeepEqual(got, []string{"pastor", "staff"}) {
		t.Fatalf("with shared team: %v", got)
	}
	// The pending one is marked, and the viewer is never in their own list.
	if got := f.away("orphan", "2026-10-12", "2026-10-12"); got[0].Status != "pending" {
		t.Errorf("status: %+v", got[0])
	}
	f.insertAway("orphan", "annual", "2026-10-12", "2026-10-12", "approved")
	if got := names(f.away("orphan", "2026-10-12", "2026-10-12")); len(got) != 2 {
		t.Errorf("viewer included: %v", got)
	}
	// A disabled team shares nothing.
	f.db.Exec(`UPDATE teams SET disabled = 1`)
	if got := names(f.away("orphan", "2026-10-12", "2026-10-12")); !reflect.DeepEqual(got, []string{"staff"}) {
		t.Errorf("disabled team: %v", got)
	}
	// A disabled colleague is not shown.
	f.db.Exec(`UPDATE users SET disabled = 1 WHERE id = ?`, f.people["staff"].ID)
	if got := f.away("orphan", "2026-10-12", "2026-10-12"); len(got) != 0 {
		t.Errorf("disabled colleague: %v", names(got))
	}
}

func TestAwayEmptyDepartmentMatchesNobody(t *testing.T) {
	f := newFixture(t)
	// orphan and staff both have no department; they must not match each other.
	f.insertAway("staff", "annual", "2026-10-12", "2026-10-12", "approved")
	f.insertAway("pastor", "annual", "2026-10-12", "2026-10-12", "approved")
	if got := f.away("orphan", "2026-10-12", "2026-10-12"); len(got) != 0 {
		t.Fatalf("empty department matched: %v", names(got))
	}
	// A manager still sees their reports, with no department at all.
	if got := names(f.away("head", "2026-10-12", "2026-10-12")); !reflect.DeepEqual(got, []string{"staff"}) {
		t.Errorf("reports: %v", got)
	}
}

func TestAwayStatusesAndBoundaries(t *testing.T) {
	f := newFixture(t)
	f.dept("Ops", "orphan", "staff", "pastor", "head")
	f.insertAway("staff", "annual", "2026-10-12", "2026-10-14", "approved")
	f.insertAway("pastor", "annual", "2026-10-12", "2026-10-14", "awaiting_hr")
	f.insertAway("head", "annual", "2026-10-12", "2026-10-14", "draft")

	if got := names(f.away("orphan", "2026-10-12", "2026-10-12")); !reflect.DeepEqual(got, []string{"staff"}) {
		t.Fatalf("awaiting_hr and draft shown: %v", got)
	}
	// The range's last day is the leave's first day, and its first day the leave's last.
	for _, c := range []struct{ s, e string }{{"2026-10-10", "2026-10-12"}, {"2026-10-14", "2026-10-16"}} {
		if got := f.away("orphan", c.s, c.e); len(got) != 1 {
			t.Errorf("%s..%s: %v", c.s, c.e, names(got))
		}
	}
	for _, c := range []struct{ s, e string }{{"2026-10-09", "2026-10-11"}, {"2026-10-15", "2026-10-16"}} {
		if got := f.away("orphan", c.s, c.e); len(got) != 0 {
			t.Errorf("%s..%s shown: %v", c.s, c.e, names(got))
		}
	}
}
