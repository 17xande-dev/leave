package leave

import (
	"context"
	"database/sql"
	"strings"

	"github.com/17xande-dev/leave/internal/users"
)

// MsgRecordFuture is the refusal for leave whose last day is still to come.
const MsgRecordFuture = "Only leave that has already happened can be recorded; future days go through an application."

// MsgReassignOwner and the like are the admin refusals the pages and the
// assistant both name; the service returns them as FieldErrors.
const (
	MsgReassignOwner   = "Not the person whose request it is."
	MsgReassignDormant = "Choose someone who can currently sign in."
	MsgUnrecordOwn     = "Another admin removes leave recorded for you."
	MsgUnrecordGone    = "That recorded leave was already removed."
	MsgRecordOwn       = "Another admin records your own leave."
	MsgAdjustOwn       = "Another admin adjusts your own balance."
)

// RecordLeave lets an admin write down leave a person has already taken (it
// must be over by today), so it shows in their history, the team view, overlap
// checks and the calendar like any approved request.
//
// Days are counted exactly as for a request (quote does the work). What is
// relaxed is what makes no sense after the fact: the backdating limit, the
// department and team coverage limits, the balance -- recording what happened
// may take it below zero, which is flagged on the request (AllowNegative) --
// and a type since retired or a restricted-day rule since changed. Leave whose
// last day is still to come is refused: it would sidestep approval and
// coverage. The person's own overlapping leave still refuses, and a reason is required.
//
// The request is stored approved, with the admin as approver and decider. The
// requests table forbids approver = owner, so nobody records their own leave,
// the same rule as GrantNegative: another admin does it.
func (s *Service) RecordLeave(ctx context.Context, admin users.User, userID int64, in Input) (Request, error) {
	if !admin.Admin || admin.Disabled {
		return Request{}, ErrNotAllowed
	}
	if admin.ID == userID {
		return Request{}, FieldErrors{"person": MsgRecordOwn}
	}
	u, err := s.users.ByID(ctx, userID)
	if err != nil {
		return Request{}, err
	}
	if err := s.ensureYears(ctx, in); err != nil {
		return Request{}, err
	}
	var out Request
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		qt, err := s.quote(ctx, tx, u, in, quoteOpts{record: true})
		if err != nil {
			return err
		}
		now := stampNow(s.Now())
		res, err := tx.ExecContext(ctx, `INSERT INTO requests (user_id, type_id, kind, start_date, end_date, days,
			cycle_start, note, status, approver_id, decided_by_id, decided_at, decision_note, allow_negative,
			recorded_by_id, created_at)
			VALUES (?, ?, 'leave', ?, ?, ?, ?, ?, 'approved', ?, ?, ?, ?, ?, ?, ?)`,
			u.ID, qt.Type.ID, fmtDate(qt.Start), fmtDate(qt.End), qt.Days, fmtDate(qt.CycleStart),
			strings.TrimSpace(in.Note), admin.ID, admin.ID, now, RecordedPrefix+admin.Name,
			qt.Shortfall > 0, admin.ID, now)
		if err != nil {
			return err
		}
		id, _ := res.LastInsertId()
		if out, err = s.get(ctx, tx, id); err != nil {
			return err
		}
		return s.hook(ctx, tx, Event{Kind: EventRecorded, Request: out, Actor: admin, Shortfall: qt.Shortfall})
	})
	return out, err
}

// Unrecord lets an admin remove leave that was recorded (not any approved
// leave: ordinary leave is cancelled through the approval flow). The person's
// own cancellation flow cannot help here, because recorded leave has usually
// started already. It no longer counts against the balance and the calendar event goes;
// who removed it, and when, stays on the row.
//
// The guard is in the UPDATE itself: recorded_by_id is set only by RecordLeave,
// so nothing an approver types can make ordinary leave removable this way.
func (s *Service) Unrecord(ctx context.Context, admin users.User, id int64) (Request, error) {
	if !admin.Admin || admin.Disabled {
		return Request{}, ErrNotAllowed
	}
	var out Request
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		r, err := s.get(ctx, tx, id)
		if err != nil {
			return err
		}
		if !r.IsRecorded() {
			return ErrNotAllowed
		}
		if r.UserID == admin.ID {
			return FieldErrors{"person": MsgUnrecordOwn}
		}
		if r.RemovedByID != 0 {
			return FieldErrors{"request": MsgUnrecordGone}
		}
		now := stampNow(s.Now())
		res, err := tx.ExecContext(ctx, `UPDATE requests SET status = 'cancelled', removed_by_id = ?, removed_at = ?
			WHERE id = ? AND recorded_by_id IS NOT NULL AND status = 'approved'`, admin.ID, now, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotAllowed
		}
		// No pending cancellation can be waiting on it: the owner may only ask
		// to cancel leave starting after today, and RecordLeave refuses to
		// record any such leave.
		if out, err = s.get(ctx, tx, id); err != nil {
			return err
		}
		return s.hook(ctx, tx, Event{Kind: EventRevoked, Request: out, Actor: admin})
	})
	return out, err
}
