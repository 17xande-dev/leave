package leave

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/17xande-dev/leave/internal/users"
)

// Department coverage: an admin caps how much of a department may be away on
// any one day, as a percentage. A request that would take a day over the cap
// is refused -- unless the person asks for special permission, which sends it
// up the approval chain instead (see Decide).

// CoverageRules returns every rule by department; "" is the default rule,
// used by a department with none of its own.
func (s *Service) CoverageRules(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT department, max_percent FROM coverage_rules`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var d string
		var p int
		if err := rows.Scan(&d, &p); err != nil {
			return nil, err
		}
		out[d] = p
	}
	return out, rows.Err()
}

// SetCoverageRule sets the limit for a department ("" is the default rule).
// 0 removes the rule.
func (s *Service) SetCoverageRule(ctx context.Context, department string, maxPercent int) error {
	switch {
	case maxPercent == 0:
		_, err := s.db.ExecContext(ctx, `DELETE FROM coverage_rules WHERE department = ?`, department)
		return err
	case maxPercent < 0 || maxPercent > 100:
		return FieldErrors{"percent": "Enter a whole number from 1 to 100, or leave it empty for no limit."}
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO coverage_rules (department, max_percent) VALUES (?, ?)
		ON CONFLICT(department) DO UPDATE SET max_percent = excluded.max_percent`, department, maxPercent)
	return err
}

// Department is one department of the directory and how many enabled people it has.
type Department struct {
	Name string
	Size int
}

// Departments lists the distinct non-empty departments of enabled users.
func (s *Service) Departments(ctx context.Context) ([]Department, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT department, count(*) FROM users
		WHERE disabled = 0 AND department <> '' GROUP BY department ORDER BY department`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Department
	for rows.Next() {
		var d Department
		if err := rows.Scan(&d.Name, &d.Size); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Coverage describes the worst day of a request that is over its department's
// limit.
type Coverage struct {
	Department string
	MaxPercent int
	Size       int
	// Away counts the requester.
	Away int
	// Day is the worst day, YYYY-MM-DD.
	Day string
}

// Percent is the share of the department away on the worst day, rounded.
func (c Coverage) Percent() int { return (c.Away*100 + c.Size/2) / c.Size }

// Sentence is the warning shown on the apply page, the request page, the
// approvals queue and the escalation email.
func (c Coverage) Sentence() string {
	day := c.Day
	if t, err := ParseDate(c.Day); err == nil {
		day = t.Format("Mon 2 Jan")
	}
	return fmt.Sprintf("%d%% of %s (%d of %d) would be away on %s. The limit is %d%%.",
		c.Percent(), c.Department, c.Away, c.Size, day, c.MaxPercent)
}

// coverage checks a request against its department's limit. It returns nil
// unless the request is over the limit and t can be blocked by it (a "count"
// type is never refused, though it still counts for everyone else's checks).
// excludeID is a request of u's own to leave out, so a draft or pending
// request being re-evaluated does not collide with itself.
func (s *Service) coverage(ctx context.Context, q querier, u users.User, t Type, start, end time.Time,
	holidays map[time.Time]bool, excludeID int64) (*Coverage, error) {
	if t.CoverageRule != CoverageLimit || u.Department == "" {
		return nil, nil
	}
	// The department's own rule, else the default; none means no limit.
	var limit int
	err := q.QueryRowContext(ctx, `SELECT max_percent FROM coverage_rules WHERE department IN (?, '')
		ORDER BY department = '' LIMIT 1`, u.Department).Scan(&limit)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var size int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE department = ? AND disabled = 0`,
		u.Department).Scan(&size); err != nil {
		return nil, err
	}
	if size == 0 {
		return nil, nil
	}

	// Others' leave touching the range. A drafted request is not away yet.
	rows, err := q.QueryContext(ctx, `SELECT r.user_id, r.start_date, r.end_date FROM requests r
		JOIN users x ON x.id = r.user_id
		WHERE x.department = ? AND x.disabled = 0 AND r.user_id <> ? AND r.id <> ? AND r.kind = 'leave'
		AND r.status IN ('pending', 'approved', 'awaiting_hr') AND r.start_date <= ? AND r.end_date >= ?`,
		u.Department, u.ID, excludeID, fmtDate(end), fmtDate(start))
	if err != nil {
		return nil, err
	}
	type span struct {
		user       int64
		start, end string
	}
	var spans []span
	for rows.Next() {
		var sp span
		if err := rows.Scan(&sp.user, &sp.start, &sp.end); err != nil {
			rows.Close()
			return nil, err
		}
		spans = append(spans, sp)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// A weekend off is taken on days the week does not work, so for that unit
	// every day counts; otherwise only days someone would actually be missed.
	worst, worstDay := -1, ""
	for d := start; !d.After(end); d = d.AddDate(0, 0, 1) {
		if t.Unit != UnitWeekend && !s.cfg.Week.Working(d, holidays) {
			continue
		}
		ds := fmtDate(d)
		away := map[int64]bool{}
		for _, sp := range spans {
			if sp.start <= ds && sp.end >= ds {
				away[sp.user] = true
			}
		}
		if len(away) > worst {
			worst, worstDay = len(away), ds
		}
	}
	// "More than", not "at least": one of two people at 50% is allowed. And
	// the first person away is always allowed, whatever the maths: otherwise
	// someone alone in their department (1 of 1 is 100%) could never take
	// leave without escalating it to the top.
	if worst <= 0 || (worst+1)*100 <= limit*size {
		return nil, nil
	}
	return &Coverage{Department: u.Department, MaxPercent: limit, Size: size, Away: worst + 1, Day: worstDay}, nil
}

// CoverageFor recomputes the coverage warning for a request as the department
// stands now, leaving the request itself out. Approvers see this rather than
// what was true at submission: others may have been approved since.
func (s *Service) CoverageFor(ctx context.Context, r Request) (*Coverage, error) {
	return s.coverageFor(ctx, s.db, r)
}

func (s *Service) coverageFor(ctx context.Context, q querier, r Request) (*Coverage, error) {
	if r.Kind != KindLeave {
		return nil, nil
	}
	start, err1 := ParseDate(r.Start)
	end, err2 := ParseDate(r.End)
	if err1 != nil || err2 != nil {
		return nil, nil
	}
	t, err := scanType(q.QueryRowContext(ctx, `SELECT `+typeCols+` FROM leave_types WHERE id = ?`, r.TypeID))
	if err != nil {
		return nil, err
	}
	u := users.User{ID: r.UserID}
	if err := q.QueryRowContext(ctx, `SELECT department FROM users WHERE id = ?`, r.UserID).Scan(&u.Department); err != nil {
		return nil, err
	}
	holidays, err := s.holidaySet(ctx, q, start, end)
	if err != nil {
		return nil, err
	}
	return s.coverage(ctx, q, u, t, start, end, holidays, r.ID)
}
