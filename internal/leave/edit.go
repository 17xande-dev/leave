package leave

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/17xande-dev/leave/internal/users"
)

// MsgNotEditable is what a refused edit says, wherever it is shown.
const MsgNotEditable = "This request can no longer be edited: withdraw it and apply again."

// MsgEditNegative is added to the balance error of an edit that would go below
// zero. Asking HR makes a different kind of request (it waits for an admin,
// away from the approver), so an edit never does it.
const MsgEditNegative = "To go below zero, withdraw this request and ask HR when you apply again."

// canEdit reports whether u may edit r now: their own pending leave request
// that nobody has begun to decide. A request needing special permission gets a
// step logged at the first approval, so "no steps" also means the approver has
// not acted yet. Shutdown requests have fixed dates, and recorded leave was
// never a request. q is the connection to read the steps on, so Edit can judge
// inside its transaction.
func (s *Service) canEdit(ctx context.Context, q querier, u users.User, r Request) (bool, error) {
	if r.UserID != u.ID || r.Status != StatusPending || r.Kind != KindLeave || r.IsShutdown() || r.IsRecorded() {
		return false, nil
	}
	var n int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM request_steps WHERE request_id = ?`, r.ID).Scan(&n); err != nil {
		return false, err
	}
	return n == 0, nil
}

// CanEdit is canEdit for pages, read on the connection pool.
func (s *Service) CanEdit(ctx context.Context, u users.User, r Request) (bool, error) {
	return s.canEdit(ctx, s.db, u, r)
}

// PreviewEdit is Preview for the form editing request id: the request's own
// old version does not count against overlap, coverage or balance. When id is
// not an editable request of u's, it is an ordinary Preview and editing is
// false, so a made-up or someone else's id excludes nothing. A balance error
// carries MsgEditNegative, since an edit cannot ask HR.
func (s *Service) PreviewEdit(ctx context.Context, u users.User, id int64, in Input) (qt Quote, editing bool, err error) {
	if err := s.ensureYears(ctx, in); err != nil {
		return Quote{}, false, err
	}
	var opt quoteOpts
	if r, gerr := s.Get(ctx, id); gerr == nil {
		if editing, err = s.canEdit(ctx, s.db, u, r); err != nil {
			return Quote{}, false, err
		}
		if editing {
			opt.excludeID = id
		}
	}
	qt, err = s.quote(ctx, s.db, u, in, opt)
	if editing {
		editShortfall(qt, err)
	}
	return qt, editing, err
}

// editShortfall words the balance error of an edit, in place.
func editShortfall(qt Quote, err error) {
	var fe FieldErrors
	if errors.As(err, &fe) && fe["end"] != "" && qt.Shortfall > 0 {
		fe["end"] += " " + MsgEditNegative
	}
}

// Edit changes the type, dates and note of u's own pending request, which stays
// with the approver it was sent to (their frozen approver_id, not a fresh
// ResolveApprover) and keeps its place in the queue. It runs the same checks as
// Submit, with the request's old version out of the overlap, coverage and
// balance sums. An edit that is over a coverage limit is refused unless
// in.Override asks for special permission, exactly as a new submission is, and
// one that is no longer over a limit stops needing it. in.Negative is ignored:
// see MsgEditNegative. Anything that is not an editable request of u's is
// ErrNotAllowed, judged again inside the transaction, so a decision landing
// between the read and the write wins.
func (s *Service) Edit(ctx context.Context, u users.User, id int64, in Input) (Request, error) {
	if err := s.ensureYears(ctx, in); err != nil {
		return Request{}, err
	}
	// Fast refusal, without taking the write lock; re-checked below.
	if r, err := s.Get(ctx, id); err != nil {
		return Request{}, err
	} else if ok, err := s.canEdit(ctx, s.db, u, r); err != nil {
		return Request{}, err
	} else if !ok {
		return Request{}, ErrNotAllowed
	}
	if s.beforeEditTx != nil {
		s.beforeEditTx()
	}
	var out Request
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		old, err := s.get(ctx, tx, id)
		if err != nil {
			return err
		}
		if ok, err := s.canEdit(ctx, tx, u, old); err != nil {
			return err
		} else if !ok {
			return ErrNotAllowed
		}
		qt, err := s.quote(ctx, tx, u, in, quoteOpts{override: in.Override, excludeID: id})
		if err != nil {
			editShortfall(qt, err)
			return err
		}
		if len(qt.Coverage) > 0 && !in.Override {
			return FieldErrors{"coverage": Sentences(qt.Coverage) + " You can ask for special permission."}
		}
		escalate := in.Override && len(qt.Coverage) > 0
		note := strings.TrimSpace(in.Note)
		if qt.Type.ID == old.TypeID && fmtDate(qt.Start) == old.Start && fmtDate(qt.End) == old.End &&
			note == old.Note && escalate == old.Escalate {
			out = old // nothing to save, nobody to tell
			return nil
		}
		// allow_negative goes too: HR's permission was for the request as it was.
		res, err := tx.ExecContext(ctx, `UPDATE requests SET type_id = ?, start_date = ?, end_date = ?, days = ?,
			cycle_start = ?, note = ?, escalate = ?, allow_negative = 0
			WHERE id = ? AND user_id = ? AND status = 'pending' AND kind = 'leave'
			AND shutdown_part_id IS NULL AND recorded_by_id IS NULL
			AND NOT EXISTS (SELECT 1 FROM request_steps WHERE request_id = requests.id)`,
			qt.Type.ID, fmtDate(qt.Start), fmtDate(qt.End), qt.Days, fmtDate(qt.CycleStart), note, escalate, id, u.ID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotAllowed
		}
		if out, err = s.get(ctx, tx, id); err != nil {
			return err
		}
		return s.emit(ctx, tx, Event{Kind: EventEdited, Request: out, Before: &old, Actor: u})
	})
	return out, err
}
