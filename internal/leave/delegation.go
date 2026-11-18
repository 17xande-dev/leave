package leave

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/17xande-dev/leave/internal/users"
)

// MaxDelegationDays is the longest a stand-in may cover, counting the first
// and the last day: a delegation starting on day D ends on D+29 at the latest.
const MaxDelegationDays = 30

// MsgDelegateUnusable is the refusal for a stand-in who is unknown, disabled
// or the delegator themselves.
const MsgDelegateUnusable = "Choose someone else who can sign in."

// MaxDelegationNote is the longest note, in characters.
const MaxDelegationNote = 500

// Delegation is an approver's stand-in for a date range: while it is active
// today the delegate may also decide the requests waiting for the delegator.
// The requests stay with the delegator; they revert when it ends.
type Delegation struct {
	ID, DelegatorID, DelegateID, CreatedByID, RevokedByID int64
	// Start and End are YYYY-MM-DD, both inclusive. RevokedAt is empty while
	// the delegation has not been revoked.
	Start, End, Note, CreatedAt, RevokedAt            string
	DelegatorName, DelegateName, CreatedBy, RevokedBy string
}

// Active reports whether the delegation lets the delegate act on today (a
// YYYY-MM-DD date): not revoked, and today within its range.
func (d Delegation) Active(today string) bool {
	return d.RevokedAt == "" && d.Start <= today && today <= d.End
}

const delegationSelect = `SELECT d.id, d.delegator_id, d.delegate_id, d.created_by_id, COALESCE(d.revoked_by_id, 0),
	d.start_date, d.end_date, d.note, d.created_at, COALESCE(d.revoked_at, ''),
	dr.name, de.name, cb.name, COALESCE(rb.name, '')
	FROM approval_delegations d
	JOIN users dr ON dr.id = d.delegator_id
	JOIN users de ON de.id = d.delegate_id
	JOIN users cb ON cb.id = d.created_by_id
	LEFT JOIN users rb ON rb.id = d.revoked_by_id`

func scanDelegation(row interface{ Scan(...any) error }) (Delegation, error) {
	var d Delegation
	err := row.Scan(&d.ID, &d.DelegatorID, &d.DelegateID, &d.CreatedByID, &d.RevokedByID,
		&d.Start, &d.End, &d.Note, &d.CreatedAt, &d.RevokedAt,
		&d.DelegatorName, &d.DelegateName, &d.CreatedBy, &d.RevokedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return Delegation{}, ErrNotFound
	}
	return d, err
}

func (s *Service) getDelegation(ctx context.Context, q querier, id int64) (Delegation, error) {
	return scanDelegation(q.QueryRowContext(ctx, delegationSelect+` WHERE d.id = ?`, id))
}

func dateLabel(day string) string {
	t, err := ParseDate(day)
	if err != nil {
		return day
	}
	return t.Format("Mon 2 Jan 2006")
}

// Delegate names delegateID as delegatorID's stand-in from start to end (both
// YYYY-MM-DD, inclusive). The delegator sets it up for themselves; an enabled
// admin may for anyone. It is refused as a field error when the delegate
// cannot sign in or is the delegator, when the range starts in the past, ends
// before it starts or runs longer than MaxDelegationDays, and when the
// delegator already has an unrevoked stand-in over any of those days.
func (s *Service) Delegate(ctx context.Context, actor users.User, delegatorID, delegateID int64, start, end, note string) (Delegation, error) {
	if actor.Disabled || (actor.ID != delegatorID && !actor.Admin) {
		return Delegation{}, ErrNotAllowed
	}
	note = strings.TrimSpace(note)
	errs := FieldErrors{}
	today := s.today()
	startDay, err := ParseDate(start)
	switch {
	case err != nil:
		errs["start"] = "Enter the first day."
	case startDay.Before(today):
		errs["start"] = "Start today or later."
	}
	endDay, err := ParseDate(end)
	switch {
	case err != nil:
		errs["end"] = "Enter the last day."
	case errs["start"] == "" && endDay.Before(startDay):
		errs["end"] = "The last day cannot be before the first."
	case errs["start"] == "" && endDay.After(startDay.AddDate(0, 0, MaxDelegationDays-1)):
		errs["end"] = fmt.Sprintf("A stand-in can cover at most %d days.", MaxDelegationDays)
	}
	if utf8.RuneCountInString(note) > MaxDelegationNote {
		errs["note"] = fmt.Sprintf("Keep the note under %d characters.", MaxDelegationNote)
	}
	var out Delegation
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		var delegatorName string
		if err := tx.QueryRowContext(ctx, `SELECT name FROM users WHERE id = ?`, delegatorID).Scan(&delegatorName); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrNotAllowed
			}
			return err
		}
		// Read under the write lock: a directory sync may have disabled the
		// delegate since the form was opened.
		var disabled bool
		err := tx.QueryRowContext(ctx, `SELECT disabled FROM users WHERE id = ?`, delegateID).Scan(&disabled)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			errs["delegate"] = MsgDelegateUnusable
		case err != nil:
			return err
		case disabled || delegateID == delegatorID:
			errs["delegate"] = MsgDelegateUnusable
		}
		if len(errs) > 0 {
			return errs
		}
		var os, oe string
		err = tx.QueryRowContext(ctx, `SELECT start_date, end_date FROM approval_delegations
			WHERE delegator_id = ? AND revoked_at IS NULL AND start_date <= ? AND end_date >= ?
			ORDER BY start_date LIMIT 1`, delegatorID, end, start).Scan(&os, &oe)
		if err == nil {
			return FieldErrors{"delegate": fmt.Sprintf("%s already has a stand-in from %s to %s. Revoke it first.",
				delegatorName, dateLabel(os), dateLabel(oe))}
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO approval_delegations
			(delegator_id, delegate_id, start_date, end_date, note, created_by_id, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			delegatorID, delegateID, start, end, note, actor.ID, stampNow(s.Now()))
		if err != nil {
			return err
		}
		id, _ := res.LastInsertId()
		if out, err = s.getDelegation(ctx, tx, id); err != nil {
			return err
		}
		return s.hook(ctx, tx, Event{Kind: EventDelegated, Actor: actor, Delegation: &out})
	})
	return out, err
}

