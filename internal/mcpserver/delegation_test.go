package mcpserver

import (
	"testing"
	"time"

	"github.com/17xande-dev/leave/internal/users"
)

// A stand-in works the approver's queue through the tools exactly as through
// the pages: they see it, can open it, can_decide says so, and a decision
// goes through; when the delegation is over it is all gone again.
func TestStandInUsesTheTools(t *testing.T) {
	f := newFixture(t)
	staff, head, kids := f.as("staff1"), f.as("headops"), f.as("headkids")
	r := f.ok(staff, "apply_for_leave", map[string]any{"type": "annual", "start": "2026-10-12", "end": "2026-10-14"})
	reqID := id(r)

	// Before any delegation: nothing, as for any stranger.
	if q := f.ok(kids, "approval_queue", nil); len(q.Data["queue"].([]any)) != 0 {
		t.Fatalf("queue before the delegation = %v", q.Data)
	}
	f.refused(kids, "get_request", map[string]any{"id": reqID}, "No such request")

	d, o := mustUser(t, f, "headops"), mustUser(t, f, "headkids")
	if _, err := f.svc.Delegate(t.Context(), d, d.ID, o.ID, "2026-10-05", "2026-10-09", ""); err != nil {
		t.Fatal(err)
	}
	q := f.ok(kids, "approval_queue", nil).Data["queue"].([]any)
	if len(q) != 1 || get(q[0].(map[string]any), "request", "id").(float64) != float64(reqID) {
		t.Fatalf("queue = %v", q)
	}
	got := f.ok(kids, "get_request", map[string]any{"id": reqID})
	if got.Data["can_decide"] != true {
		t.Fatalf("can_decide = %v", got.Data["can_decide"])
	}
	// The approver is unaffected.
	if got := f.ok(head, "get_request", map[string]any{"id": reqID}); got.Data["can_decide"] != true {
		t.Fatal("head lost it")
	}
	a := f.ok(kids, "decide_request", map[string]any{"id": reqID, "decision": "approve", "note": "enjoy"})
	if get(a.Data, "request", "status") != "approved" || get(a.Data, "request", "decided_by") != "Kagiso Kids" {
		t.Fatalf("decision = %v", a.Data)
	}

	// Assistants are told it was done "for" the approver, on the request and,
	// for an escalated one, on the step.
	if get(a.Data, "request", "decided_for") != d.Name {
		t.Fatalf("decided_for = %v, want %q", get(a.Data, "request", "decided_for"), d.Name)
	}
	if got := f.ok(head, "get_request", map[string]any{"id": reqID}); get(got.Data, "request", "decided_for") != d.Name {
		t.Fatalf("get_request decided_for = %v", get(got.Data, "request", "decided_for"))
	}
	if _, err := f.db.Exec(`INSERT INTO request_steps (request_id, approver_id, decision, note, decided_at, on_behalf_of_id)
		VALUES (?, ?, 'approved', '', '2026-10-06T08:00:00Z', ?)`, reqID, o.ID, d.ID); err != nil {
		t.Fatal(err)
	}
	steps := f.ok(head, "get_request", map[string]any{"id": reqID}).Data["steps"].([]any)
	if len(steps) != 1 || steps[0].(map[string]any)["on_behalf_of"] != d.Name || steps[0].(map[string]any)["approver"] != o.Name {
		t.Fatalf("steps = %v", steps)
	}

	// Over: a fresh request waits with headops and the stand-in cannot reach it.
	r2 := f.ok(staff, "apply_for_leave", map[string]any{"type": "annual", "start": "2026-11-02", "end": "2026-11-02"})
	f.svc.Now = func() time.Time { return time.Date(2026, 10, 10, 8, 0, 0, 0, time.UTC) }
	if q := f.ok(kids, "approval_queue", nil); len(q.Data["queue"].([]any)) != 0 {
		t.Fatalf("queue after the end = %v", q.Data)
	}
	f.refused(kids, "get_request", map[string]any{"id": id(r2)}, "No such request")
	f.refused(kids, "decide_request", map[string]any{"id": id(r2), "decision": "approve"}, "No such request")
	// What they decided themselves stays visible to them.
	f.ok(kids, "get_request", map[string]any{"id": reqID})
}

func mustUser(t *testing.T, f *fixture, username string) users.User {
	t.Helper()
	u, err := f.us.ByUsername(t.Context(), username)
	if err != nil {
		t.Fatal(err)
	}
	return u
}
