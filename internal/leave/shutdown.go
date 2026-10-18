package leave

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/17xande-dev/leave/internal/users"
)

// A company shutdown is a period everyone is off. An admin defines it as
// parts, each charged to one leave type -- typically some days to annual leave
// and the rest to "Company closure", which deducts nothing. Every enabled
// person gets a pre-filled draft request per part, which they review and
// submit; from there it follows their normal approval chain, or is approved
// at once if the shutdown says so.

type ShutdownPart struct {
	ID       int64
	TypeID   int64
	TypeName string
	Start    string
	End      string
	Days     int
}

type Shutdown struct {
	ID          int64
	Name        string
	AutoApprove bool
	CreatedAt   string
	Parts       []ShutdownPart
}

// Start and End span every part.
func (sd Shutdown) Start() string {
	if len(sd.Parts) == 0 {
		return ""
	}
	return sd.Parts[0].Start
}

func (sd Shutdown) End() string {
	end := ""
	for _, p := range sd.Parts {
		if p.End > end {
			end = p.End
		}
	}
	return end
}

type PartInput struct {
	TypeID     int64
	Start, End string
}

type ShutdownInput struct {
	Name        string
	AutoApprove bool
	Parts       []PartInput
}

// CreateShutdown validates and saves a shutdown, then creates everyone's drafts.
func (s *Service) CreateShutdown(ctx context.Context, admin users.User, in ShutdownInput) (int64, error) {
	if !admin.Admin {
		return 0, ErrNotAllowed
	}
	in.Name = strings.TrimSpace(in.Name)
	errs := FieldErrors{}
	if in.Name == "" {
		errs["name"] = "Name the shutdown, e.g. December shutdown 2026."
	}
	type part struct {
		PartInput
		start, end time.Time
		t          Type
	}
	var parts []part
	for i, p := range in.Parts {
		if p.TypeID == 0 && p.Start == "" && p.End == "" {
			continue // an unused row of the form
		}
		key := fmt.Sprintf("part%d", i)
		t, err := s.Type(ctx, p.TypeID)
		start, err1 := ParseDate(p.Start)
		end, err2 := ParseDate(p.End)
		switch {
		case err != nil:
			errs[key] = "Choose a leave type."
		case err1 != nil || err2 != nil:
			errs[key] = "Enter both dates."
		case end.Before(start):
			errs[key] = "The last day cannot be before the first day."
		case end.Sub(start) > maxSpan*24*time.Hour:
			errs[key] = "That part is too long. Check the year."
		default:
			parts = append(parts, part{p, start, end, t})
		}
	}
	if len(parts) == 0 && len(errs) == 0 {
		errs["part0"] = "Add at least one part."
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].start.Before(parts[j].start) })
	for i := 1; i < len(parts); i++ {
		if !parts[i].start.After(parts[i-1].end) {
			errs["part0"] = "The parts overlap. Each day of the shutdown belongs to one part."
		}
	}
	if len(errs) > 0 {
		return 0, errs
	}
	for _, p := range parts {
		for y := p.start.Year(); y <= p.end.Year(); y++ {
			if err := s.EnsureHolidays(ctx, y); err != nil {
				return 0, err
			}
		}
		hol, err := s.holidaySet(ctx, s.db, p.start, p.end)
		if err != nil {
			return 0, err
		}
		if CountDays(p.start, p.end, p.t.Unit, hol) == 0 {
			return 0, FieldErrors{"part0": fmt.Sprintf("%s – %s is all weekends or public holidays.",
				fmtDate(p.start), fmtDate(p.end))}
		}
	}

	var id int64
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `INSERT INTO shutdowns (name, auto_approve, created_by_id, created_at)
			VALUES (?, ?, ?, ?)`, in.Name, in.AutoApprove, admin.ID, stampNow(s.Now()))
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
		for _, p := range parts {
			if _, err := tx.ExecContext(ctx, `INSERT INTO shutdown_parts (shutdown_id, type_id, start_date, end_date)
				VALUES (?, ?, ?, ?)`, id, p.TypeID, fmtDate(p.start), fmtDate(p.end)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	_, err = s.GenerateDrafts(ctx, id)
	return id, err
}

// GenerateDrafts creates the missing drafts for a shutdown: one per enabled
// person per part, skipping anyone who already has a live one for that part.
// Run again, it fills in only new starters (and anyone whose request was
// declined), so it is safe to press twice.
func (s *Service) GenerateDrafts(ctx context.Context, shutdownID int64) (int, error) {
	sd, err := s.Shutdown(ctx, shutdownID)
	if err != nil {
		return 0, err
	}
	people, err := s.users.Enabled(ctx)
	if err != nil {
		return 0, err
	}
	created := 0
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		for _, u := range people {
			var first *Request
			for _, p := range sd.Parts {
				if p.Days == 0 {
					continue // every day of it has since become a public holiday
				}
				t, err := scanType(tx.QueryRowContext(ctx, `SELECT `+typeCols+` FROM leave_types WHERE id = ?`, p.TypeID))
				if err != nil {
					return err
				}
				start, _ := ParseDate(p.Start)
				res, err := tx.ExecContext(ctx, `INSERT INTO requests (user_id, type_id, kind, start_date, end_date,
					days, cycle_start, status, shutdown_part_id, created_at)
					SELECT ?, ?, 'leave', ?, ?, ?, ?, 'draft', ?, ?
					WHERE NOT EXISTS (SELECT 1 FROM requests WHERE shutdown_part_id = ? AND user_id = ?
						AND status IN ('draft', 'pending', 'approved'))`,
					u.ID, p.TypeID, p.Start, p.End, p.Days,
					fmtDate(CycleStart(start, t.CycleMonths, s.cfg.SickCycleAnchor)), p.ID, stampNow(s.Now()),
					p.ID, u.ID)
				if err != nil {
					return err
				}
				if n, _ := res.RowsAffected(); n == 0 {
					continue
				}
				created++
				if first == nil {
					rid, _ := res.LastInsertId()
					r, err := s.get(ctx, tx, rid)
					if err != nil {
						return err
					}
					first = &r
				}
			}
			// One event per person, however many parts: one email asking
			// them to submit, not one per part.
			if first != nil {
				if err := s.hook(ctx, tx, Event{Kind: EventDraftsCreated, Request: *first, Actor: u}); err != nil {
					return err
				}
			}
		}
		return nil
	})
	return created, err
}

