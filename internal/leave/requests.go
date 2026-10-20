package leave

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/17xande-dev/leave/internal/users"
)

const (
	KindLeave        = "leave"
	KindCancellation = "cancellation"

	StatusPending   = "pending"
	StatusApproved  = "approved"
	StatusDeclined  = "declined"
	StatusWithdrawn = "withdrawn"
	StatusCancelled = "cancelled"
)

// Request is one row of requests with the names a page needs joined in.
type Request struct {
	ID              int64
	UserID          int64
	TypeID          int64
	Kind            string
	CancelsID       int64 // 0 for a leave request
	Start, End      string
	Days            int
	CycleStart      string
	Note            string
	Status          string
	ApproverID      int64 // 0: the admins' queue
	DecidedByID     int64
	DecidedAt       string
	DecisionNote    string
	CalendarEventID string
	CreatedAt       string

	UserName     string
	UserEmail    string
	Department   string
	TypeName     string
	TypeUnit     string
	ApproverName string
	DecidedBy    string
}

func (r Request) IsCancellation() bool { return r.Kind == KindCancellation }

const requestSelect = `SELECT r.id, r.user_id, r.type_id, r.kind, COALESCE(r.cancels_request_id, 0),
	r.start_date, r.end_date, r.days, r.cycle_start, r.note, r.status, COALESCE(r.approver_id, 0),
	COALESCE(r.decided_by_id, 0), COALESCE(r.decided_at, ''), r.decision_note, r.calendar_event_id, r.created_at,
	u.name, u.email, u.department, t.name, t.unit, COALESCE(a.name, ''), COALESCE(dby.name, '')
	FROM requests r
	JOIN users u ON u.id = r.user_id
	JOIN leave_types t ON t.id = r.type_id
	LEFT JOIN users a ON a.id = r.approver_id
	LEFT JOIN users dby ON dby.id = r.decided_by_id`

func scanRequest(row interface{ Scan(...any) error }) (Request, error) {
	var r Request
	err := row.Scan(&r.ID, &r.UserID, &r.TypeID, &r.Kind, &r.CancelsID, &r.Start, &r.End, &r.Days, &r.CycleStart,
		&r.Note, &r.Status, &r.ApproverID, &r.DecidedByID, &r.DecidedAt, &r.DecisionNote, &r.CalendarEventID,
		&r.CreatedAt, &r.UserName, &r.UserEmail, &r.Department, &r.TypeName, &r.TypeUnit, &r.ApproverName, &r.DecidedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return Request{}, ErrNotFound
	}
	return r, err
}

