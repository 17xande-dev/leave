package notify

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/17xande-dev/leave/internal/leave"
)

func (e *env) typeID(t *testing.T, code string) int64 {
	t.Helper()
	ts, err := e.svc.Types(e.ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, ty := range ts {
		if ty.Code == code {
			return ty.ID
		}
	}
	t.Fatalf("no type %s", code)
	return 0
}

// A correction tells the person always, and the approver only while the
// request is still pending; nothing goes on the calendar (it is not approved).
func TestCorrectedEmailsAndNoCalendar(t *testing.T) {
	for _, status := range []string{leave.StatusPending, leave.StatusDraft, leave.StatusAwaitingHR} {
		t.Run(status, func(t *testing.T) {
			e := setup(t)
			r, err := e.svc.Submit(e.ctx, e.people["staff"], leave.Input{TypeID: e.annual, Start: "2026-10-12", End: "2026-10-14", Note: "n"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.db.Exec(`UPDATE requests SET status=? WHERE id=?`, status, r.ID); err != nil {
				t.Fatal(err)
			}
			e.drain(t)
			if _, err := e.svc.Correct(e.ctx, e.people["hr"], r.ID,
				leave.Input{TypeID: e.typeID(t, "compassionate"), Start: "2026-10-13", End: "2026-10-14", Note: "funeral"}, "It was a bereavement", ""); err != nil {
				t.Fatal(err)
			}
			jobs := e.drain(t)
			want := 1
			if status == leave.StatusPending {
				want = 2
			}
			if len(jobs) != want {
				t.Fatalf("jobs=%+v", jobs)
			}
			for i, j := range jobs {
				if j.kind != "email" {
					t.Fatalf("calendar job: %+v", j)
				}
				m := email(t, j)
				for _, w := range []string{"Hannah HR changed", "Was: Annual leave, Mon 12 Oct – Wed 14 Oct 2026 (3 working days)",
					"Now: Compassionate leave, Tue 13 Oct – Wed 14 Oct 2026 (2 working days)", "Reason: It was a bereavement"} {
					if !strings.Contains(m.Text, w) {
						t.Errorf("missing %q in:\n%s", w, m.Text)
					}
				}
				if i == 0 && (m.To[0] != "staff@x.test" || !strings.Contains(m.Subject, "Your leave request was changed")) {
					t.Fatalf("owner: %+v", m)
				}
				if i == 1 && (m.To[0] != "head@x.test" || !strings.Contains(m.Text, "Sam Staff") || !strings.Contains(m.Text, "Please decide it as changed")) {
					t.Fatalf("approver: %+v", m)
				}
			}
		})
	}
}

// In the admins' queue every admin but the person is told.
func TestCorrectedGoesToTheAdminsQueue(t *testing.T) {
	e := setup(t)
	r, err := e.svc.Submit(e.ctx, e.people["pastor"], leave.Input{TypeID: e.annual, Start: "2026-10-12", End: "2026-10-14"})
	if err != nil || r.ApproverID != 0 {
		t.Fatalf("setup: %+v %v", r, err)
	}
	e.drain(t)
	if _, err := e.svc.Correct(e.ctx, e.people["hr"], r.ID, leave.Input{TypeID: e.annual, Start: "2026-10-13", End: "2026-10-14"}, "a day too many", ""); err != nil {
		t.Fatal(err)
	}
	// hr2 has no address in the directory: skipped, and the acting admin is told as an approver too.
	var to []string
	for _, j := range e.drain(t) {
		to = append(to, email(t, j).To[0])
	}
	if len(to) != 2 || to[0] != "pastor@x.test" || to[1] != "hr@x.test" {
		t.Fatalf("recipients: %v", to)
	}
}

// Approved leave: only the person is emailed (the email says the calendar is
// updated), and one calendar_create job carries the old event ids. A payload
// with no ids stays exactly {"request_id":N}, so older payloads and the other
// calendar jobs are byte-identical.
func TestCorrectedApprovedEmailsPersonAndQueuesCalendar(t *testing.T) {
	e := setup(t)
	r, err := e.svc.Submit(e.ctx, e.people["staff"], leave.Input{TypeID: e.annual, Start: "2026-10-12", End: "2026-10-14", Note: "n"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Decide(e.ctx, e.people["head"], r.ID, true, ""); err != nil {
		t.Fatal(err)
	}
	e.drain(t)
	if err := e.svc.SetRangeEventID(e.ctx, r.ID, "2026-10-12", "ev-old"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Correct(e.ctx, e.people["hr"], r.ID,
		leave.Input{TypeID: e.typeID(t, "compassionate"), Start: "2026-10-13", End: "2026-10-14", Note: "funeral"}, "It was a bereavement", ""); err != nil {
		t.Fatal(err)
	}
	jobs := e.drain(t)
	if len(jobs) != 2 || jobs[0].kind != "email" || jobs[1].kind != "calendar_create" {
		t.Fatalf("jobs=%+v", jobs)
	}
	m := email(t, jobs[0])
	if len(m.To) != 1 || m.To[0] != "staff@x.test" {
		t.Fatalf("recipients %v", m.To)
	}
	if !strings.Contains(m.Text, "The shared calendar will be updated") {
		t.Errorf("email does not mention the calendar:\n%s", m.Text)
	}
	if want := `{"request_id":` + itoa(r.ID) + `,"delete_event_ids":["ev-old"]}`; jobs[1].payload != want {
		t.Fatalf("payload %s, want %s", jobs[1].payload, want)
	}
	if got, _ := json.Marshal(CalendarJob{RequestID: 7}); string(got) != `{"request_id":7}` {
		t.Fatalf("payload without ids changed: %s", got)
	}
}
