package leave

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/17xande-dev/leave/internal/users"
)

// ResolveApprover returns who approves u's requests, or 0 for the admins'
// queue. In order:
//
//  1. an admin's override for u;
//  2. u's manager in the directory (a department head's manager is the senior
//     person above them, so heads need nothing special);
//  3. the final approver (Admin -> Settings), for anyone with no usable
//     manager;
//  4. the admins -- where the final approver's own leave goes, since nobody
//     is above them, and everyone's when none is set or they are disabled.
//
// A candidate is skipped if disabled or if it is u: nobody approves their own
// leave, whatever the directory says.
func (s *Service) ResolveApprover(ctx context.Context, u users.User) (int64, error) {
	usable := func(c users.User, err error) (bool, error) {
		if errors.Is(err, users.ErrNotFound) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return !c.Disabled && c.ID != u.ID, nil
	}

	st, err := s.settings(ctx, s.db)
	if err != nil {
		return 0, err
	}
	var overrideID int64
	err = s.db.QueryRowContext(ctx, `SELECT approver_id FROM approver_overrides WHERE user_id = ?`, u.ID).Scan(&overrideID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if overrideID != 0 {
		c, err := s.users.ByID(ctx, overrideID)
		if ok, err := usable(c, err); err != nil || ok {
			return c.ID, err
		}
	}
	if strings.TrimSpace(u.ManagerDN) != "" {
		c, err := s.users.ByDN(ctx, u.ManagerDN)
		if ok, err := usable(c, err); err != nil || ok {
			return c.ID, err
		}
	}
	if st.FinalApproverID != 0 {
		c, err := s.users.ByID(ctx, st.FinalApproverID)
		if ok, err := usable(c, err); err != nil || ok {
			return c.ID, err
		}
	}
	return 0, nil
}

// SetOverride routes u's future requests to approverID; 0 removes the override.
// Requests already pending keep the approver they were submitted to -- an admin
// reassigns those explicitly.
func (s *Service) SetOverride(ctx context.Context, userID, approverID int64) error {
	if approverID == 0 {
		_, err := s.db.ExecContext(ctx, `DELETE FROM approver_overrides WHERE user_id = ?`, userID)
		return err
	}
	if approverID == userID {
		return FieldErrors{"approver": "Nobody can approve their own leave."}
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO approver_overrides (user_id, approver_id) VALUES (?, ?)
		ON CONFLICT(user_id) DO UPDATE SET approver_id = excluded.approver_id`, userID, approverID)
	return err
}

// Override returns the override approver for userID, or 0.
func (s *Service) Override(ctx context.Context, userID int64) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT approver_id FROM approver_overrides WHERE user_id = ?`, userID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return id, err
}

// Withdraw takes back a request that has not been decided yet: pending, or
// waiting for HR, or a draft HR allowed. A shutdown request cannot be
// withdrawn: the days are mandatory.
func (s *Service) Withdraw(ctx context.Context, u users.User, id int64) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE requests SET status = 'withdrawn', decided_at = ?
			WHERE id = ? AND user_id = ? AND status IN ('pending', 'awaiting_hr', 'draft')
			AND shutdown_part_id IS NULL`,
			stampNow(s.Now()), id, u.ID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotAllowed
		}
		r, err := s.get(ctx, tx, id)
		if err != nil {
			return err
		}
		return s.hook(ctx, tx, Event{Kind: EventWithdrawn, Request: r, Actor: u})
	})
}

// CanRequestCancellation reports whether r is approved leave, belonging to u,
// that has not started yet. Cancelling leave already under way is an open
// decision (what does it mean for the days already taken?); for now an admin
// handles it with a balance adjustment.
func (s *Service) CanRequestCancellation(u users.User, r Request) bool {
	return r.UserID == u.ID && r.Kind == KindLeave && r.Status == StatusApproved && !r.IsShutdown() &&
		r.Start > fmtDate(s.today())
}

// RequestCancellation asks for approved leave to be cancelled. The request
// goes through the same approval chain as leave does; the leave stays booked
// until it is approved.
func (s *Service) RequestCancellation(ctx context.Context, u users.User, id int64, note string) (Request, error) {
	orig, err := s.Get(ctx, id)
	if err != nil {
		return Request{}, err
	}
	if !s.CanRequestCancellation(u, orig) {
		return Request{}, ErrNotAllowed
	}
	approver, err := s.ResolveApprover(ctx, u)
	if err != nil {
		return Request{}, err
	}
	var out Request
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO requests (user_id, type_id, kind, cancels_request_id,
			start_date, end_date, days, cycle_start, note, status, approver_id, created_at)
			SELECT user_id, type_id, 'cancellation', id, start_date, end_date, days, cycle_start, ?, 'pending', ?, ?
			FROM requests WHERE id = ? AND status = 'approved'`,
			strings.TrimSpace(note), nullID(approver), stampNow(s.Now()), id)
		if err != nil {
			// idx_requests_one_cancellation: one is already waiting.
			if strings.Contains(err.Error(), "UNIQUE") {
				return ErrNotAllowed
			}
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotAllowed
		}
		newID, _ := res.LastInsertId()
		if out, err = s.get(ctx, tx, newID); err != nil {
			return err
		}
		return s.hook(ctx, tx, Event{Kind: EventSubmitted, Request: out, Actor: u})
	})
	return out, err
}

