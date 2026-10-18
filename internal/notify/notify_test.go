package notify

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/17xande-dev/mailer"

	"github.com/17xande-dev/leave/internal/dbtest"
	"github.com/17xande-dev/leave/internal/directory"
	"github.com/17xande-dev/leave/internal/leave"
	"github.com/17xande-dev/leave/internal/users"
)

type env struct {
	ctx    context.Context
	db     *sql.DB
	svc    *leave.Service
	people map[string]users.User
	annual int64
}

func setup(t *testing.T) *env {
	ctx := context.Background()
	d := dbtest.New(t)
	us := users.NewStore(d)
	svc := leave.New(d, us, leave.Config{Location: time.UTC, FinalApprover: "pastor", SickCycleAnchor: 2025, SickBackdateDays: 14})
	svc.Now = func() time.Time { return time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC) }
	svc.OnEvent(New("https://leave.test").Hook)
	e := &env{ctx: ctx, db: d, svc: svc, people: map[string]users.User{}}
	for _, p := range []struct {
		name, full, manager, email string
		admin                      bool
	}{
		{"pastor", "Senior Pastor", "", "pastor@x.test", false},
		{"head", "Olivia Head", "pastor", "head@x.test", false},
		{"staff", "Sam Staff", "head", "staff@x.test", false},
		{"hr", "Hannah HR", "head", "hr@x.test", true},
		{"hr2", "Harry HR", "head", "", true},
	} {
		mdn := ""
		if p.manager != "" {
			mdn = "CN=" + p.manager
		}
		u, err := us.Upsert(ctx, directory.User{ID: "CN=" + p.name, DN: "CN=" + p.name, Username: p.name, Name: p.full, Email: p.email, ManagerDN: mdn, Admin: p.admin})
		if err != nil {
			t.Fatal(err)
		}
		e.people[p.name] = u
	}
	svc.SeedTypes(ctx)
	ts, _ := svc.Types(ctx, true)
	e.annual = ts[0].ID
	return e
}

type job struct {
	kind    string
	payload string
}

