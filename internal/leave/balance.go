package leave

import (
	"context"
	"strings"
	"time"
)

// Balance is one leave type's position in one cycle.
type Balance struct {
	Type        Type
	CycleStart  string
	CycleEnd    string
	Allowance   *int // nil: unlimited
	Adjustments int
	Taken       int // approved
	Pending     int // awaiting a decision, and reserved so they cannot be spent twice
}

// Available is what is left to request; nil when the type is unlimited.
func (b Balance) Available() *int {
	if b.Allowance == nil {
		return nil
	}
	n := *b.Allowance + b.Adjustments - b.Taken - b.Pending
	return &n
}

func (s *Service) balance(ctx context.Context, q querier, userID int64, t Type, cycle time.Time) (Balance, error) {
	b := Balance{Type: t, Allowance: t.Allowance, CycleStart: fmtDate(cycle),
		CycleEnd: fmtDate(CycleEnd(cycle, t.CycleMonths))}
	err := q.QueryRowContext(ctx, `SELECT
		COALESCE((SELECT sum(days) FROM balance_adjustments WHERE user_id = ?1 AND type_id = ?2 AND cycle_start = ?3), 0),
		COALESCE((SELECT sum(days) FROM requests WHERE user_id = ?1 AND type_id = ?2 AND cycle_start = ?3
			AND kind = 'leave' AND status = 'approved'), 0),
		COALESCE((SELECT sum(days) FROM requests WHERE user_id = ?1 AND type_id = ?2 AND cycle_start = ?3
			AND kind = 'leave' AND status = 'pending'), 0)`,
		userID, t.ID, b.CycleStart).Scan(&b.Adjustments, &b.Taken, &b.Pending)
	return b, err
}

// Balances is every active type's balance for the cycle containing today.
func (s *Service) Balances(ctx context.Context, userID int64) ([]Balance, error) {
	types, err := s.Types(ctx, true)
	if err != nil {
		return nil, err
	}
	st, err := s.settings(ctx, s.db)
	if err != nil {
		return nil, err
	}
	today := s.today()
	out := make([]Balance, 0, len(types))
	for _, t := range types {
		b, err := s.balance(ctx, s.db, userID, t, CycleStart(today, t.CycleMonths, st.SickCycleAnchor))
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}

// Adjustment is an admin's correction to a balance: carry-over, a pro-rata
// first year, a correction.
type Adjustment struct {
	ID         int64
	UserID     int64
	TypeID     int64
	TypeName   string
	CycleStart string
	Days       int
	Reason     string
	ByName     string
	CreatedAt  string
}

func (s *Service) AddAdjustment(ctx context.Context, by int64, a Adjustment) error {
	errs := FieldErrors{}
	t, err := s.Type(ctx, a.TypeID)
	if err != nil {
		errs["type"] = "Choose a leave type."
	}
	st, serr := s.settings(ctx, s.db)
	if serr != nil {
		return serr
	}
	cycle, derr := ParseDate(a.CycleStart)
	if derr != nil {
		errs["cycle"] = "Choose a cycle."
	} else if err == nil && !CycleStart(cycle, t.CycleMonths, st.SickCycleAnchor).Equal(cycle) {
		errs["cycle"] = "That is not the start of a cycle for this leave type."
	}
	if a.Days == 0 {
		errs["days"] = "Enter a number of days, negative to subtract."
	}
	if strings.TrimSpace(a.Reason) == "" {
		errs["reason"] = "Say why, for whoever reads this later."
	}
	if len(errs) > 0 {
		return errs
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO balance_adjustments
		(user_id, type_id, cycle_start, days, reason, by_user_id, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		a.UserID, a.TypeID, a.CycleStart, a.Days, strings.TrimSpace(a.Reason), by, stampNow(s.Now()))
	return err
}

func (s *Service) Adjustments(ctx context.Context, userID int64) ([]Adjustment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT a.id, a.user_id, a.type_id, t.name, a.cycle_start, a.days,
		a.reason, u.name, a.created_at FROM balance_adjustments a
		JOIN leave_types t ON t.id = a.type_id JOIN users u ON u.id = a.by_user_id
		WHERE a.user_id = ? ORDER BY a.created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Adjustment
	for rows.Next() {
		var a Adjustment
		if err := rows.Scan(&a.ID, &a.UserID, &a.TypeID, &a.TypeName, &a.CycleStart, &a.Days, &a.Reason,
			&a.ByName, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
