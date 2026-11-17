package leave

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"github.com/17xande-dev/leave/internal/users"
)

// The refusals of Service.Correct, named so pages, the assistant and tests say
// the same thing.
const (
	MsgCorrectOwn       = "Another admin corrects your own leave."
	MsgCorrectReason    = "Say why you are changing it; the person is told."
	MsgCorrectStarted   = "Someone has started approving this. Wait until it is decided, then correct it."
	MsgCorrectCancel    = "Decide or withdraw the pending cancellation before correcting this request."
	MsgCorrectUnchanged = "Nothing changed."
)

// CanCorrect reports the role and request-state guards for the admin's
// "correct this request" form: an enabled admin, someone else's leave request
// that is not shutdown or recorded leave and has not been approved. Correct
// also looks for approval steps and a pending cancellation inside its
// transaction (CorrectBlocked, for pages).
func CanCorrect(admin users.User, r Request) bool {
	if !admin.Admin || admin.Disabled || admin.ID == r.UserID || r.Kind != KindLeave || r.IsShutdown() || r.IsRecorded() {
		return false
	}
	switch r.Status {
	case StatusPending, StatusAwaitingHR, StatusDraft:
		return true
	}
	return false
}

// correctBlocked is what stops a correctable request being corrected right now:
// a request already part-approved through special permission, or a pending
// cancellation of it. It is read on q so Correct can judge inside its
// transaction.
func (s *Service) correctBlocked(ctx context.Context, q querier, r Request) error {
	if r.Status == StatusPending {
		var steps int
		if err := q.QueryRowContext(ctx, `SELECT count(*) FROM request_steps WHERE request_id = ?`, r.ID).Scan(&steps); err != nil {
			return err
		}
		if steps > 0 {
			return FieldErrors{"correct": MsgCorrectStarted}
		}
	}
	var cancelling bool
	if err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM requests WHERE cancels_request_id = ? AND status = 'pending')`, r.ID).Scan(&cancelling); err != nil {
		return err
	}
	if cancelling {
		return FieldErrors{"correct": MsgCorrectCancel}
	}
	return nil
}

// CorrectBlocked is correctBlocked for pages: the sentence saying why r cannot
// be corrected right now, or "".
func (s *Service) CorrectBlocked(ctx context.Context, r Request) (string, error) {
	err := s.correctBlocked(ctx, s.db, r)
	var fe FieldErrors
	if errors.As(err, &fe) {
		return fe["correct"], nil
	}
	return "", err
}

// correctionSnapshot is what a request said, stored on each side of a
// correction so a later change to a type's name or a user's settings cannot
// rewrite history.
type correctionSnapshot struct {
	TypeID    int64       `json:"type_id"`
	Type      string      `json:"type"`
	Ranges    []snapRange `json:"ranges"`
	DaysX100  int64       `json:"days_x100"`
	HalfStart bool        `json:"half_start"`
	HalfEnd   bool        `json:"half_end"`
	Note      string      `json:"note"`
}

type snapRange struct {
	Start string `json:"start"`
	End   string `json:"end"`
}

func snapshotOf(r Request) correctionSnapshot {
	c := correctionSnapshot{TypeID: r.TypeID, Type: r.TypeName, DaysX100: int64(r.Days),
		HalfStart: r.HalfStart, HalfEnd: r.HalfEnd, Note: r.Note}
	for _, rg := range r.Ranges {
		c.Ranges = append(c.Ranges, snapRange{rg.Start, rg.End})
	}
	return c
}

// Correction is one admin correction of a request, as the page lists it.
type Correction struct {
	ID        int64
	AdminID   int64
	AdminName string
	At        string
	Reason    string
	Before    Request
	After     Request
}

func (c correctionSnapshot) request(unit string) Request {
	r := Request{TypeID: c.TypeID, TypeName: c.Type, TypeUnit: unit, Days: Days(c.DaysX100),
		HalfStart: c.HalfStart, HalfEnd: c.HalfEnd, Note: c.Note}
	for _, rg := range c.Ranges {
		r.Ranges = append(r.Ranges, Range{rg.Start, rg.End})
	}
	if len(r.Ranges) > 0 {
		r.Start, r.End = r.Ranges[0].Start, r.Ranges[len(r.Ranges)-1].End
	}
	return r
}

// Corrections lists the corrections of one request, oldest first. Each side is
// a Request carrying only what the snapshot held (type, dates, days, halves,
// note), so pages and mail format it as they format any request.
func (s *Service) Corrections(ctx context.Context, requestID int64) ([]Correction, error) {
	var unit string
	if err := s.db.QueryRowContext(ctx, `SELECT t.unit FROM requests r JOIN leave_types t ON t.id = r.type_id WHERE r.id = ?`, requestID).Scan(&unit); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT c.id, c.admin_id, u.name, c.corrected_at, c.reason, c.before_json, c.after_json
		FROM request_corrections c JOIN users u ON u.id = c.admin_id WHERE c.request_id = ? ORDER BY c.id`, requestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Correction
	for rows.Next() {
		var c Correction
		var before, after string
		if err := rows.Scan(&c.ID, &c.AdminID, &c.AdminName, &c.At, &c.Reason, &before, &after); err != nil {
			return nil, err
		}
		var b, a correctionSnapshot
		if err := json.Unmarshal([]byte(before), &b); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(after), &a); err != nil {
			return nil, err
		}
		// The unit is today's type unit: only the figure's wording uses it.
		c.Before, c.After = b.request(unit), a.request(unit)
		out = append(out, c)
	}
	return out, rows.Err()
}