// PendingCancellation returns the cancellation waiting on leave request id, if any.
func (s *Service) PendingCancellation(ctx context.Context, id int64) (*Request, error) {
	rs, err := s.queryRequests(ctx, s.db, `WHERE r.cancels_request_id = ? AND r.status = 'pending'`, id)
	if err != nil || len(rs) == 0 {
		return nil, err
	}
	return &rs[0], nil
}

// Decide approves or declines a pending request. Approving a cancellation
// marks the leave it cancels as cancelled, which returns its days to the
// balance (balances count approved leave only).
//
// A request needing special permission (escalate) is approved level by level:
// each approval is logged and, unless the approver is the final one, passes the
// request to their own approver. Declining at any level ends it.
func (s *Service) Decide(ctx context.Context, actor users.User, id int64, approve bool, note string) (Request, error) {
	note = strings.TrimSpace(note)
	if !approve && note == "" {
		return Request{}, FieldErrors{"note": "Say why, so they know what to change."}
	}
	// Who comes next is decided before the transaction: it reads the
	// directory through other connections, which must not wait on the write
	// lock the transaction takes. The request's approver is re-checked inside.
	before, err := s.Get(ctx, id)
	if err != nil {
		return Request{}, err
	}
	var next int64
	if approve && before.Escalate && before.Kind == KindLeave {
		if next, err = s.escalationNext(ctx, s.db, before, actor); err != nil {
			return Request{}, err
		}
	}
	var out Request
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		r, err := s.get(ctx, tx, id)
		if err != nil {
			return err
		}
		if !CanDecide(actor, r) || r.ApproverID != before.ApproverID {
			return ErrNotAllowed
		}
		if r.Escalate {
			decision := "approved"
			if !approve {
				decision = "declined"
			}
			if err := s.logStep(ctx, tx, id, actor.ID, decision, note); err != nil {
				return err
			}
			if approve && next != 0 {
				res, err := tx.ExecContext(ctx, `UPDATE requests SET approver_id = ?
					WHERE id = ? AND status = 'pending'`, next, id)
				if err != nil {
					return err
				}
				if n, _ := res.RowsAffected(); n == 0 {
					return ErrNotAllowed
				}
				if out, err = s.get(ctx, tx, id); err != nil {
					return err
				}
				return s.emit(ctx, tx, Event{Kind: EventEscalated, Request: out, Actor: actor})
			}
		}
		status := StatusDeclined
		if approve {
			status = StatusApproved
		}
		res, err := tx.ExecContext(ctx, `UPDATE requests SET status = ?, decided_by_id = ?, decided_at = ?,
			decision_note = ? WHERE id = ? AND status = 'pending'`, status, actor.ID, stampNow(s.Now()), note, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotAllowed
		}

		ev := Event{Kind: EventDecided, Actor: actor}
		if approve && r.Kind == KindCancellation {
			res, err := tx.ExecContext(ctx, `UPDATE requests SET status = 'cancelled'
				WHERE id = ? AND status = 'approved'`, r.CancelsID)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n != 1 {
				return ErrNotAllowed
			}
			c, err := s.get(ctx, tx, r.CancelsID)
			if err != nil {
				return err
			}
			ev.Cancelled = &c
		}
		if out, err = s.get(ctx, tx, id); err != nil {
			return err
		}
		ev.Request = out
		return s.hook(ctx, tx, ev)
	})
	return out, err
}

