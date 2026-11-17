package notify

import (
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
