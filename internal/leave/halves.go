package leave

import (
	"context"
	"database/sql"

	"github.com/17xande-dev/leave/internal/users"
)

const MsgHalvesOwn = "Another admin changes half days on your own leave."
const MsgHalvesUnchanged = "The half days are unchanged."
const MsgHalvesChanged = "This request changed since you opened it. Check it again."

// CanSetHalves reports the role and request-state guards for the admin form.
// SetHalves also checks for a pending cancellation inside its transaction.
func CanSetHalves(admin users.User, r Request) bool {
	if !admin.Admin || admin.Disabled || admin.ID == r.UserID || r.Kind != KindLeave || r.IsShutdown() || r.TypeUnit != UnitWorkdays {
		return false
	}
	switch r.Status {
	case StatusPending, StatusApproved, StatusDraft, StatusAwaitingHR:
		return true
	}
	return false
}

// SetHalves corrects the charged first/last day without replacing the request
// or changing its approval state. The audit and notification commit together.
// A nonempty seen fingerprint refuses a change to a request edited since it was read.
func (s *Service) SetHalves(ctx context.Context, admin users.User, id int64, halfStart, halfEnd bool, seen ...string) (Request, error) {
	if !admin.Admin || admin.Disabled {
		return Request{}, ErrNotAllowed
	}
	r, err := s.Get(ctx, id)
	if err != nil {
		return Request{}, err
	}
	if admin.ID == r.UserID {
		return Request{}, FieldErrors{"halves": MsgHalvesOwn}
	}
	if !CanSetHalves(admin, r) {
		return Request{}, ErrNotAllowed
	}
	u, err := s.users.ByID(ctx, r.UserID)
	if err != nil {
		return Request{}, err
	}
	if err := s.ensureYears(ctx, Input{Start: r.Start, End: r.End}); err != nil {
		return Request{}, err
	}
	var out Request
	if s.beforeHalvesTx != nil {
		s.beforeHalvesTx()
	}
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		old, err := s.get(ctx, tx, id)
		if err != nil {
			return err
		}
		if admin.ID == old.UserID {
			return FieldErrors{"halves": MsgHalvesOwn}
		}
		if !CanSetHalves(admin, old) {
			return ErrNotAllowed
		}
		if len(seen) > 0 && seen[0] != "" && seen[0] != old.Fingerprint() {
			return ErrChanged
		}
		var pending bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM requests WHERE cancels_request_id = ? AND status = 'pending')`, id).Scan(&pending); err != nil {
			return err
		}
		if pending {
			return FieldErrors{"halves": "Decide or withdraw the pending cancellation before changing half days."}
		}
		if old.HalfStart == halfStart && old.HalfEnd == halfEnd {
			return FieldErrors{"halves": MsgHalvesUnchanged}
		}
		in := Input{TypeID: old.TypeID, Ranges: old.Ranges, Note: old.Note,
			HalfStart: halfStart, HalfEnd: halfEnd}
		days := old.Days + halfDays(Input{HalfStart: old.HalfStart, HalfEnd: old.HalfEnd}) - halfDays(in)
		qt, err := s.quote(ctx, tx, u, in, quoteOpts{adjust: true, adjustedDays: days, halvesAllowed: true, excludeID: id, cycleStart: old.CycleStart})
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE requests SET half_start = ?, half_end = ?, days_x100 = ?, allow_negative = ?, halves_set_by_id = ?, halves_set_at = ? WHERE id = ?`,
			halfStart, halfEnd, qt.Days, qt.Shortfall > 0, admin.ID, stampNow(s.Now()), id)
		if err != nil {
			return err
		}
		if out, err = s.get(ctx, tx, id); err != nil {
			return err
		}
		return s.hook(ctx, tx, Event{Kind: EventHalvesChanged, Request: out, Before: &old, Actor: admin})
	})
	return out, err
}