func (s *Service) queryRequests(ctx context.Context, q querier, where string, args ...any) ([]Request, error) {
	rows, err := q.QueryContext(ctx, requestSelect+" "+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Request
	for rows.Next() {
		r, err := scanRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Service) get(ctx context.Context, q querier, id int64) (Request, error) {
	return scanRequest(q.QueryRowContext(ctx, requestSelect+` WHERE r.id = ?`, id))
}

func (s *Service) Get(ctx context.Context, id int64) (Request, error) { return s.get(ctx, s.db, id) }

// CanView reports whether someone may see a request: its owner, whoever it is
// or was routed to, whoever decided it, and admins.
func CanView(u users.User, r Request) bool {
	return u.Admin || u.ID == r.UserID || u.ID == r.ApproverID || u.ID == r.DecidedByID
}

// CanDecide reports whether u may approve or decline r now.
func CanDecide(u users.User, r Request) bool {
	if r.Status != StatusPending || u.ID == r.UserID || u.Disabled {
		return false
	}
	if r.ApproverID == 0 {
		return u.Admin
	}
	return u.ID == r.ApproverID
}

// ForUser lists someone's own requests, newest first.
func (s *Service) ForUser(ctx context.Context, userID int64) ([]Request, error) {
	return s.queryRequests(ctx, s.db, `WHERE r.user_id = ? ORDER BY r.start_date DESC, r.id DESC`, userID)
}

// Queue lists the pending requests u can decide, oldest first.
func (s *Service) Queue(ctx context.Context, u users.User) ([]Request, error) {
	return s.queryRequests(ctx, s.db, `WHERE r.status = 'pending' AND r.user_id <> ?
		AND (r.approver_id = ? OR (r.approver_id IS NULL AND ? = 1))
		ORDER BY r.created_at, r.id`, u.ID, u.ID, u.Admin)
}

// PendingCount is len(Queue) without the rows, for the navigation badge.
func (s *Service) PendingCount(ctx context.Context, u users.User) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM requests r WHERE r.status = 'pending'
		AND r.user_id <> ? AND (r.approver_id = ? OR (r.approver_id IS NULL AND ? = 1))`,
		u.ID, u.ID, u.Admin).Scan(&n)
	return n, err
}

// Team lists current and upcoming leave (approved or pending) of the people
// who report to u -- by directory manager or by an approver override.
func (s *Service) Team(ctx context.Context, u users.User) ([]Request, error) {
	return s.queryRequests(ctx, s.db, `WHERE r.kind = 'leave' AND r.status IN ('pending', 'approved')
		AND r.end_date >= ?
		AND r.user_id IN (
			SELECT id FROM users WHERE manager_dn = ? AND manager_dn <> '' AND id NOT IN (SELECT user_id FROM approver_overrides)
			UNION SELECT user_id FROM approver_overrides WHERE approver_id = ?)
		ORDER BY r.start_date, u.name`, fmtDate(s.today()), u.DN, u.ID)
}

// Filter narrows the admin's list of all requests.
type Filter struct {
	Status string
	From   string // requests ending on or after
	To     string // requests starting on or before
	UserID int64
}

func (s *Service) All(ctx context.Context, f Filter) ([]Request, error) {
	var where []string
	var args []any
	if f.Status != "" {
		where, args = append(where, "r.status = ?"), append(args, f.Status)
	}
	if f.From != "" {
		where, args = append(where, "r.end_date >= ?"), append(args, f.From)
	}
	if f.To != "" {
		where, args = append(where, "r.start_date <= ?"), append(args, f.To)
	}
	if f.UserID != 0 {
		where, args = append(where, "r.user_id = ?"), append(args, f.UserID)
	}
	q := ""
	if len(where) > 0 {
		q = "WHERE " + strings.Join(where, " AND ")
	}
	return s.queryRequests(ctx, s.db, q+" ORDER BY r.start_date DESC, r.id DESC LIMIT 2000", args...)
}

// Input is what the apply form submits.
type Input struct {
	TypeID int64
	Start  string
	End    string
	Note   string
}

// Quote is what a request would cost, for the form's live preview and for
// Submit's own checks.
type Quote struct {
	Type       Type
	Start, End time.Time
	Days       int
	CycleStart time.Time
	Balance    Balance
	// After is the balance left if this request is approved; nil when unlimited.
	After *int
}

// maxSpan keeps a typo ("2062" for "2026") from becoming a request spanning
// decades. Maternity leave, the longest, is four months.
const maxSpan = 190

// Preview validates a request without saving it.
func (s *Service) Preview(ctx context.Context, u users.User, in Input) (Quote, error) {
	if err := s.ensureYears(ctx, in); err != nil {
		return Quote{}, err
	}
	return s.quote(ctx, s.db, u, in)
}

func (s *Service) ensureYears(ctx context.Context, in Input) error {
	start, err1 := ParseDate(in.Start)
	end, err2 := ParseDate(in.End)
	if err1 != nil || err2 != nil || end.Before(start) || end.Sub(start) > maxSpan*24*time.Hour {
		return nil // quote reports the problem
	}
	for y := start.Year(); y <= end.Year(); y++ {
		if err := s.EnsureHolidays(ctx, y); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) quote(ctx context.Context, q querier, u users.User, in Input) (Quote, error) {
	errs := FieldErrors{}
	var qt Quote

	t, err := scanType(q.QueryRowContext(ctx, `SELECT `+typeCols+` FROM leave_types WHERE id = ?`, in.TypeID))
	switch {
	case errors.Is(err, ErrNotFound) || (err == nil && !t.Active):
		errs["type"] = "Choose a type of leave."
	case err != nil:
		return Quote{}, err
	}
	qt.Type = t

	start, err1 := ParseDate(in.Start)
	end, err2 := ParseDate(in.End)
	if err1 != nil {
		errs["start"] = "Enter the first day of leave."
	}
	if err2 != nil {
		errs["end"] = "Enter the last day of leave."
	}
	if len(errs) > 0 {
		return qt, errs
	}
	qt.Start, qt.End = start, end

	today := s.today()
	switch {
	case end.Before(start):
		errs["end"] = "The last day cannot be before the first day."
	case end.Sub(start) > maxSpan*24*time.Hour:
		errs["end"] = fmt.Sprintf("A single request can cover at most %d days. Check the year.", maxSpan)
	case t.Backdate && start.Before(today.AddDate(0, 0, -s.cfg.SickBackdateDays)):
		errs["start"] = fmt.Sprintf("%s can be applied for up to %d days after the fact.", t.Name, s.cfg.SickBackdateDays)
	case !t.Backdate && start.Before(today):
		errs["start"] = "Leave cannot start in the past."
	}
	if t.RequiresNote && strings.TrimSpace(in.Note) == "" {
		errs["note"] = "Please add a short note for " + strings.ToLower(t.Name) + "."
	}
	if len(errs) > 0 {
		return qt, errs
	}

	holidays, err := s.holidaySet(ctx, q, start, end)
	if err != nil {
		return Quote{}, err
	}
	qt.Days = CountDays(start, end, t.Unit, holidays)
	if qt.Days == 0 {
		errs["end"] = "Those dates are all weekends or public holidays, so there is nothing to take."
		return qt, errs
	}

	var overlap int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM requests WHERE user_id = ? AND kind = 'leave'
		AND status IN ('pending', 'approved') AND start_date <= ? AND end_date >= ?`,
		u.ID, fmtDate(end), fmtDate(start)).Scan(&overlap); err != nil {
		return Quote{}, err
	}
	if overlap > 0 {
		errs["start"] = "You already have leave booked or waiting for approval on some of these days."
	}

	// Days count against the cycle the leave starts in. A request crossing
	// the year end is charged to the old year in full -- simpler to explain
	// than a split, and the admin can adjust if it matters.
	qt.CycleStart = CycleStart(start, t.CycleMonths, s.cfg.SickCycleAnchor)
	if qt.Balance, err = s.balance(ctx, q, u.ID, t, qt.CycleStart); err != nil {
		return Quote{}, err
	}
	if avail := qt.Balance.Available(); avail != nil {
		after := *avail - qt.Days
		qt.After = &after
		if after < 0 {
			errs["end"] = fmt.Sprintf("This is %d %s, but you have %d left for this cycle.",
				qt.Days, plural(qt.Days, "day", "days"), *avail)
		}
	}
	if len(errs) > 0 {
		return qt, errs
	}
	return qt, nil
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// Submit validates and saves a leave request, routed to the requester's approver.
func (s *Service) Submit(ctx context.Context, u users.User, in Input) (Request, error) {
	if err := s.ensureYears(ctx, in); err != nil {
		return Request{}, err
	}
	approver, err := s.ResolveApprover(ctx, u)
	if err != nil {
		return Request{}, err
	}
	var out Request
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		// Re-quoted inside the transaction: the overlap and balance checks
		// must see the same state the insert commits against.
		qt, err := s.quote(ctx, tx, u, in)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO requests (user_id, type_id, kind, start_date, end_date, days,
			cycle_start, note, status, approver_id, created_at) VALUES (?, ?, 'leave', ?, ?, ?, ?, ?, 'pending', ?, ?)`,
			u.ID, qt.Type.ID, fmtDate(qt.Start), fmtDate(qt.End), qt.Days, fmtDate(qt.CycleStart),
			strings.TrimSpace(in.Note), nullID(approver), stampNow(s.Now()))
		if err != nil {
			return err
		}
		id, _ := res.LastInsertId()
		if out, err = s.get(ctx, tx, id); err != nil {
			return err
		}
		return s.hook(ctx, tx, Event{Kind: EventSubmitted, Request: out, Actor: u})
	})
	return out, err
}

// nullID stores 0 as NULL.
func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}