// drain returns and clears the outbox.
func (e *env) drain(t *testing.T) []job {
	rows, err := e.db.Query(`SELECT kind, payload FROM outbox ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []job
	for rows.Next() {
		var j job
		rows.Scan(&j.kind, &j.payload)
		out = append(out, j)
	}
	e.db.Exec(`DELETE FROM outbox`)
	return out
}

func email(t *testing.T, j job) mailer.Message {
	var m mailer.Message
	if err := json.Unmarshal([]byte(j.payload), &m); err != nil {
		t.Fatal(err)
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("unsendable message: %v", err)
	}
	return m
}

func TestLifecycleNotifications(t *testing.T) {
	e := setup(t)
	r, err := e.svc.Submit(e.ctx, e.people["staff"], leave.Input{TypeID: e.annual, Start: "2026-10-12", End: "2026-10-16"})
	if err != nil {
		t.Fatal(err)
	}
	jobs := e.drain(t)
	if len(jobs) != 1 || jobs[0].kind != "email" {
		t.Fatalf("submit jobs = %+v", jobs)
	}
	m := email(t, jobs[0])
	if m.To[0] != "head@x.test" || !strings.Contains(m.Subject, "Sam Staff") ||
		!strings.Contains(m.Text, "Hi Olivia") || !strings.Contains(m.Text, "5 working days") ||
		!strings.Contains(m.Text, "https://leave.test/leave/") || !strings.Contains(m.HTML, "Review the request") {
		t.Fatalf("submitted email:\n%+v", m)
	}

	// Approval: the requester hears, and the calendar gets an event.
	e.svc.Decide(e.ctx, e.people["head"], r.ID, true, "enjoy")
	jobs = e.drain(t)
	if len(jobs) != 2 || jobs[1].kind != "calendar_create" || !strings.Contains(jobs[1].payload, `"request_id":`) {
		t.Fatalf("approve jobs = %+v", jobs)
	}
	m = email(t, jobs[0])
	if m.To[0] != "staff@x.test" || !strings.Contains(m.Text, "approved by Olivia Head") || !strings.Contains(m.Text, "enjoy") {
		t.Fatalf("approved email:\n%s", m.Text)
	}

	// An approved cancellation deletes the event of the leave it cancels.
	c, err := e.svc.RequestCancellation(e.ctx, e.people["staff"], r.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if m := email(t, e.drain(t)[0]); !strings.Contains(m.Subject, "Cancellation request") {
		t.Fatalf("cancellation request subject: %s", m.Subject)
	}
	e.svc.Decide(e.ctx, e.people["head"], c.ID, true, "")
	jobs = e.drain(t)
	want := `{"request_id":` + itoa(r.ID) + `}`
	if len(jobs) != 2 || jobs[1].kind != "calendar_delete" || jobs[1].payload != want {
		t.Fatalf("cancellation approval jobs = %+v, want delete %s", jobs, want)
	}
	if m := email(t, jobs[0]); !strings.Contains(m.Text, "back in your balance") {
		t.Fatalf("cancellation approved email:\n%s", m.Text)
	}
}

// A decline creates no calendar work; the final approver's own leave goes to
// every admin who has an address, and not to themselves.
func TestDeclineAndAdminQueue(t *testing.T) {
	e := setup(t)
	r, _ := e.svc.Submit(e.ctx, e.people["staff"], leave.Input{TypeID: e.annual, Start: "2026-10-12", End: "2026-10-12"})
	e.drain(t)
	e.svc.Decide(e.ctx, e.people["head"], r.ID, false, "busy week")
	jobs := e.drain(t)
	if len(jobs) != 1 || !strings.Contains(email(t, jobs[0]).Text, "declined") {
		t.Fatalf("decline jobs = %+v", jobs)
	}

	e.svc.Submit(e.ctx, e.people["pastor"], leave.Input{TypeID: e.annual, Start: "2026-10-13", End: "2026-10-13"})
	jobs = e.drain(t)
	if len(jobs) != 1 || email(t, jobs[0]).To[0] != "hr@x.test" {
		t.Fatalf("admins' queue jobs = %+v (hr2 has no address and must be skipped)", jobs)
	}
}

func TestDates(t *testing.T) {
	for in, want := range map[[2]string]string{
		{"2026-10-12", "2026-10-12"}: "Mon 12 Oct 2026",
		{"2026-10-12", "2026-10-16"}: "Mon 12 Oct – Fri 16 Oct 2026",
		{"2026-12-28", "2027-01-04"}: "Mon 28 Dec 2026 – Mon 4 Jan 2027",
	} {
		if got := Dates(in[0], in[1]); got != want {
			t.Errorf("Dates(%v) = %q, want %q", in, got, want)
		}
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

func TestShutdownNotifications(t *testing.T) {
	e := setup(t)
	types, _ := e.svc.Types(e.ctx, false)
	var closure int64
	for _, ty := range types {
		if ty.Code == "closure" {
			closure = ty.ID
		}
	}
	_, err := e.svc.CreateShutdown(e.ctx, e.people["hr"], leave.ShutdownInput{Name: "December shutdown", AutoApprove: true,
		Parts: []leave.PartInput{{TypeID: e.annual, Start: "2026-12-07", End: "2026-12-11"},
			{TypeID: closure, Start: "2026-12-21", End: "2026-12-31"}}})
	if err != nil {
		t.Fatal(err)
	}
	jobs := e.drain(t)
	// One email per person with an address (hr2 has none), not one per draft.
	if len(jobs) != 4 {
		t.Fatalf("%d jobs after creating a shutdown, want 4", len(jobs))
	}
	m := email(t, jobs[0])
	if !strings.Contains(m.Subject, "Please submit your December shutdown leave") || !strings.Contains(m.Text, "https://leave.test/\n") {
		t.Fatalf("drafts email:\n%s\n%s", m.Subject, m.Text)
	}

	ds, _ := e.svc.Drafts(e.ctx, e.people["staff"].ID)
	if _, err := e.svc.SubmitDraft(e.ctx, e.people["staff"], ds[0].ID, ""); err != nil {
		t.Fatal(err)
	}
	jobs = e.drain(t)
	if len(jobs) != 2 || jobs[1].kind != "calendar_create" {
		t.Fatalf("auto-approved submit jobs = %+v", jobs)
	}
	if m := email(t, jobs[0]); !strings.Contains(m.Text, "was approved automatically.") || strings.Contains(m.Text, "Their note") {
		t.Fatalf("auto-approval email:\n%s", m.Text)
	}
}
