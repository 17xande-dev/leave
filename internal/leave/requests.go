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

	StatusPending  = "pending"
	StatusApproved = "approved"
	StatusDraft    = "draft"
	// StatusAwaitingHR: asked to go below zero; an admin must allow it first.
	StatusAwaitingHR = "awaiting_hr"
	StatusDeclined   = "declined"
	StatusWithdrawn  = "withdrawn"
	StatusCancelled  = "cancelled"
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
	// ShutdownPartID is set on a request generated for a company shutdown.
	ShutdownPartID int64
	// Escalate: over a department coverage limit, so every level up to the
	// final approver must approve. AllowNegative: HR let it go below zero.
	Escalate      bool
	AllowNegative bool

	UserName     string
	UserEmail    string
	Department   string
	TypeName     string
	TypeUnit     string
	ApproverName string
	DecidedBy    string
	ShutdownName string
}

func (r Request) IsCancellation() bool { return r.Kind == KindCancellation }

// IsShutdown reports whether r was generated for a company shutdown: its type
// and dates are fixed and it cannot be withdrawn or cancelled.
func (r Request) IsShutdown() bool { return r.ShutdownPartID != 0 }

const requestSelect = `SELECT r.id, r.user_id, r.type_id, r.kind, COALESCE(r.cancels_request_id, 0),
	r.start_date, r.end_date, r.days, r.cycle_start, r.note, r.status, COALESCE(r.approver_id, 0),
	COALESCE(r.decided_by_id, 0), COALESCE(r.decided_at, ''), r.decision_note, r.calendar_event_id, r.created_at,
	COALESCE(r.shutdown_part_id, 0), r.escalate, r.allow_negative,
	u.name, u.email, u.department, t.name, t.unit, COALESCE(a.name, ''), COALESCE(dby.name, ''),
	COALESCE(sd.name, '')
	FROM requests r
	JOIN users u ON u.id = r.user_id
	JOIN leave_types t ON t.id = r.type_id
	LEFT JOIN users a ON a.id = r.approver_id
	LEFT JOIN users dby ON dby.id = r.decided_by_id
	LEFT JOIN shutdown_parts sp ON sp.id = r.shutdown_part_id
	LEFT JOIN shutdowns sd ON sd.id = sp.shutdown_id`

