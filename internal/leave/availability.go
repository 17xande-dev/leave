package leave

import (
	"context"
	"database/sql"
	"errors"

	"github.com/17xande-dev/leave/internal/users"
)

// AnnualCode is the code of the seeded annual leave type. The code, not the
// name, is what identifies it: it is unique and SaveType never changes it.
const AnnualCode = "annual"

// MaternityCode is the code of the maternity leave type, which is refused to
// anyone recorded as male.
const MaternityCode = "maternity"

// OfferedTo reports whether a form should list this type for u. It is only the
// picker's half of the rule; quote is the guard that actually refuses.
func (t Type) OfferedTo(u users.User) bool {
	return !maternityRefused(t, u)
}

// maternityRefused is the one statement of the rule: maternity leave is not
// for someone recorded as male. An unrecorded sex blocks nobody. quote applies
// it to every request as made, and decide again when one is approved, since
// the person may have been marked male after applying.
func maternityRefused(t Type, u users.User) bool {
	return t.Code == MaternityCode && u.Sex == "m"
}

// maternityMessage says why, and what to do. yours is true when the reader is
// the person themselves (applying, editing); otherwise it is an admin or
// approver acting on someone else's behalf.
func maternityMessage(u users.User, yours bool) string {
	if yours {
		return "Maternity leave is not available to you: you are recorded as male. If that is wrong, ask an admin to change it."
	}
	return "Maternity leave is not available to " + u.Name + ": they are recorded as male (an admin can change this on their admin page)."
}

// Availability is every person's available balance for one leave type in the
// current cycle, read in a single query so a list of people costs one query
// and not one per person.
type Availability struct {
	allowance *Days
	used      map[int64]Days // adjustments less taken and pending, for people who have any
	ok        bool
}

// For is what the person has left; nil when the type does not exist or is unlimited.
func (a Availability) For(userID int64) *Days {
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
		SELECT user_id, days_x100 AS delta FROM balance_adjustments WHERE type_id = ?1 AND cycle_start = ?2
		UNION ALL
		SELECT user_id, -days_x100 FROM requests WHERE type_id = ?1 AND cycle_start = ?2
			AND kind = 'leave' AND status IN ('approved', 'pending'))
		GROUP BY user_id`, t.ID, cycle)
	if err != nil {
		return Availability{}, err
	}
	defer rows.Close()
	a := Availability{allowance: t.Allowance, used: map[int64]Days{}, ok: true}
	for rows.Next() {
		var id int64
		var n Days
		if err := rows.Scan(&id, &n); err != nil {
			return Availability{}, err
		}
		a.used[id] = n
	}
	return a, rows.Err()
}
