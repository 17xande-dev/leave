package leave

import (
	"reflect"
	"testing"
	"time"
)

func payrollAdjustment(t *testing.T, f *fixture, who, code, cycle string, days Days, reason, at string) {
	t.Helper()
	_, err := f.db.Exec(`INSERT INTO balance_adjustments
		(user_id,type_id,cycle_start,days_x100,reason,by_user_id,created_at) VALUES (?,?,?,?,?,?,?)`,
		f.people[who].ID, f.types[code].ID, cycle, days, reason, f.people["hr"].ID, at)
	if err != nil {
		t.Fatal(err)
	}
}

func TestPayrollParity(t *testing.T) {
	f := newFixture(t)
	payrollAdjustment(t, f, "staff", "annual", "2026-01-01", 1033, "correction", "2026-10-01")
	payrollAdjustment(t, f, "staff", "annual", "2026-01-01", -50, "correction", "2026-10-02")
	payrollAdjustment(t, f, "head", "sick", "2025-01-01", 200, "correction", "2026-10-02")
	// Use stored rows to pin exactly which states the aggregation counts, even
	// states that cannot be created by today's UI policy.
	for _, r := range []struct {
		code, cycle, status, kind, start, end string
		days                                  Days
	}{
		{"annual", "2026-01-01", "approved", "leave", "2026-10-06", "2026-10-06", 50},
		{"annual", "2026-01-01", "approved", "leave", "2026-10-04", "2026-10-12", 300},
		{"annual", "2026-01-01", "pending", "leave", "2026-11-02", "2026-11-02", 100},
		{"annual", "2026-01-01", "pending", "cancellation", "2026-10-06", "2026-10-06", 50},
		{"annual", "2026-01-01", "awaiting_hr", "leave", "2026-11-03", "2026-11-03", 100},
		{"annual", "2026-01-01", "draft", "leave", "2026-11-04", "2026-11-04", 100},
		{"annual", "2025-01-01", "approved", "leave", "2025-10-06", "2025-10-06", 400},
		{"sick", "2025-01-01", "approved", "leave", "2026-09-01", "2026-09-02", 200},
		{"closure", "2026-01-01", "approved", "leave", "2026-12-24", "2026-12-24", 100},
	} {
		var cancels any
		if r.kind == KindCancellation {
			if err := f.db.QueryRow(`SELECT id FROM requests WHERE status='approved' AND kind='leave' ORDER BY id LIMIT 1`).Scan(&cancels); err != nil {
				t.Fatal(err)
			}
		}
		res, err := f.db.Exec(`INSERT INTO requests (user_id,type_id,kind,start_date,end_date,days_x100,cycle_start,status,created_at,cancels_request_id)
			VALUES (?,?,?,?,?,?,?,?,?,?)`, f.people["staff"].ID, f.types[r.code].ID, r.kind, r.start, r.end, r.days, r.cycle, r.status, "2026-10-01", cancels)
		if err != nil {
			t.Fatal(err)
		}
		if r.days == 50 && r.kind == KindLeave {
			id, err := res.LastInsertId()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.Exec(`UPDATE requests SET half_start=1 WHERE id=?`, id); err != nil {
				t.Fatal(err)
			}
		}
		// One approved request has several ranges. Stored total, not the span
		// or a join to ranges, must drive the payroll figure.
		if r.days == 300 {
			id, _ := res.LastInsertId()
			_, err = f.db.Exec(`DELETE FROM request_ranges WHERE request_id=?`, id)
			if err == nil {
				_, err = f.db.Exec(`INSERT INTO request_ranges(request_id,start_date,end_date) VALUES (?, '2026-10-04','2026-10-05'), (?, '2026-10-12','2026-10-12')`, id, id)
			}
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	rep, err := f.svc.Payroll(f.ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	compared := 0
	for _, p := range rep.People {
		for _, pt := range rep.Types {
			fig, ok := p.Figures[pt.Type.ID]
			if !ok {
				continue
			}
			cs, _ := ParseDate(pt.CycleStart)
			b, err := f.svc.BalanceIn(f.ctx, p.User.ID, pt.Type, cs)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(fig.Allowance, b.Allowance) || fig.Adjustments != b.Adjustments ||
				fig.Approved != b.Taken || fig.Pending != b.Pending || !reflect.DeepEqual(fig.Available(), b.Available()) {
				t.Errorf("%s/%s: %+v != %+v", p.User.Username, pt.Type.Code, fig, b)
			}
			compared++
			if p.User.Username == "staff" && pt.Type.Code == "annual" {
				if fig.ApprovedFuture != 50 || *fig.ToDate() != 2183 || *fig.Available() != 2033 {
					t.Errorf("future/to date: %+v, %v", fig, fig.ToDate())
				}
			}
		}
	}
	if compared < 40 {
		t.Fatalf("only compared %d pairs", compared)
	}
	foundClosure := false
	for _, pt := range rep.Types {
		if pt.Type.Code == "floating" {
			t.Error("empty inactive floating retained")
		}
		if pt.Type.Code == "closure" {
			foundClosure = true
		}
		if pt.Type.Code == "sick" && (pt.CycleStart != "2025-01-01" || pt.CycleEnd != "2027-12-31") {
			t.Errorf("sick cycle: %+v", pt)
		}
	}
	if !foundClosure {
		t.Error("non-zero inactive closure missing")
	}
	if got := (PayrollFigures{}).ToDate(); got != nil {
		t.Error("unlimited to date not nil")
	}
}

func TestPayrollPeopleAndHiddenTypes(t *testing.T) {
	f := newFixture(t)
	if _, err := f.db.Exec(`UPDATE users SET sex='m' WHERE username='staff';
		UPDATE users SET name='Same' WHERE username IN ('head','hr');
		UPDATE users SET disabled=1 WHERE username='orphan'`); err != nil {
		t.Fatal(err)
	}
	rep, err := f.svc.Payroll(f.ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.People) != 4 {
		t.Fatalf("people=%d", len(rep.People))
	}
	for _, p := range rep.People {
		for _, code := range []string{"compassionate", "unpaid", "parental", "maternity"} {
			_, got := p.Figures[f.types[code].ID]
			want := !(code == "maternity" && p.User.Username == "staff")
			if got != want {
				t.Errorf("%s/%s present=%v", p.User.Username, code, got)
			}
			if f.types[code].ShowBalance {
				t.Errorf("fixture type %s not hidden", code)
			}
		}
	}
	if rep.People[1].User.Username != "head" || rep.People[2].User.Username != "hr" {
		t.Errorf("order: %+v", rep.People)
	}
	rep, err = f.svc.Payroll(f.ctx, true)
	if err != nil || len(rep.People) != 5 || !rep.IncludeDisabled {
		t.Fatalf("disabled: %d %v", len(rep.People), err)
	}
}

func TestPayrollOpenings(t *testing.T) {
	f := newFixture(t)
	for _, a := range []struct{ who, cycle, reason, at string }{
		{"staff", "2026-01-01", "Opening balance from payroll, October 2026: D = 12.5.", "2026-10-01"},
		{"staff", "2026-01-01", "Opening balance from payroll, October 2026: D = -1.33.", "2026-10-02"},
		{"staff", "2026-01-01", "Opening balance from payroll, October 2026: D = 10.33.", "2026-10-02"},
		{"staff", "2026-01-01", "Other D = 90.", "2026-10-03"},
		{"staff", "2025-01-01", "Opening balance from payroll, October 2026: D = 99.", "2026-10-04"},
		{"head", "2026-01-01", "Opening balance from payroll, October 2026: D = 12.5.", "2026-10-01"},
		{"head", "2026-01-01", "Opening balance from payroll: malformed", "2026-10-02"},
		{"orphan", "2026-01-01", "Opening balance from payroll, October 2026: D = -1.33.", "2026-10-02"},
	} {
		payrollAdjustment(t, f, a.who, "annual", a.cycle, 100, a.reason, a.at)
	}
	rep, err := f.svc.Payroll(f.ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	if rep.UnparsedOpenings != 1 {
		t.Errorf("unparsed=%d", rep.UnparsedOpenings)
	}
	for _, p := range rep.People {
		if p.User.Username == "staff" {
			if p.Opening == nil || *p.Opening != 1033 {
				t.Errorf("opening=%v", p.Opening)
			}
		} else if p.User.Username == "orphan" {
			if p.Opening == nil || *p.Opening != -133 {
				t.Errorf("negative opening=%v", p.Opening)
			}
		} else if p.Opening != nil {
			t.Errorf("%s unexpected opening", p.User.Username)
		}
	}
	if rep.GeneratedAt.Location().String() != "Africa/Johannesburg" || rep.GeneratedAt.Hour() != 10 {
		t.Errorf("local time=%s", rep.GeneratedAt)
	}
	if !rep.AsAt.Equal(time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("as at=%s", rep.AsAt)
	}
}