func scanRequest(row interface{ Scan(...any) error }) (Request, error) {
	var r Request
	err := row.Scan(&r.ID, &r.UserID, &r.TypeID, &r.Kind, &r.CancelsID, &r.Start, &r.End, &r.Days, &r.CycleStart,
		&r.Note, &r.Status, &r.ApproverID, &r.DecidedByID, &r.DecidedAt, &r.DecisionNote, &r.CalendarEventID,
		&r.CreatedAt, &r.ShutdownPartID, &r.Escalate, &r.AllowNegative, &r.UserName, &r.UserEmail, &r.Department, &r.TypeName, &r.TypeUnit,
		&r.ApproverName, &r.DecidedBy, &r.ShutdownName)
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

// CanWithdraw reports whether u may take r back: their own request, before
// anyone has decided it. That includes one waiting for HR and a draft HR has
// allowed -- both are theirs to drop -- but not a shutdown draft, whose days
// are mandatory.
func CanWithdraw(u users.User, r Request) bool {
	if r.UserID != u.ID || r.IsShutdown() {
		return false
	}
	switch r.Status {
	case StatusPending, StatusAwaitingHR, StatusDraft:
		return true
	}
	return false
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
	// Override asks for special permission: a request over a department's
	// coverage limit (department or team) is saved anyway and goes up the whole approval chain.
	Override bool
	// Negative asks HR to allow a request that takes the balance below zero.
	Negative bool
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
	// Shortfall is how many days below zero the request would take the
	// balance, whether or not that is allowed; 0 when it fits.
	Shortfall int
	// Coverage has one entry for each limit (its department's, its teams')
	// the request is over, whether or not that is allowed; empty when none.
	Coverage []Coverage
}

// Offers reads what a refused quote leaves open to the person: canOverride is
// special permission, offered when the request is over any coverage limit and
// nothing else stands in its way but the balance; canNegative is asking HR to
// go below zero, offered when the balance is the only thing wrong (a coverage
// limit does not matter until the request is submitted). fe is the quote's own
// errors; the "end" error is the balance one only when the quote carries a
// shortfall, because every earlier "end" error returns before the balance is
// worked out. The apply form and the MCP preview both use this, so the two
// cannot disagree about which buttons exist.
func Offers(qt Quote, fe FieldErrors) (canOverride, canNegative bool) {
	short := fe["end"] != "" && qt.Shortfall > 0
	other := 0
	for k := range fe {
		if k != "coverage" && !(k == "end" && short) {
			other++
		}
	}
	return fe["coverage"] != "" && other == 0, short && other == 0
}

// maxSpan keeps a typo ("2062" for "2026") from becoming a request spanning
// decades. Maternity leave, the longest, is four months.
const maxSpan = 190

// Preview validates a request without saving it.
func (s *Service) Preview(ctx context.Context, u users.User, in Input) (Quote, error) {
	if err := s.ensureYears(ctx, in); err != nil {
		return Quote{}, err
	}
	return s.quote(ctx, s.db, u, in, quoteOpts{})
}

// quoteOpts relaxes quote for a shutdown request. The days are mandatory, so
// the type may be one not offered on the apply form (an unlimited "company
// closure"), the dates may already have passed by the time someone submits,
// and the balance may go below zero. The overlap check still applies --
// against everything except the draft being submitted. A mandatory request is
// also exempt from coverage limits.
//
// override and allowNegative relax one rule each for an ordinary request:
// special permission for a department limit, and HR's permission to go below
// zero. Both still report what they waived (Quote.Coverage, Quote.Shortfall).
type quoteOpts struct {
	mandatory     bool
	override      bool
	allowNegative bool
	excludeID     int64
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

func (s *Service) quote(ctx context.Context, q querier, u users.User, in Input, opt quoteOpts) (Quote, error) {
	errs := FieldErrors{}
	var qt Quote

	st, err := s.settings(ctx, q)
	if err != nil {
		return Quote{}, err
	}
	t, err := scanType(q.QueryRowContext(ctx, `SELECT `+typeCols+` FROM leave_types WHERE id = ?`, in.TypeID))
	switch {
	case errors.Is(err, ErrNotFound) || (err == nil && !t.Active && !opt.mandatory):
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
	case opt.mandatory:
	case start.Before(today.AddDate(0, 0, -t.BackdateDays)):
		if t.BackdateDays == 0 {
			errs["start"] = "Leave cannot start in the past."
		} else {
			errs["start"] = fmt.Sprintf("%s can start at most %d days ago (no earlier than %s).",
				t.Name, t.BackdateDays, today.AddDate(0, 0, -t.BackdateDays).Format("Mon 2 Jan 2006"))
		}
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
	if t.Unit == UnitWeekend {
		if msg := weekendShape(start, end, u.LongWeekend); msg != "" {
			errs["start"] = msg
			return qt, errs
		}
		qt.Days = 1
	} else {
		qt.Days = CountDays(start, end, t.Unit, holidays, st.Week)
		if qt.Days == 0 {
			errs["end"] = "Those dates are all days off or public holidays, so there is nothing to take."
			return qt, errs
		}
		if msg := restrictedDayRule(st, t, start, end, qt.Days, holidays); msg != "" && !opt.mandatory {
			errs["start"] = msg
			return qt, errs
		}
	}

	// Drafts count here although they reserve no balance: booking annual leave
	// over days a shutdown already covers would charge the same days twice.
	var overlap int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM requests WHERE user_id = ? AND kind = 'leave'
		AND status IN ('draft', 'awaiting_hr', 'pending', 'approved') AND start_date <= ? AND end_date >= ? AND id <> ?`,
		u.ID, fmtDate(end), fmtDate(start), opt.excludeID).Scan(&overlap); err != nil {
		return Quote{}, err
	}
	if overlap > 0 {
		errs["start"] = "You already have leave booked, waiting for approval or waiting for you to submit on some of these days."
	}

	if !opt.mandatory {
		if qt.Coverage, err = s.coverage(ctx, q, st.Week, u, t, start, end, holidays, opt.excludeID); err != nil {
			return Quote{}, err
		}
		if len(qt.Coverage) > 0 && !opt.override {
			errs["coverage"] = Sentences(qt.Coverage) + " You can ask for special permission."
		}
	}

	// Days count against the cycle the leave starts in. A request crossing
	// the year end is charged to the old year in full -- simpler to explain
	// than a split, and the admin can adjust if it matters.
	qt.CycleStart = CycleStart(start, t.CycleMonths, st.SickCycleAnchor)
	if qt.Balance, err = s.balance(ctx, q, u.ID, t, qt.CycleStart); err != nil {
		return Quote{}, err
	}
	if avail := qt.Balance.Available(); avail != nil {
		after := *avail - qt.Days
		qt.After = &after
		if after < 0 {
			qt.Shortfall = -after
		}
		if after < 0 && !opt.mandatory && !opt.allowNegative {
			errs["end"] = fmt.Sprintf("This is %d %s, but you have %d left for this cycle.",
				qt.Days, plural(qt.Days, "day", "days"), *avail)
		}
	}
	if len(errs) > 0 {
		return qt, errs
	}
	return qt, nil
}

// weekendShape checks a weekend-off request covers exactly one weekend:
// Saturday to Sunday, or for someone on long weekends Friday to Sunday.
func weekendShape(start, end time.Time, long bool) string {
	sat := start.Weekday() == time.Saturday && end.Equal(start.AddDate(0, 0, 1))
	fri := start.Weekday() == time.Friday && end.Equal(start.AddDate(0, 0, 2))
	switch {
	case sat || (long && fri):
		return ""
	case long:
		return "A weekend off runs from a Friday (or Saturday) to the Sunday straight after it."
	}
	return "A weekend off runs from a Saturday to the Sunday straight after it."
}

// restrictedDayRule enforces the restricted day (Admin -> Settings): a working
// day that may not be taken off by itself. Leave including it must cover at
// least RestrictedMinDays working days, counting it; a type marked "never"
// may not include it at all; a weekend off always may. It only bites when the
// day is actually worked -- a public holiday on that day is free anyway.
func restrictedDayRule(st Settings, t Type, start, end time.Time, days int, holidays map[time.Time]bool) string {
	rd := st.RestrictedDay
	if rd == nil || t.Restricted == RestrictAny {
		return ""
	}
	includes := false
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		if d.Weekday() == *rd && st.Week.Working(d, holidays) {
			includes = true
			break
		}
	}
	if !includes {
		return ""
	}
	day := rd.String()
	if t.Restricted == RestrictNever {
		return fmt.Sprintf("%s cannot include a %s. To have a %s off, use a weekend off.", t.Name, day, day)
	}
	if days < st.RestrictedMinDays {
		return fmt.Sprintf("Leave that includes a %s must cover at least %d working days (this is %d). "+
			"To have a %s off on its own, use a weekend off.", day, st.RestrictedMinDays, days, day)
	}
	return ""
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// Submit validates and saves a leave request, routed to the requester's approver.
//
// With in.Override a request over a department limit is saved anyway, marked
// escalate so it needs every level of approval; one that is not over the limit
// is saved as an ordinary request, so asking for permission nobody needed
// costs nothing. With in.Negative a request that would take the balance below
// zero is saved awaiting HR instead, and an admin decides whether to allow it.
// That stage does not enforce coverage: the limit is checked when the person
// submits what HR allowed, where the override is on offer again.
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
		qt, err := s.quote(ctx, tx, u, in, quoteOpts{
			override: in.Override || in.Negative, allowNegative: in.Negative})
		if err != nil {
			return err
		}
		if in.Negative && qt.Shortfall > 0 {
			return s.saveAwaitingHR(ctx, tx, u, in, qt, &out)
		}
		if len(qt.Coverage) > 0 && !in.Override {
			// Negative relaxed coverage too, for a request that turned out
			// not to need HR; it may not slip past the limit that way.
			return FieldErrors{"coverage": Sentences(qt.Coverage) + " You can ask for special permission."}
		}
		escalate := in.Override && len(qt.Coverage) > 0
		res, err := tx.ExecContext(ctx, `INSERT INTO requests (user_id, type_id, kind, start_date, end_date, days,
			cycle_start, note, status, approver_id, escalate, created_at)
			VALUES (?, ?, 'leave', ?, ?, ?, ?, ?, 'pending', ?, ?, ?)`,
			u.ID, qt.Type.ID, fmtDate(qt.Start), fmtDate(qt.End), qt.Days, fmtDate(qt.CycleStart),
			strings.TrimSpace(in.Note), nullID(approver), escalate, stampNow(s.Now()))
		if err != nil {
			return err
		}
		id, _ := res.LastInsertId()
		if out, err = s.get(ctx, tx, id); err != nil {
			return err
		}
		return s.emit(ctx, tx, Event{Kind: EventSubmitted, Request: out, Actor: u})
	})
	return out, err
}

// saveAwaitingHR saves a request that needs HR's permission to go below zero.
// It has no approver: the admins decide it, and it reserves nothing until
// it comes back as a draft and is submitted.
func (s *Service) saveAwaitingHR(ctx context.Context, tx *sql.Tx, u users.User, in Input, qt Quote, out *Request) error {
	if strings.TrimSpace(in.Note) == "" {
		return FieldErrors{"note": "Say why you need to go below zero, so HR can decide."}
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO requests (user_id, type_id, kind, start_date, end_date, days,
		cycle_start, note, status, created_at) VALUES (?, ?, 'leave', ?, ?, ?, ?, ?, 'awaiting_hr', ?)`,
		u.ID, qt.Type.ID, fmtDate(qt.Start), fmtDate(qt.End), qt.Days, fmtDate(qt.CycleStart),
		strings.TrimSpace(in.Note), stampNow(s.Now()))
	if err != nil {
		return err
	}
	id, _ := res.LastInsertId()
	if *out, err = s.get(ctx, tx, id); err != nil {
		return err
	}
	return s.hook(ctx, tx, Event{Kind: EventNegativeRequested, Request: *out, Actor: u, Shortfall: qt.Shortfall})
}

// nullID stores 0 as NULL.
func nullID(id int64) any {
	if id == 0 {
		return nil
	}
	return id
}