func (s *Service) Shutdown(ctx context.Context, id int64) (Shutdown, error) {
	sds, err := s.shutdowns(ctx, `WHERE sd.id = ?`, id)
	if err != nil {
		return Shutdown{}, err
	}
	if len(sds) == 0 {
		return Shutdown{}, ErrNotFound
	}
	return sds[0], nil
}

// Shutdowns lists every shutdown, latest first.
func (s *Service) Shutdowns(ctx context.Context) ([]Shutdown, error) {
	return s.shutdowns(ctx, ``)
}

func (s *Service) shutdowns(ctx context.Context, where string, args ...any) ([]Shutdown, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT sd.id, sd.name, sd.auto_approve, sd.created_at,
		p.id, p.type_id, t.name, t.unit, p.start_date, p.end_date
		FROM shutdowns sd JOIN shutdown_parts p ON p.shutdown_id = sd.id JOIN leave_types t ON t.id = p.type_id
		`+where+` ORDER BY sd.id DESC, p.start_date`, args...)
	if err != nil {
		return nil, err
	}
	var out []Shutdown
	var units []string
	for rows.Next() {
		var sd Shutdown
		var p ShutdownPart
		var unit string
		if err := rows.Scan(&sd.ID, &sd.Name, &sd.AutoApprove, &sd.CreatedAt,
			&p.ID, &p.TypeID, &p.TypeName, &unit, &p.Start, &p.End); err != nil {
			rows.Close()
			return nil, err
		}
		if len(out) == 0 || out[len(out)-1].ID != sd.ID {
			out = append(out, sd)
		}
		last := &out[len(out)-1]
		last.Parts = append(last.Parts, p)
		units = append(units, unit)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Day counts are computed rather than stored, so a holiday added after
	// the shutdown was made is reflected everywhere, the submit included.
	i := 0
	for si := range out {
		for pi := range out[si].Parts {
			p := &out[si].Parts[pi]
			start, _ := ParseDate(p.Start)
			end, _ := ParseDate(p.End)
			hol, err := s.holidaySet(ctx, s.db, start, end)
			if err != nil {
				return nil, err
			}
			p.Days = CountDays(start, end, units[i], hol)
			i++
		}
	}
	return out, nil
}

// ShutdownRequests lists everyone's requests for a shutdown, by person.
func (s *Service) ShutdownRequests(ctx context.Context, id int64) ([]Request, error) {
	return s.queryRequests(ctx, s.db, `WHERE sp.shutdown_id = ? ORDER BY u.name, r.start_date, r.id`, id)
}

// Drafts lists u's unsubmitted shutdown requests, soonest first.
func (s *Service) Drafts(ctx context.Context, userID int64) ([]Request, error) {
	return s.queryRequests(ctx, s.db, `WHERE r.user_id = ? AND r.status = 'draft' ORDER BY r.start_date`, userID)
}

// DeleteShutdown removes a shutdown that nobody has submitted anything for
// yet -- the way to fix a mistake in its dates. Once a request is pending or
// approved it is real leave and the shutdown stays.
func (s *Service) DeleteShutdown(ctx context.Context, admin users.User, id int64) error {
	if !admin.Admin {
		return ErrNotAllowed
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		var live int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM requests r JOIN shutdown_parts p ON p.id = r.shutdown_part_id
			WHERE p.shutdown_id = ? AND r.status <> 'draft'`, id).Scan(&live); err != nil {
			return err
		}
		if live > 0 {
			return ErrNotAllowed
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM requests WHERE shutdown_part_id IN
			(SELECT id FROM shutdown_parts WHERE shutdown_id = ?)`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM shutdown_parts WHERE shutdown_id = ?`, id); err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM shutdowns WHERE id = ?`, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotFound
		}
		return nil
	})
}

// SubmitDraft submits u's shutdown draft. The days are recounted (a holiday
// may have been added since) and checked for overlap with u's other leave, but
// not against the balance: the days are mandatory, so annual leave may go
// below zero. With auto-approve on, it is approved there and then.
func (s *Service) SubmitDraft(ctx context.Context, u users.User, id int64, note string) (Request, error) {
	d, err := s.Get(ctx, id)
	if err != nil {
		return Request{}, err
	}
	if d.UserID != u.ID || d.Status != StatusDraft {
		return Request{}, ErrNotAllowed
	}
	var auto bool
	if err := s.db.QueryRowContext(ctx, `SELECT sd.auto_approve FROM shutdowns sd
		JOIN shutdown_parts p ON p.shutdown_id = sd.id WHERE p.id = ?`, d.ShutdownPartID).Scan(&auto); err != nil {
		return Request{}, err
	}
	in := Input{TypeID: d.TypeID, Start: d.Start, End: d.End, Note: note}
	if err := s.ensureYears(ctx, in); err != nil {
		return Request{}, err
	}
	approver, err := s.ResolveApprover(ctx, u)
	if err != nil {
		return Request{}, err
	}
	var out Request
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		qt, err := s.quote(ctx, tx, u, in, quoteOpts{mandatory: true, excludeID: id})
		if err != nil {
			return err
		}
		status, decidedAt, decisionNote := StatusPending, any(nil), ""
		if auto {
			status, decidedAt, decisionNote = StatusApproved, stampNow(s.Now()), "Approved automatically: company shutdown."
		}
		res, err := tx.ExecContext(ctx, `UPDATE requests SET status = ?, days = ?, cycle_start = ?, note = ?,
			approver_id = ?, decided_at = ?, decision_note = ? WHERE id = ? AND status = 'draft'`,
			status, qt.Days, fmtDate(qt.CycleStart), strings.TrimSpace(note), nullID(approver),
			decidedAt, decisionNote, id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNotAllowed
		}
		if out, err = s.get(ctx, tx, id); err != nil {
			return err
		}
		kind := EventSubmitted
		if auto {
			kind = EventDecided
		}
		return s.hook(ctx, tx, Event{Kind: kind, Request: out, Actor: u})
	})
	if errors.Is(err, ErrNotFound) {
		return Request{}, ErrNotAllowed
	}
	return out, err
}