// RevokeDelegation ends a delegation now. The delegator or an enabled admin
// may; one already revoked or already over is ErrNotAllowed.
func (s *Service) RevokeDelegation(ctx context.Context, actor users.User, id int64) (Delegation, error) {
	if actor.Disabled {
		return Delegation{}, ErrNotAllowed
	}
	var out Delegation
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		d, err := s.getDelegation(ctx, tx, id)
		if errors.Is(err, ErrNotFound) {
			return ErrNotAllowed
		}
		if err != nil {
			return err
		}
		if (actor.ID != d.DelegatorID && !actor.Admin) || d.RevokedAt != "" || d.End < fmtDate(s.today()) {
			return ErrNotAllowed
		}
		res, err := tx.ExecContext(ctx, `UPDATE approval_delegations SET revoked_by_id = ?, revoked_at = ?
			WHERE id = ? AND revoked_at IS NULL`, actor.ID, stampNow(s.Now()), id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotAllowed
		}
		if out, err = s.getDelegation(ctx, tx, id); err != nil {
			return err
		}
		return s.hook(ctx, tx, Event{Kind: EventDelegationRevoked, Actor: actor, Delegation: &out})
	})
	return out, err
}

// DelegationFilter selects delegations for a page. With neither person set it
// covers everyone. Past asks for the last 20 that are over or revoked instead
// of the current and upcoming ones (unrevoked, ending today or later).
type DelegationFilter struct {
	DelegatorID, DelegateID int64
	Past                    bool
}

