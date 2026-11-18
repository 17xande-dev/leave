package mcpserver

import (
	"github.com/17xande-dev/leave/internal/leave"
	"strings"
	"testing"
)

func list(d map[string]any, key string) []any {
	v, _ := d[key].([]any)
	return v
}

// Everyone sets up their own stand-in, an administrator may for anyone, and
// the rules are the service's.
func TestDelegateApprovalsTools(t *testing.T) {
	f := newFixture(t)
	ops := f.as("headops")

	r := f.ok(ops, "delegate_approvals", map[string]any{"delegate": "Kagiso Kids", "start": "2026-10-05", "end": "2026-10-09", "note": "away"})
	if get(r.Data, "delegation", "stand_in") != "Kagiso Kids" || get(r.Data, "delegation", "approver") != "Olivia Operations" ||
		get(r.Data, "delegation", "active") != true || !strings.Contains(r.Text, "Kagiso Kids will approve for you from 2026-10-05 to 2026-10-09") {
		t.Fatalf("delegate = %v / %s", r.Data, r.Text)
	}
	id := get(r.Data, "delegation", "id").(float64)

	// Rules come back as readable tool errors.
	f.refused(ops, "delegate_approvals", map[string]any{"delegate": "staff2", "start": "2026-10-05", "end": "2026-10-06"}, "already has a stand-in")
	f.refused(ops, "delegate_approvals", map[string]any{"delegate": "staff2", "start": "2026-11-01", "end": "2026-12-05"}, "at most 30 days")
	f.refused(ops, "delegate_approvals", map[string]any{"delegate": "headops", "start": "2026-11-01", "end": "2026-11-02"}, "Stand-in: ")
	f.refused(ops, "delegate_approvals", map[string]any{"delegate": "ghost", "start": "2026-11-01", "end": "2026-11-02"}, "No one matches")

	// my_delegations: from them, to them, nothing for a bystander.
	mine := f.ok(ops, "my_delegations", nil)
	if len(list(mine.Data, "stand_ins_for_them")) != 1 || len(list(mine.Data, "standing_in")) != 0 {
		t.Fatalf("my_delegations = %v", mine.Data)
	}
	theirs := f.ok(f.as("headkids"), "my_delegations", nil)
	if len(list(theirs.Data, "standing_in")) != 1 || len(list(theirs.Data, "stand_ins_for_them")) != 0 {
		t.Fatalf("stand-in's view = %v", theirs.Data)
	}

	// Someone else may not end it; the approver can; once is enough.
	f.refused(f.as("staff2"), "revoke_delegation", map[string]any{"id": id}, "does not exist, has already ended, or is not yours")
	f.refused(f.as("headkids"), "revoke_delegation", map[string]any{"id": id}, "not yours")
	rv := f.ok(ops, "revoke_delegation", map[string]any{"id": id})
	if get(rv.Data, "delegation", "revoked_by") != "Olivia Operations" || !strings.Contains(rv.Text, "no longer approves for Olivia Operations") {
		t.Fatalf("revoke = %v", rv.Data)
	}
	f.refused(ops, "revoke_delegation", map[string]any{"id": id}, "already ended")
	if past := f.ok(ops, "my_delegations", nil); len(list(past.Data, "past")) != 1 || len(list(past.Data, "stand_ins_for_them")) != 0 {
		t.Fatalf("after revoking = %v", past.Data)
	}
}

