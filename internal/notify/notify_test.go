package notify

import (
	"context"
	"database/sql"
	"encoding/json"
	"regexp"
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
	svc := leave.New(d, us, leave.Config{Location: time.UTC})
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
	st, _ := svc.Settings(ctx)
	st.FinalApproverID = e.people["pastor"].ID
	if err := svc.SaveSettings(ctx, e.people["hr"], st); err != nil {
		t.Fatal(err)
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

func (e *env) dept(t *testing.T, name string, who ...string) {
	t.Helper()
	us := users.NewStore(e.db)
	for _, w := range who {
		p := e.people[w]
		u, err := us.Upsert(e.ctx, directory.User{ID: p.DN, DN: p.DN, Username: p.Username, Name: p.Name, Email: p.Email,
			ManagerDN: p.ManagerDN, Admin: p.Admin, Department: name})
		if err != nil {
			t.Fatal(err)
		}
		e.people[w] = u
	}
}

func TestSpecialPermissionNotifications(t *testing.T) {
	e := setup(t)
	e.dept(t, "Pair", "staff", "hr")
	if err := e.svc.SetCoverageRule(e.ctx, "Pair", 50); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Submit(e.ctx, e.people["hr"], leave.Input{TypeID: e.annual, Start: "2026-10-12", End: "2026-10-14"}); err != nil {
		t.Fatal(err)
	}
	e.drain(t)

	r, err := e.svc.Submit(e.ctx, e.people["staff"], leave.Input{TypeID: e.annual, Start: "2026-10-12", End: "2026-10-14", Override: true})
	if err != nil {
		t.Fatal(err)
	}
	jobs := e.drain(t)
	if len(jobs) != 1 {
		t.Fatalf("submit jobs = %+v", jobs)
	}
	m := email(t, jobs[0])
	if m.To[0] != "head@x.test" || !strings.Contains(m.Text, "needs special permission") ||
		!strings.Contains(m.Text, "100% of Pair (2 of 2) would be away on Mon 12 Oct. The limit is 50%.") {
		t.Fatalf("submitted email:\n%s", m.Text)
	}

	// The head approves; the email goes to the pastor, says why and who already did.
	if _, err := e.svc.Decide(e.ctx, e.people["head"], r.ID, true, ""); err != nil {
		t.Fatal(err)
	}
	jobs = e.drain(t)
	if len(jobs) != 1 || jobs[0].kind != "email" {
		t.Fatalf("escalation jobs = %+v", jobs)
	}
	m = email(t, jobs[0])
	if m.To[0] != "pastor@x.test" || !strings.Contains(m.Subject, "Special permission needed: Sam Staff") ||
		!strings.Contains(m.Text, "Hi Senior") || !strings.Contains(m.Text, "100% of Pair (2 of 2)") ||
		!strings.Contains(m.Text, "Already approved by Olivia Head") || !strings.Contains(m.Text, "https://leave.test/leave/"+itoa(r.ID)) {
		t.Fatalf("escalated email:\n%s\n%s", m.Subject, m.Text)
	}

	// The final approval is the usual one: the requester hears, and the calendar gets it.
	e.svc.Decide(e.ctx, e.people["pastor"], r.ID, true, "")
	jobs = e.drain(t)
	if len(jobs) != 2 || email(t, jobs[0]).To[0] != "staff@x.test" || jobs[1].kind != "calendar_create" {
		t.Fatalf("final jobs = %+v", jobs)
	}
}

func TestNegativeBalanceNotifications(t *testing.T) {
	e := setup(t)
	r, _ := e.svc.Submit(e.ctx, e.people["staff"], leave.Input{TypeID: e.annual, Start: "2026-10-12", End: "2026-10-30"})
	e.svc.Decide(e.ctx, e.people["head"], r.ID, true, "")
	e.drain(t)

	r, err := e.svc.Submit(e.ctx, e.people["staff"], leave.Input{TypeID: e.annual, Start: "2026-11-02", End: "2026-11-04",
		Note: "wedding abroad", Negative: true})
	if err != nil {
		t.Fatal(err)
	}
	// Every admin but the requester; hr2 has no address and is skipped.
	jobs := e.drain(t)
	if len(jobs) != 1 {
		t.Fatalf("negative-request jobs = %+v", jobs)
	}
	m := email(t, jobs[0])
	if m.To[0] != "hr@x.test" || !strings.Contains(m.Subject, "Permission needed to go below zero: Sam Staff") ||
		!strings.Contains(m.Text, "3 working days below zero") || !strings.Contains(m.Text, "wedding abroad") ||
		!strings.Contains(m.Text, "https://leave.test/admin/permissions") {
		t.Fatalf("negative email:\n%s\n%s", m.Subject, m.Text)
	}

	// Granting tells the person to submit.
	if _, err := e.svc.GrantNegative(e.ctx, e.people["hr"], r.ID, "just this once"); err != nil {
		t.Fatal(err)
	}
	jobs = e.drain(t)
	if len(jobs) != 1 {
		t.Fatalf("grant jobs = %+v", jobs)
	}
	m = email(t, jobs[0])
	if m.To[0] != "staff@x.test" || !strings.Contains(m.Subject, "You may now submit") ||
		!strings.Contains(m.Text, "just this once") || !strings.Contains(m.Text, "https://leave.test/leave/"+itoa(r.ID)) {
		t.Fatalf("granted email:\n%s\n%s", m.Subject, m.Text)
	}

	// Refusing is the declined email, to the person.
	r2, _ := e.svc.Submit(e.ctx, e.people["staff"], leave.Input{TypeID: e.annual, Start: "2026-11-09", End: "2026-11-09",
		Note: "again", Negative: true})
	e.drain(t)
	e.svc.RefuseNegative(e.ctx, e.people["hr"], r2.ID, "not twice")
	jobs = e.drain(t)
	if len(jobs) != 1 {
		t.Fatalf("refusal jobs = %+v", jobs)
	}
	if m := email(t, jobs[0]); m.To[0] != "staff@x.test" || !strings.Contains(m.Text, "declined by Hannah HR") ||
		!strings.Contains(m.Text, "not twice") {
		t.Fatalf("refusal email:\n%s", m.Text)
	}
}

func TestSpecialPermissionEmailListsEveryWarning(t *testing.T) {
	e := setup(t)
	e.dept(t, "Pair", "staff", "hr")
	if err := e.svc.SetCoverageRule(e.ctx, "Pair", 50); err != nil {
		t.Fatal(err)
	}
	us := users.NewStore(e.db)
	// A team of the same two people, with its own limit.
	if _, _, err := us.SyncTeams(e.ctx, "fake", []directory.Team{{ID: "t1", Name: "Band",
		MemberIDs: []string{"CN=staff", "CN=hr"}}}); err != nil {
		t.Fatal(err)
	}
	var bandID int64
	e.db.QueryRow(`SELECT id FROM teams`).Scan(&bandID)
	if err := e.svc.SetTeamRule(e.ctx, bandID, 50); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Submit(e.ctx, e.people["hr"], leave.Input{TypeID: e.annual, Start: "2026-10-12", End: "2026-10-14"}); err != nil {
		t.Fatal(err)
	}
	e.drain(t)
	r, err := e.svc.Submit(e.ctx, e.people["staff"], leave.Input{TypeID: e.annual, Start: "2026-10-12", End: "2026-10-14", Override: true})
	if err != nil {
		t.Fatal(err)
	}
	jobs := e.drain(t)
	m := email(t, jobs[0])
	for _, want := range []string{"100% of Pair (2 of 2)", "100% of the Band team (2 of 2)"} {
		if !strings.Contains(m.Text, want) {
			t.Fatalf("submitted email lacks %q:\n%s", want, m.Text)
		}
	}
	// Escalated: a "Why:" list with a line for each.
	if _, err := e.svc.Decide(e.ctx, e.people["head"], r.ID, true, ""); err != nil {
		t.Fatal(err)
	}
	jobs = e.drain(t)
	m = email(t, jobs[0])
	if !strings.Contains(m.Text, "Why:\n- 100% of Pair (2 of 2)") || !strings.Contains(m.Text, "\n- 100% of the Band team (2 of 2)") {
		t.Fatalf("escalated email:\n%s", m.Text)
	}
}

// Outlook's Word engine drops the padding and often the background of an inline
// link, so the button is a table cell with its own bgcolor and the link inside.
func TestEmailButtonIsATableCell(t *testing.T) {
	e := setup(t)
	if _, err := e.svc.Submit(e.ctx, e.people["staff"], leave.Input{TypeID: e.annual, Start: "2026-10-12", End: "2026-10-14"}); err != nil {
		t.Fatal(err)
	}
	m := email(t, e.drain(t)[0])
	if !strings.Contains(m.HTML, `<td bgcolor="#0c0c0c"`) || !regexp.MustCompile(`<td bgcolor="#0c0c0c"[^>]*><a href="https://leave\.test/[^"]*"`).MatchString(m.HTML) {
		t.Fatalf("no table-cell button with the link inside:\n%s", m.HTML)
	}
	if !strings.Contains(m.HTML, "mso-padding-alt:10px 18px") {
		t.Fatalf("button cell lacks mso-padding-alt, so Outlook would drop the padding:\n%s", m.HTML)
	}
	// Outlook does not inherit the body's font into tables.
	if !regexp.MustCompile(`<td style="padding:24px 28px;font-family:`).MatchString(m.HTML) {
		t.Fatalf("content cell lacks its own font-family:\n%s", m.HTML)
	}
}