// Delegations lists delegations by start date: current and upcoming ones
// soonest first, or with Past the 20 most recent that are over or revoked.
func (s *Service) Delegations(ctx context.Context, f DelegationFilter) ([]Delegation, error) {
	where := []string{}
	var args []any
	today := fmtDate(s.today())
	if f.Past {
		where = append(where, `(d.end_date < ? OR d.revoked_at IS NOT NULL)`)
		args = append(args, today)
	} else {
		where = append(where, `d.revoked_at IS NULL AND d.end_date >= ?`)
		args = append(args, today)
	}
	if f.DelegatorID != 0 {
		where = append(where, `d.delegator_id = ?`)
		args = append(args, f.DelegatorID)
	}
	if f.DelegateID != 0 {
		where = append(where, `d.delegate_id = ?`)
		args = append(args, f.DelegateID)
	}
	q := delegationSelect + ` WHERE ` + strings.Join(where, " AND ")
	if f.Past {
		q += ` ORDER BY d.start_date DESC, d.id DESC LIMIT 20`
	} else {
		q += ` ORDER BY d.start_date, d.id`
	}
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Delegation
	for rows.Next() {
		d, err := scanDelegation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Grant is one approver u is standing in for today.
type Grant struct {
	DelegatorID, CreatedByID int64
	DelegatorName, Until     string
}

// ActingFor lists who u is a stand-in for today: unrevoked delegations naming
// u whose range includes today. Nothing for a disabled user. One level only:
// someone who delegated to u's delegator does not appear, so there are no
// chains.
func (s *Service) ActingFor(ctx context.Context, q querier, u users.User) ([]Grant, error) {
	if u.Disabled {
		return nil, nil
	}
	today := fmtDate(s.today())
	rows, err := q.QueryContext(ctx, `SELECT d.delegator_id, d.created_by_id, dr.name, d.end_date
		FROM approval_delegations d JOIN users dr ON dr.id = d.delegator_id
		WHERE d.delegate_id = ? AND d.revoked_at IS NULL AND d.start_date <= ? AND d.end_date >= ?
		ORDER BY d.id`, u.ID, today, today)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Grant
	for rows.Next() {
		var g Grant
		if err := rows.Scan(&g.DelegatorID, &g.CreatedByID, &g.DelegatorName, &g.Until); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// StandingInFor is ActingFor for callers outside a transaction (pages).
func (s *Service) StandingInFor(ctx context.Context, u users.User) ([]Grant, error) {
	return s.ActingFor(ctx, s.db, u)
}

// CanDecideNow is CanDecide plus delegation: onBehalfOf is 0 when u decides
// as the request's own approver (or as an admin in the admins' queue), and the
// delegator's id when u decides as their stand-in. A stand-in must be able to
// act under the usual rules (pending, not their own request, enabled), the
// request must be with the delegator (the admins' queue is never delegated),
// and the delegation must not have been set up by the request's owner.
func (s *Service) CanDecideNow(ctx context.Context, q querier, u users.User, r Request) (onBehalfOf int64, ok bool, err error) {
	if CanDecide(u, r) {
		return 0, true, nil
	}
	if r.Status != StatusPending || u.ID == r.UserID || u.Disabled || r.ApproverID == 0 {
		return 0, false, nil
	}
	grants, err := s.ActingFor(ctx, q, u)
	if err != nil {
		return 0, false, err
	}
	for _, g := range grants {
		if g.DelegatorID == r.ApproverID && g.CreatedByID != r.UserID {
			return r.ApproverID, true, nil
		}
	}
	return 0, false, nil
}

// CanDecideToday is CanDecideNow for callers outside a transaction (pages and
// tools), which only need the answer.
func (s *Service) CanDecideToday(ctx context.Context, u users.User, r Request) (bool, error) {
	_, ok, err := s.CanDecideNow(ctx, s.db, u, r)
	return ok, err
}

// CanViewNow is CanView, and also true for whoever may decide r today as a
// stand-in, so they can open what they are asked to decide.
func (s *Service) CanViewNow(ctx context.Context, u users.User, r Request) (bool, error) {
	if CanView(u, r) {
		return true, nil
	}
	return s.CanDecideToday(ctx, u, r)
}

// ActiveStandIn is approverID's delegation active today, or nil.
func (s *Service) ActiveStandIn(ctx context.Context, q querier, approverID int64) (*Delegation, error) {
	today := fmtDate(s.today())
	d, err := scanDelegation(q.QueryRowContext(ctx, delegationSelect+`
		WHERE d.delegator_id = ? AND d.revoked_at IS NULL AND d.start_date <= ? AND d.end_date >= ?
		AND de.disabled = 0
		ORDER BY d.id LIMIT 1`, approverID, today, today))
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// actingApprover is whose approval u's decision on r is: u's own, or, when u is
// not r's approver, the approver's. It is only a lookup; CanDecideNow decides
// whether u may.
func (s *Service) actingApprover(ctx context.Context, u users.User, r Request) (users.User, error) {
	if r.ApproverID == 0 || r.ApproverID == u.ID {
		return u, nil
	}
	return s.users.ByID(ctx, r.ApproverID)
}

// Delegation is one delegation by id, or ErrNotFound. It checks nothing about
// who may see it: callers decide that.
func (s *Service) Delegation(ctx context.Context, id int64) (Delegation, error) {
	return s.getDelegation(ctx, s.db, id)
}

// InDelegation is whether u is the delegator or the delegate of any unrevoked
// delegation that has not ended: what decides if the "Away" link shows.
func (s *Service) InDelegation(ctx context.Context, u users.User) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM approval_delegations
		WHERE (delegator_id = ? OR delegate_id = ?) AND revoked_at IS NULL AND end_date >= ?`,
		u.ID, u.ID, fmtDate(s.today())).Scan(&n)
	return n > 0, err
}

// StandInFor is who is approving for r's approver today, for showing beside
// the approver: nil when there is nobody, when r is not pending with a named
// approver, or when the only stand-in could not act on r (the requester set the
// delegation up or is the stand-in: CanDecideNow refuses both).
func (s *Service) StandInFor(ctx context.Context, r Request) (*Delegation, error) {
	if r.Status != StatusPending || r.ApproverID == 0 {
		return nil, nil
	}
	d, err := s.ActiveStandIn(ctx, s.db, r.ApproverID)
	if err != nil || d == nil || d.CreatedByID == r.UserID || d.DelegateID == r.UserID {
		return nil, err
	}
	return d, nil
}