// PreviewCorrect is Preview for the admin form correcting request id: quoted
// for the request's owner with the correction's relaxations and the request's
// own old version out of overlap and balance. The returned request is as it
// stands now, so the page can say "was N days, now M". An admin who cannot
// correct it gets ErrNotAllowed, or the own-request message.
func (s *Service) PreviewCorrect(ctx context.Context, admin users.User, id int64, in Input) (Quote, Request, error) {
	if !admin.Admin || admin.Disabled {
		return Quote{}, Request{}, ErrNotAllowed
	}
	r, err := s.Get(ctx, id)
	if err != nil {
		return Quote{}, Request{}, err
	}
	if admin.ID == r.UserID {
		return Quote{}, r, FieldErrors{"correct": MsgCorrectOwn}
	}
	if !CanCorrect(admin, r) {
		return Quote{}, r, ErrNotAllowed
	}
	if err := s.correctBlocked(ctx, s.db, r); err != nil {
		return Quote{}, r, err
	}
	owner, err := s.users.ByID(ctx, r.UserID)
	if err != nil {
		return Quote{}, r, err
	}
	if err := s.ensureYears(ctx, in); err != nil {
		return Quote{}, r, err
	}
	qt, err := s.quote(ctx, s.db, owner, in, correctOpts(r))
	return qt, r, err
}

func correctOpts(r Request) quoteOpts {
	return quoteOpts{correct: true, halvesAllowed: true, excludeID: r.ID, keepTypeID: r.TypeID}
}

