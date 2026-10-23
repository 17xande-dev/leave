package leave

import (
	"context"
	"database/sql"
	"errors"
)

// AnnualCode is the code of the seeded annual leave type. The code, not the
// name, is what identifies it: it is unique and SaveType never changes it.
const AnnualCode = "annual"

// Availability is every person's available balance for one leave type in the
// current cycle, read in a single query so a list of people costs one query
// and not one per person.
type Availability struct {
	allowance *int
	used      map[int64]int // adjustments less taken and pending, for people who have any
	ok        bool
}

// For is what the person has left; nil when the type does not exist or is unlimited.
func (a Availability) For(userID int64) *int {
	if !a.ok || a.allowance == nil {
		return nil
	}
	n := *a.allowance + a.used[userID]
	return &n
}

// AvailabilityByCode reads the available balance of the type with this code,
// for everyone, in the cycle containing today. A missing type gives an
// Availability whose For is always nil.
func (s *Service) AvailabilityByCode(ctx context.Context, code string) (Availability, error) {
	t, err := scanType(s.db.QueryRowContext(ctx, `SELECT `+typeCols+` FROM leave_types WHERE code = ? AND active = 1`, code))
	if errors.Is(err, sql.ErrNoRows) || errors.Is(err, ErrNotFound) {
		return Availability{}, nil
	}
	if err != nil {
		return Availability{}, err
	}
	st, err := s.settings(ctx, s.db)
	if err != nil {
		return Availability{}, err
	}
	cycle := fmtDate(CycleStart(s.today(), t.CycleMonths, st.SickCycleAnchor))
	rows, err := s.db.QueryContext(ctx, `SELECT user_id, sum(delta) FROM (
		SELECT user_id, days AS delta FROM balance_adjustments WHERE type_id = ?1 AND cycle_start = ?2
		UNION ALL
		SELECT user_id, -days FROM requests WHERE type_id = ?1 AND cycle_start = ?2
			AND kind = 'leave' AND status IN ('approved', 'pending'))
		GROUP BY user_id`, t.ID, cycle)
	if err != nil {
		return Availability{}, err
	}
	defer rows.Close()
	a := Availability{allowance: t.Allowance, used: map[int64]int{}, ok: true}
	for rows.Next() {
		var id int64
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return Availability{}, err
		}
		a.used[id] = n
	}
	return a, rows.Err()
}
