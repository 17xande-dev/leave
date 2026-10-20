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
//  3. FINAL_APPROVER, for anyone with no usable manager;
//  4. the admins -- where FINAL_APPROVER's own leave goes, since nobody is
//     above them.
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

	var overrideID int64
	err := s.db.QueryRowContext(ctx, `SELECT approver_id FROM approver_overrides WHERE user_id = ?`, u.ID).Scan(&overrideID)
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
	if s.cfg.FinalApprover != "" {
		c, err := s.users.ByUsername(ctx, s.cfg.FinalApprover)
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

// Withdraw takes back a request that has not been decided yet.
func (s *Service) Withdraw(ctx context.Context, u users.User, id int64) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE requests SET status = 'withdrawn', decided_at = ?
			WHERE id = ? AND user_id = ? AND status = 'pending'`, stampNow(s.Now()), id, u.ID)
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
	return r.UserID == u.ID && r.Kind == KindLeave && r.Status == StatusApproved && r.Start > fmtDate(s.today())
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
func (s *Service) Decide(ctx context.Context, actor users.User, id int64, approve bool, note string) (Request, error) {
	note = strings.TrimSpace(note)
	if !approve && note == "" {
		return Request{}, FieldErrors{"note": "Say why, so they know what to change."}
	}
	var out Request
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		r, err := s.get(ctx, tx, id)
		if err != nil {
			return err
		}
		if !CanDecide(actor, r) {
			return ErrNotAllowed
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
		return s.hook(ctx, tx, Event{Kind: EventReassigned, Request: out, Actor: admin})
	})
	return out, err
}

// SetCalendarEventID records the shared-calendar event made for approved leave.
func (s *Service) SetCalendarEventID(ctx context.Context, id int64, eventID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE requests SET calendar_event_id = ? WHERE id = ?`, eventID, id)
	return err
}
