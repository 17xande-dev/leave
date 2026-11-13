package notify

import (
	"strings"
	"testing"

	"github.com/17xande-dev/leave/internal/leave"
)

func TestHalvesChangedEmailsAndNoCalendar(t *testing.T) {
	for _, status := range []string{leave.StatusPending, leave.StatusApproved, leave.StatusDraft, leave.StatusAwaitingHR} {
		t.Run(status, func(t *testing.T) {
			e := setup(t)
			r, err := e.svc.Submit(e.ctx, e.people["staff"], leave.Input{TypeID: e.annual, Start: "2026-10-12", End: "2026-10-14"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.db.Exec(`UPDATE requests SET status=? WHERE id=?`, status, r.ID); err != nil {
				t.Fatal(err)
			}
			e.drain(t)
			if _, err := e.svc.SetHalves(e.ctx, e.people["hr"], r.ID, false, true); err != nil {
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
				if !strings.Contains(m.Text, "is now 2.5 working days, last day a half day, was 3 working days") || !strings.Contains(m.Text, "Hannah HR changed the half days") {
					t.Fatalf("text: %s", m.Text)
				}
				if i == 0 && (m.To[0] != "staff@x.test" || !strings.Contains(m.Text, "Your leave")) {
					t.Fatalf("owner: %+v", m)
				}
				if i == 1 && (m.To[0] != "head@x.test" || !strings.Contains(m.Text, "Sam Staff's leave")) {
					t.Fatalf("approver: %+v", m)
				}
			}
			if _, err := e.svc.SetHalves(e.ctx, e.people["hr"], r.ID, false, true); err == nil {
				t.Fatal("no-op accepted")
			}
			if jobs := e.drain(t); len(jobs) != 0 {
				t.Fatalf("no-op jobs: %+v", jobs)
			}
		})
	}
}