// escalationNext is who an escalated request moves to when actor approves it,
// or 0 when the approval is final. It is final when actor is the final
// approver, when the request is in the admins' queue (one admin finishes it),
// when nobody sits above actor, when that person is the requester, and when
// they have already been through this request -- so it cannot loop.
func (s *Service) escalationNext(ctx context.Context, q querier, r Request, actor users.User) (int64, error) {
	st, err := s.settings(ctx, q)
	if err != nil {
		return 0, err
	}
	if r.ApproverID == 0 || (st.FinalApproverID != 0 && actor.ID == st.FinalApproverID) {
		return 0, nil
	}
	next, err := s.ResolveApprover(ctx, actor)
	if err != nil || next == 0 || next == r.UserID || next == actor.ID {
		return 0, err
	}
	var seen int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM request_steps WHERE request_id = ? AND approver_id = ?`,
		r.ID, next).Scan(&seen); err != nil || seen > 0 {
		return 0, err
	}
	return next, nil
}

// NextApprover is who an escalated request would go to if actor approved it;
// ok is false when their approval would be the last.
func (s *Service) NextApprover(ctx context.Context, actor users.User, r Request) (u users.User, ok bool, err error) {
	if !r.Escalate || r.Kind != KindLeave {
		return users.User{}, false, nil
	}
	id, err := s.escalationNext(ctx, s.db, r, actor)
	if err != nil || id == 0 {
		return users.User{}, false, err
	}
	u, err = s.users.ByID(ctx, id)
	return u, err == nil, err
}

// Step is one entry in a request's trail of decisions.
type Step struct {
	ID           int64
	RequestID    int64
	ApproverID   int64
	ApproverName string
	// Decision is approved, declined, granted or refused.
	Decision  string
	Note      string
	DecidedAt string
}

func (s *Service) logStep(ctx context.Context, tx *sql.Tx, requestID, approverID int64, decision, note string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO request_steps (request_id, approver_id, decision, note, decided_at)
		VALUES (?, ?, ?, ?, ?)`, requestID, approverID, decision, note, stampNow(s.Now()))
	return err
}

// Steps lists the decisions made on a request, in order.
func (s *Service) Steps(ctx context.Context, requestID int64) ([]Step, error) {
	return s.steps(ctx, s.db, requestID)
}