func TestDelegateApprovalsAdminForSomeoneElse(t *testing.T) {
	f := newFixture(t)
	hr := f.as("hr")
	args := map[string]any{"approver": "headops", "delegate": "staff2", "start": "2026-10-05", "end": "2026-10-09"}

	// A non-admin naming someone else is refused, for every tool that takes approver,
	// and nothing is created or revealed.
	for who, other := range map[string]string{"staff1": "headops", "headops": "staff1"} {
		a := map[string]any{"approver": other, "delegate": "staff2", "start": "2026-10-05", "end": "2026-10-09"}
		f.refused(f.as(who), "delegate_approvals", a, "Only administrators can act for someone else")
		f.refused(f.as(who), "my_delegations", map[string]any{"approver": other}, "Only administrators can act for someone else")
	}
	if ds, _ := f.svc.Delegations(t.Context(), leaveFilter()); len(ds) != 0 {
		t.Fatal("a non-admin created a delegation for someone")
	}
	// Naming yourself is not acting for someone else, however you spell it.
	for _, self := range []string{"headops", "HEADOPS", "Olivia Operations"} {
		in := map[string]any{"approver": self, "delegate": "staff2", "start": "2026-10-05", "end": "2026-10-06"}
		r := f.ok(f.as("headops"), "delegate_approvals", in)
		if get(r.Data, "delegation", "approver") != "Olivia Operations" {
			t.Fatalf("self-named approver %q: %v", self, r.Data)
		}
		f.ok(f.as("headops"), "revoke_delegation", map[string]any{"id": get(r.Data, "delegation", "id").(float64)})
	}
	f.ok(f.as("headops"), "my_delegations", map[string]any{"approver": "headops"})

	r := f.ok(hr, "delegate_approvals", args)
	if get(r.Data, "delegation", "approver") != "Olivia Operations" || get(r.Data, "delegation", "set_up_by") != "Hannah HR" ||
		!strings.Contains(r.Text, "Thandi Teacher will approve for Olivia Operations") {
		t.Fatalf("admin delegate = %v / %s", r.Data, r.Text)
	}
	id := get(r.Data, "delegation", "id").(float64)
	if v := f.ok(hr, "my_delegations", map[string]any{"approver": "headops"}); len(list(v.Data, "stand_ins_for_them")) != 1 {
		t.Fatalf("admin's view of headops = %v", v.Data)
	}
	// hr's own list is empty: the optional approver really chose the person.
	if v := f.ok(hr, "my_delegations", nil); len(list(v.Data, "stand_ins_for_them")) != 0 {
		t.Fatalf("hr sees headops's stand-in as their own: %v", v.Data)
	}
	// An admin ends anyone's.
	f.ok(hr, "revoke_delegation", map[string]any{"id": id})
	// An administrator may name themselves as the person.
	f.ok(hr, "delegate_approvals", map[string]any{"approver": "hr", "delegate": "staff2", "start": "2026-10-05", "end": "2026-10-06"})
}

// The queue says when a row is only theirs as a stand-in.
func TestApprovalQueueForStandIn(t *testing.T) {
	f := newFixture(t)
	r := f.ok(f.as("staff1"), "apply_for_leave", map[string]any{"type": "annual", "start": "2026-10-12", "end": "2026-10-13"})
	f.ok(f.as("headops"), "delegate_approvals", map[string]any{"delegate": "Kagiso Kids", "start": "2026-10-05", "end": "2026-10-09"})

	own := f.ok(f.as("headops"), "approval_queue", nil)
	item := list(own.Data, "queue")[0].(map[string]any)
	if _, has := item["for"]; has || int64(get(item, "request", "id").(float64)) != id(r) {
		t.Fatalf("the approver's own row has 'for': %v", item)
	}
	q := f.ok(f.as("headkids"), "approval_queue", nil)
	item = list(q.Data, "queue")[0].(map[string]any)
	if item["for"] != "Olivia Operations" {
		t.Fatalf("stand-in's row = %v", item)
	}
	// And the decision goes through, as theirs for the approver.
	d := f.ok(f.as("headkids"), "decide_request", map[string]any{"id": id(r), "decision": "approve"})
	if get(d.Data, "request", "decided_for") != "Olivia Operations" {
		t.Fatalf("decide = %v", d.Data)
	}
}

func TestInstructionsListStandInsAmongThingsNeverDone(t *testing.T) {
	if !strings.Contains(instructions, "set up a stand-in for approvals") {
		t.Fatal("server instructions do not mention stand-ins")
	}
}

func leaveFilter() leave.DelegationFilter { return leave.DelegationFilter{} }