// Correct lets an admin fix what someone submitted: the type, dates, half days
// and note of another person's request that has not been approved. Staff
// cannot apply for compassionate or unpaid leave, so they ask for something
// else with a note, and HR corrects it here. The request keeps its id, status,
// approver, special-permission flag and place in the queue; its fingerprint
// changes, so an approver reading the old page gets ErrChanged.
//
// What is relaxed is what quote relaxes for quoteOpts.correct (no coverage
// check, any balance, admin-only types). The days are recounted, since the
// type or the dates may have changed. A reason is required, kept with a
// before/after snapshot in request_corrections (every correction is kept), and
// sent to the person, and to the approver while it is pending.
//
// Judged again inside the transaction: the role, the request's state, no
// approval started (a pending request with a step) and no pending cancellation.
// A non-empty seen is the fingerprint the admin read; a different one is
// ErrChanged. Nobody corrects their own request: another admin does.
func (s *Service) Correct(ctx context.Context, admin users.User, id int64, in Input, reason, seen string) (Request, error) {
	if !admin.Admin || admin.Disabled {
		return Request{}, ErrNotAllowed
	}
	r, err := s.Get(ctx, id)
	if err != nil {
		return Request{}, err
	}
	if admin.ID == r.UserID {
		return Request{}, FieldErrors{"correct": MsgCorrectOwn}
	}
	if !CanCorrect(admin, r) {
		return Request{}, ErrNotAllowed
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return Request{}, FieldErrors{"reason": MsgCorrectReason}
	}
	owner, err := s.users.ByID(ctx, r.UserID)
	if err != nil {
		return Request{}, err
	}
	if err := s.ensureYears(ctx, in); err != nil {
		return Request{}, err
	}
	var out Request
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		old, err := s.get(ctx, tx, id)
		if err != nil {
			return err
		}
		if admin.ID == old.UserID {
			return FieldErrors{"correct": MsgCorrectOwn}
		}
		if !CanCorrect(admin, old) {
			return ErrNotAllowed
		}
		if seen != "" && seen != old.Fingerprint() {
			return ErrChanged
		}
		if err := s.correctBlocked(ctx, tx, old); err != nil {
			return err
		}
		qt, err := s.quote(ctx, tx, owner, in, correctOpts(old))
		if err != nil {
			return err
		}
		ranges, err := in.ranges()
		if err != nil {
			return err
		}
		if ranges, err = normaliseRanges(ranges); err != nil {
			return err
		}
		note := strings.TrimSpace(in.Note)
		if qt.Type.ID == old.TypeID && rangesString(ranges) == rangesString(old.Ranges) &&
			in.HalfStart == old.HalfStart && in.HalfEnd == old.HalfEnd && note == old.Note {
			return FieldErrors{"correct": MsgCorrectUnchanged}
		}
		// allow_negative: a request still pending is judged afresh, so it is
		// set only when this correction takes the balance below zero. One
		// waiting for HR or an HR-allowed draft keeps what it had, or gains it:
		// HR is the one correcting, and a draft must stay submittable.
		allowNegative := qt.Shortfall > 0
		if old.Status != StatusPending {
			allowNegative = allowNegative || old.AllowNegative
		}
		// The WHERE repeats every guard, so a decision or a withdrawal landing
		// first makes this a no-op rather than an overwrite.
		res, err := tx.ExecContext(ctx, `UPDATE requests SET type_id = ?, start_date = ?, end_date = ?, days_x100 = ?,
			half_start = ?, half_end = ?, cycle_start = ?, note = ?, allow_negative = ?
			WHERE id = ? AND user_id <> ? AND status = ? AND kind = 'leave'
			AND shutdown_part_id IS NULL AND recorded_by_id IS NULL
			AND (status <> 'pending' OR NOT EXISTS (SELECT 1 FROM request_steps WHERE request_id = requests.id))`,
			qt.Type.ID, ranges[0].Start, ranges[len(ranges)-1].End, qt.Days, in.HalfStart, in.HalfEnd,
			fmtDate(qt.CycleStart), note, allowNegative, id, admin.ID, old.Status)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotAllowed
		}
		if err := insertRanges(tx, id, ranges); err != nil {
			return err
		}
		if out, err = s.get(ctx, tx, id); err != nil {
			return err
		}
		before, err := json.Marshal(snapshotOf(old))
		if err != nil {
			return err
		}
		after, err := json.Marshal(snapshotOf(out))
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO request_corrections (request_id, admin_id, corrected_at, reason, before_json, after_json)
			VALUES (?, ?, ?, ?, ?, ?)`, id, admin.ID, stampNow(s.Now()), reason, string(before), string(after)); err != nil {
			return err
		}
		return s.emit(ctx, tx, Event{Kind: EventCorrected, Request: out, Before: &old, Actor: admin, Note: reason})
	})
	return out, err
}