func (s *Service) steps(ctx context.Context, q querier, requestID int64) ([]Step, error) {
	rows, err := q.QueryContext(ctx, `SELECT st.id, st.request_id, st.approver_id, u.name, st.decision, st.note,
		st.decided_at FROM request_steps st JOIN users u ON u.id = st.approver_id
		WHERE st.request_id = ? ORDER BY st.id`, requestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Step
	for rows.Next() {
		var st Step
		if err := rows.Scan(&st.ID, &st.RequestID, &st.ApproverID, &st.ApproverName, &st.Decision, &st.Note,
			&st.DecidedAt); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// emit is hook for events about a request that may need special permission:
// it attaches the coverage warnings and the trail so far, which the messages
// quote, as they stand inside this transaction.
func (s *Service) emit(ctx context.Context, tx *sql.Tx, e Event) error {
	if e.Request.Escalate && e.Request.Status == StatusPending {
		var err error
		if e.Coverage, err = s.coverageFor(ctx, tx, e.Request); err != nil {
			return err
		}
		if e.Steps, err = s.steps(ctx, tx, e.Request.ID); err != nil {
			return err
		}
	}
	return s.hook(ctx, tx, e)
}

// AwaitingHR lists the requests waiting for an admin to allow a negative
// balance, oldest first.
func (s *Service) AwaitingHR(ctx context.Context) ([]Request, error) {
	return s.queryRequests(ctx, s.db, `WHERE r.status = 'awaiting_hr' ORDER BY r.created_at, r.id`)
}

// Shortfall is how many days below zero a request awaiting HR would take the
// balance, as it stands now. It reserves nothing yet, so its days are added to
// what is already booked.
func (s *Service) Shortfall(ctx context.Context, r Request) (int, error) {
	return s.shortfall(ctx, s.db, r)
}

func (s *Service) shortfall(ctx context.Context, q querier, r Request) (int, error) {
	t, err := scanType(q.QueryRowContext(ctx, `SELECT `+typeCols+` FROM leave_types WHERE id = ?`, r.TypeID))
	if err != nil {
		return 0, err
	}
	cycle, err := ParseDate(r.CycleStart)
	if err != nil {
		return 0, err
	}
	b, err := s.balance(ctx, q, r.UserID, t, cycle)
	if err != nil {
		return 0, err
	}
	avail := b.Available()
	if avail == nil || r.Days <= *avail {
		return 0, nil
	}
	return r.Days - *avail, nil
}

// GrantNegative lets an admin allow a request to take the balance below zero.
// The request goes back to its owner as a draft to submit, with the permission
// recorded on it: nothing is reserved, and nothing is approved, until then.
func (s *Service) GrantNegative(ctx context.Context, admin users.User, id int64, note string) (Request, error) {
	if !admin.Admin || admin.Disabled {
		return Request{}, ErrNotAllowed
	}
	note = strings.TrimSpace(note)
	var out Request
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		r, err := s.get(ctx, tx, id)
		if err != nil {
			return err
		}
		// Nobody grants themselves permission, admin or not.
		if r.UserID == admin.ID {
			return ErrNotAllowed
		}
		res, err := tx.ExecContext(ctx, `UPDATE requests SET status = 'draft', allow_negative = 1
			WHERE id = ? AND status = 'awaiting_hr'`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotAllowed
		}
		if err := s.logStep(ctx, tx, id, admin.ID, "granted", note); err != nil {
			return err
		}
		if out, err = s.get(ctx, tx, id); err != nil {
			return err
		}
		return s.hook(ctx, tx, Event{Kind: EventNegativeGranted, Request: out, Actor: admin, Note: note})
	})
	return out, err
}

// RefuseNegative lets an admin turn a negative-balance request down. It ends
// as declined, and the person is told as for any declined request.
func (s *Service) RefuseNegative(ctx context.Context, admin users.User, id int64, note string) (Request, error) {
	if !admin.Admin || admin.Disabled {
		return Request{}, ErrNotAllowed
	}
	note = strings.TrimSpace(note)
	if note == "" {
		return Request{}, FieldErrors{"note": "Say why, so they know what to change."}
	}
	var out Request
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		r, err := s.get(ctx, tx, id)
		if err != nil {
			return err
		}
		if r.UserID == admin.ID {
			return ErrNotAllowed
		}
		res, err := tx.ExecContext(ctx, `UPDATE requests SET status = 'declined', decided_by_id = ?, decided_at = ?,
			decision_note = ? WHERE id = ? AND status = 'awaiting_hr'`, admin.ID, stampNow(s.Now()), note, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotAllowed
		}
		if err := s.logStep(ctx, tx, id, admin.ID, "refused", note); err != nil {
			return err
		}
		if out, err = s.get(ctx, tx, id); err != nil {
			return err
		}
		return s.hook(ctx, tx, Event{Kind: EventDecided, Request: out, Actor: admin})
	})
	return out, err
}

// Reassign moves a pending request to another approver (0: the admins' queue).
func (s *Service) Reassign(ctx context.Context, admin users.User, id, approverID int64) (Request, error) {
	if !admin.Admin {
		return Request{}, ErrNotAllowed
	}
	if approverID != 0 {
		a, err := s.users.ByID(ctx, approverID)
		if err != nil || a.Disabled {
			return Request{}, FieldErrors{"approver": "Choose someone who can currently sign in."}
		}
	}
	var out Request
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE requests SET approver_id = ?
			WHERE id = ? AND status = 'pending' AND user_id <> ?`, nullID(approverID), id, approverID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotAllowed
		}
		if out, err = s.get(ctx, tx, id); err != nil {
			return err
		}
		return s.emit(ctx, tx, Event{Kind: EventReassigned, Request: out, Actor: admin})
	})
	return out, err
}

// SetCalendarEventID records the shared-calendar event made for approved leave.
func (s *Service) SetCalendarEventID(ctx context.Context, id int64, eventID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE requests SET calendar_event_id = ? WHERE id = ?`, eventID, id)
	return err
}
