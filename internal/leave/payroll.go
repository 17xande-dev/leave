package leave

import (
	"context"
	"sort"
	"strings"
	"time"

	"github.com/17xande-dev/leave/internal/users"
)

// PayrollReport is the current position for payroll, including types whose
// balances are hidden from staff. It deliberately does not use Balances.
type PayrollReport struct {
	AsAt             time.Time
	GeneratedAt      time.Time
	IncludeDisabled  bool
	Annual           *Type
	AnnualCycle      [2]string
	Types            []PayrollType
	People           []PayrollPerson
	UnparsedOpenings int
}

type PayrollType struct {
	Type                 Type
	CycleStart, CycleEnd string
}

type PayrollPerson struct {
	User    users.User
	Figures map[int64]PayrollFigures
	Opening *Days
}

type PayrollFigures struct {
	Allowance                                      *Days
	Adjustments, Approved, ApprovedFuture, Pending Days
}

func (f PayrollFigures) Entitlement() *Days {
	if f.Allowance == nil {
		return nil
	}
	n := *f.Allowance + f.Adjustments
	return &n
}

func (f PayrollFigures) Available() *Days {
	n := f.Entitlement()
	if n != nil {
		*n -= f.Approved + f.Pending
	}
	return n
}

func (f PayrollFigures) ToDate() *Days {
	n := f.Entitlement()
	if n != nil {
		*n += f.ApprovedFuture - f.Approved
	}
	return n
}

// Payroll uses grouped reads, not a query per person. No transaction: the
// DSN's _txlock=immediate would take a write lock for this read-only export.
func (s *Service) Payroll(ctx context.Context, includeDisabled bool) (PayrollReport, error) {
	now := s.Now()
	rep := PayrollReport{AsAt: Today(now, s.cfg.Location), GeneratedAt: now.In(s.cfg.Location), IncludeDisabled: includeDisabled}
	list := s.users.Enabled
	if includeDisabled {
		list = s.users.All
	}
	people, err := list(ctx)
	if err != nil {
		return rep, err
	}
	sort.Slice(people, func(i, j int) bool {
		a, b := strings.ToLower(people[i].Name), strings.ToLower(people[j].Name)
		if a == b {
			return people[i].Username < people[j].Username
		}
		return a < b
	})
	for _, u := range people {
		rep.People = append(rep.People, PayrollPerson{User: u, Figures: map[int64]PayrollFigures{}})
	}
	types, err := s.Types(ctx, false)
	if err != nil {
		return rep, err
	}
	st, err := s.settings(ctx, s.db)
	if err != nil {
		return rep, err
	}
	for _, t := range types {
		cs := CycleStart(rep.AsAt, t.CycleMonths, st.SickCycleAnchor)
		pt := PayrollType{Type: t, CycleStart: fmtDate(cs), CycleEnd: fmtDate(CycleEnd(cs, t.CycleMonths))}
		if t.Code == AnnualCode {
			annual := t
			rep.Annual = &annual
			rep.AnnualCycle = [2]string{pt.CycleStart, pt.CycleEnd}
		}
		figures, err := s.payrollFigures(ctx, t, pt.CycleStart, fmtDate(rep.AsAt))
		if err != nil {
			return rep, err
		}
		keep := t.Active
		for _, f := range figures {
			if f.Adjustments != 0 || f.Approved != 0 || f.Pending != 0 {
				keep = true
			}
		}
		if !keep {
			continue
		}
		rep.Types = append(rep.Types, pt)
		for i := range rep.People {
			p := &rep.People[i]
			if t.OfferedTo(p.User) {
				f := figures[p.User.ID]
				f.Allowance = t.Allowance
				p.Figures[t.ID] = f
			}
		}
	}
	if rep.Annual != nil {
		if err := s.payrollOpenings(ctx, &rep); err != nil {
			return rep, err
		}
	}
	return rep, nil
}

func (s *Service) payrollFigures(ctx context.Context, t Type, cycle, asAt string) (map[int64]PayrollFigures, error) {
	out := map[int64]PayrollFigures{}
	// These two queries mirror balance(); TestPayrollParity pins their parity,
	// especially cancellations, drafts and awaiting_hr not reserving leave.
	rows, err := s.db.QueryContext(ctx, `SELECT user_id, sum(days_x100) FROM balance_adjustments
		WHERE type_id = ? AND cycle_start = ? GROUP BY user_id`, t.ID, cycle)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id int64
		var f PayrollFigures
		if err := rows.Scan(&id, &f.Adjustments); err != nil {
			rows.Close()
			return nil, err
		}
		out[id] = f
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT user_id,
		sum(CASE WHEN status = 'approved' THEN days_x100 ELSE 0 END),
		sum(CASE WHEN status = 'approved' AND start_date > ? THEN days_x100 ELSE 0 END),
		sum(CASE WHEN status = 'pending' THEN days_x100 ELSE 0 END)
		FROM requests WHERE type_id = ? AND cycle_start = ? AND kind = 'leave'
		AND status IN ('approved', 'pending') GROUP BY user_id`, asAt, t.ID, cycle)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var approved, future, pending Days
		if err := rows.Scan(&id, &approved, &future, &pending); err != nil {
			return nil, err
		}
		f := out[id]
		f.Approved, f.ApprovedFuture, f.Pending = approved, future, pending
		out[id] = f
	}
	return out, rows.Err()
}

func (s *Service) payrollOpenings(ctx context.Context, rep *PayrollReport) error {
	const prefix = "Opening balance from payroll"
	// import2026.go balanceRow stored payroll's figure in the reason, not the
	// adjustment amount. It wrote nothing when the app already matched payroll,
	// so a missing opening is honestly blank, never inferred as zero.
	rows, err := s.db.QueryContext(ctx, `SELECT user_id, reason FROM balance_adjustments
		WHERE type_id = ? AND cycle_start = ? AND substr(reason, 1, ?) = ?
		ORDER BY created_at DESC, id DESC`, rep.Annual.ID, rep.AnnualCycle[0], len(prefix), prefix)
	if err != nil {
		return err
	}
	defer rows.Close()
	reasons := map[int64]string{}
	for rows.Next() {
		var id int64
		var reason string
		if err := rows.Scan(&id, &reason); err != nil {
			return err
		}
		if _, seen := reasons[id]; !seen {
			reasons[id] = reason
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range rep.People {
		p := &rep.People[i]
		reason, ok := reasons[p.User.ID]
		if !ok {
			continue
		}
		// Strip the sentence delimiter, then let the shared decimal parser
		// accept (and validate) signs, leading dots and trailing decimal dots.
		_, figure, found := strings.Cut(reason, "D = ")
		figure, terminated := strings.CutSuffix(figure, ".")
		if found && terminated {
			if d, err := ParseDays(figure); err == nil {
				p.Opening = &d
				continue
			}
		}
		rep.UnparsedOpenings++
	}
	return nil
}
