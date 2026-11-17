package leave

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func TestReassignTargets(t *testing.T) {
	for _, tc := range []struct {
		name, owner, actor, target, message    string
		disabledActor, disabledTarget, demoted bool
		notAllowed                             bool
	}{
		{name: "admin", owner: "staff", actor: "hr", target: "pastor"},
		{name: "non-admin", owner: "staff", actor: "hr", target: "head", message: MsgReassignNotAdmin},
		{name: "disabled admin", owner: "staff", actor: "hr", target: "pastor", disabledTarget: true, message: MsgReassignDormant},
		{name: "unknown", owner: "staff", actor: "hr", target: "missing", message: MsgReassignDormant},
		{name: "applicant", owner: "staff", actor: "hr", target: "staff", message: MsgReassignOwner},
		{name: "admin applicant", owner: "hr", actor: "pastor", target: "hr", message: MsgReassignOwner},
		{name: "own request", owner: "hr", actor: "hr", target: "pastor"},
		{name: "actor as approver", owner: "staff", actor: "hr", target: "hr"},
		{name: "own self", owner: "hr", actor: "hr", target: "hr", message: MsgReassignOwner},
		{name: "queue", owner: "hr", actor: "hr"},
		{name: "non-admin actor", owner: "staff", actor: "head", target: "hr", notAllowed: true},
		{name: "disabled actor", owner: "staff", actor: "hr", target: "pastor", disabledActor: true, notAllowed: true},
		{name: "demoted before post", owner: "staff", actor: "hr", target: "pastor", demoted: true, message: MsgReassignNotAdmin},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			if _, err := f.db.Exec(`UPDATE users SET is_admin = 1 WHERE username = 'pastor'`); err != nil {
				t.Fatal(err)
			}
			p, _ := f.us.ByUsername(f.ctx, "pastor")
			f.people["pastor"] = p
			r := f.mustSubmit(tc.owner, "annual", "2026-10-12", "2026-10-12")
			target := f.people[tc.target].ID
			if tc.target == "missing" {
				target = 99999
			}
			if tc.disabledTarget {
				if _, err := f.db.Exec(`UPDATE users SET disabled = 1 WHERE id = ?`, target); err != nil {
					t.Fatal(err)
				}
			}
			if tc.demoted {
				if _, err := f.db.Exec(`UPDATE users SET is_admin = 0 WHERE id = ?`, target); err != nil {
					t.Fatal(err)
				}
			}
			actor := f.people[tc.actor]
			actor.Disabled = tc.disabledActor
			f.events = nil
			// Write a real transactional notification stand-in so refusals also
			// prove that no outbox job leaked.
			f.svc.OnEvent(func(ctx context.Context, tx *sql.Tx, e Event) error {
				f.events = append(f.events, e)
				_, err := tx.ExecContext(ctx, `INSERT INTO outbox (kind, payload, created_at, next_attempt_at) VALUES ('email', '{}', '2026-10-05', '2026-10-05')`)
				return err
			})
			_, err := f.svc.Reassign(f.ctx, actor, r.ID, target)
			if tc.notAllowed {
				if !errors.Is(err, ErrNotAllowed) {
					t.Fatalf("error = %v", err)
				}
			} else if tc.message != "" {
				var fe FieldErrors
				if !errors.As(err, &fe) || fe["approver"] != tc.message {
					t.Fatalf("error = %v, want %q", err, tc.message)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			got, err := f.svc.Get(f.ctx, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			want, events := target, 1
			if tc.notAllowed || tc.message != "" {
				want, events = r.ApproverID, 0
			}
			var jobs int
			if err := f.db.QueryRow(`SELECT count(*) FROM outbox`).Scan(&jobs); err != nil {
				t.Fatal(err)
			}
			if got.ApproverID != want || len(f.events) != events || jobs != events {
				t.Fatalf("approver=%d want=%d events=%d jobs=%d want=%d", got.ApproverID, want, len(f.events), jobs, events)
			}
			if events == 1 && (f.events[0].Kind != EventReassigned || f.events[0].Request.ApproverID != target) {
				t.Fatalf("event=%+v", f.events[0])
			}
		})
	}
}

func TestReassignRereadsTargetInTransaction(t *testing.T) {
	f := newFixture(t)
	r := f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-12")
	// Like the other transition race tests, land the competing writer at the
	// seam, not with sleeps. A pre-transaction-only check sees the old admin.
	f.svc.beforeReassignTx = func() {
		if _, err := f.db.Exec(`UPDATE users SET is_admin = 0 WHERE username = 'hr'`); err != nil {
			t.Fatal(err)
		}
	}
	_, err := f.svc.Reassign(f.ctx, f.people["hr"], r.ID, f.people["hr"].ID)
	var fe FieldErrors
	if !errors.As(err, &fe) || fe["approver"] != MsgReassignNotAdmin {
		t.Fatalf("demoted target: %v", err)
	}
	got, _ := f.svc.Get(f.ctx, r.ID)
	if got.ApproverID != r.ApproverID {
		t.Fatal("request moved")
	}
}

func TestOverrideTargets(t *testing.T) {
	for _, tc := range []struct {
		name, subject, target, message string
		disabled                       bool
	}{
		{"admin", "staff", "hr", "", false},
		{"non-admin", "staff", "head", MsgOverrideNotAdmin, false},
		{"disabled admin", "staff", "hr", MsgReassignDormant, true},
		{"unknown", "staff", "missing", MsgReassignDormant, false},
		{"subject", "hr", "hr", "Nobody can approve their own leave.", false},
		{"own override to another admin", "hr", "pastor", "", false},
		{"clear", "staff", "", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			if _, err := f.db.Exec(`UPDATE users SET is_admin = 1 WHERE username = 'pastor'`); err != nil {
				t.Fatal(err)
			}
			subject, target := f.people[tc.subject], f.people[tc.target].ID
			if tc.target == "missing" {
				target = 99999
			}
			// Pin grandfathering and clearing a legacy override.
			if _, err := f.db.Exec(`INSERT INTO approver_overrides VALUES (?, ?)`, subject.ID, f.people["head"].ID); err != nil {
				t.Fatal(err)
			}
			if got, err := f.svc.ResolveApprover(f.ctx, subject); err != nil || got != f.people["head"].ID {
				t.Fatalf("legacy route=%d %v", got, err)
			}
			if tc.disabled {
				if _, err := f.db.Exec(`UPDATE users SET disabled = 1 WHERE id = ?`, target); err != nil {
					t.Fatal(err)
				}
			}
			err := f.svc.SetOverride(f.ctx, subject.ID, target)
			if tc.message != "" {
				var fe FieldErrors
				if !errors.As(err, &fe) || fe["approver"] != tc.message {
					t.Fatalf("error=%v want=%s", err, tc.message)
				}
				target = f.people["head"].ID
			} else if err != nil {
				t.Fatal(err)
			}
			if got, err := f.svc.Override(f.ctx, subject.ID); err != nil || got != target {
				t.Fatalf("override=%d want=%d %v", got, target, err)
			}
			if target != 0 {
				if got, err := f.svc.ResolveApprover(f.ctx, subject); err != nil || got != target {
					t.Fatalf("route=%d want=%d %v", got, target, err)
				}
			}
		})
	}
}
