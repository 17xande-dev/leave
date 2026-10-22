package leave

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	UnitWorkdays = "workdays"
	UnitCalendar = "calendar"
	// UnitWeekend counts weekends off: one per Saturday-to-Sunday (or, for
	// someone on long weekends, Friday-to-Sunday) request.
	UnitWeekend = "weekend"
)

// What a type may do with the restricted day (the restricted day in Settings, e.g. Sunday).
const (
	RestrictAny   = "any"   // no rule: sick leave cannot be planned around it
	RestrictMin   = "min"   // only inside a request of at least the settings minimum of working days
	RestrictNever = "never" // not at all: floating holidays are not for the restricted day
)

// What department coverage limits do with a type; see Type.CoverageRule.
const (
	CoverageLimit = "limit" // counts as away and can be blocked by the limit
	CoverageCount = "count" // counts as away, never blocked
)

// Type is a kind of leave and its allowance.
type Type struct {
	ID   int64
	Code string
	Name string
	// Allowance is days per cycle; nil means unlimited (unpaid leave).
	Allowance   *int
	CycleMonths int
	Unit        string
	// BackdateDays is how far into the past a request of this type may
	// start; 0 means it cannot start before today.
	BackdateDays int
	RequiresNote bool
	Active       bool
	Sort         int
	// Restricted is the rule for the restricted day; see the Restrict* constants.
	Restricted string
	// CoverageRule is the rule for department coverage limits: CoverageLimit
	// or CoverageCount.
	CoverageRule string

	inactive bool // seed only: the default's Active is the inverse
}

func intp(n int) *int { return &n }

// MaxBackdateDays is the most a type may allow, and DefaultBackdateDays what
// every seeded type does: the policy is "no more than three months", and three
// calendar months are at most 92 days. An admin may lower it per type.
const (
	MaxBackdateDays     = 92
	DefaultBackdateDays = 92
)

// bceaDefaults are the Basic Conditions of Employment Act minimums for a
// five-day week, seeded into an empty database. An admin edits them from there;
// the seed never overwrites.
var bceaDefaults = []Type{
	{Code: "annual", Name: "Annual leave", Allowance: intp(15), CycleMonths: 12, Unit: UnitWorkdays, CoverageRule: CoverageLimit, Restricted: RestrictMin},
	// 30 days per 36-month cycle, usable in arrears. Nobody plans being ill
	// around the restricted day, and neither do the other statutory types.
	{Code: "sick", Name: "Sick leave", Allowance: intp(30), CycleMonths: 36, Unit: UnitWorkdays, CoverageRule: CoverageCount, Restricted: RestrictAny},
	{Code: "family", Name: "Family responsibility leave", Allowance: intp(3), CycleMonths: 12, Unit: UnitWorkdays, RequiresNote: true, CoverageRule: CoverageCount, Restricted: RestrictAny},
	// Four consecutive months, stored as 120 calendar days.
	{Code: "maternity", Name: "Maternity leave", Allowance: intp(120), CycleMonths: 12, Unit: UnitCalendar, CoverageRule: CoverageCount, Restricted: RestrictAny},
	{Code: "parental", Name: "Parental leave", Allowance: intp(10), CycleMonths: 12, Unit: UnitCalendar, CoverageRule: CoverageCount, Restricted: RestrictAny},
	{Code: "unpaid", Name: "Unpaid leave", CycleMonths: 12, Unit: UnitWorkdays, RequiresNote: true, CoverageRule: CoverageLimit, Restricted: RestrictMin},
	// Not BCEA: days off the employer grants that do not touch annual leave.
	// Seeded inactive with no days, because how many is a policy decision;
	// an admin sets the allowance and switches it on. Never on the
	// restricted day: that is what weekends off are for.
	{Code: "floating", Name: "Floating holidays", Allowance: intp(0), CycleMonths: 12, Unit: UnitWorkdays, CoverageRule: CoverageLimit, Restricted: RestrictNever, inactive: true},
	// Weekends off: a yearly allowance of whole weekends, the one way to have
	// the restricted day off without taking a run of leave around it.
	{Code: "weekend", Name: "Weekends off", Allowance: intp(0), CycleMonths: 12, Unit: UnitWeekend, CoverageRule: CoverageLimit, Restricted: RestrictAny, inactive: true},
	// The free part of a company shutdown: unlimited, and inactive so nobody
	// can apply for it themselves -- only shutdown requests use it.
	{Code: "closure", Name: "Company closure", CycleMonths: 12, Unit: UnitWorkdays, CoverageRule: CoverageCount, Restricted: RestrictAny, inactive: true},
}

// ErrNotFound is returned for a missing type or request.
var ErrNotFound = errors.New("leave: not found")

const typeCols = `id, code, name, allowance_days, cycle_months, unit, backdate_days, requires_note, active, sort, restricted_rule, coverage_rule`

func scanType(row interface{ Scan(...any) error }) (Type, error) {
	var t Type
	var allowance sql.NullInt64
	err := row.Scan(&t.ID, &t.Code, &t.Name, &allowance, &t.CycleMonths, &t.Unit, &t.BackdateDays, &t.RequiresNote, &t.Active, &t.Sort, &t.Restricted, &t.CoverageRule)
	if errors.Is(err, sql.ErrNoRows) {
		return Type{}, ErrNotFound
	}
	if allowance.Valid {
		t.Allowance = intp(int(allowance.Int64))
	}
	return t, err
}

// SeedTypes inserts any default type whose code is not in the table yet.
// An existing row is never touched, so an admin's edits survive every
// restart; and types cannot be deleted (only deactivated), so a missing code
// means a default added since the database was created.
func (s *Service) SeedTypes(ctx context.Context) error {
	for i, t := range bceaDefaults {
		t.Active, t.Sort, t.BackdateDays = !t.inactive, i, DefaultBackdateDays
		_, err := s.SaveType(ctx, t)
		var fe FieldErrors
		if errors.As(err, &fe) && fe["code"] != "" {
			continue // already there
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// Types lists leave types in display order.
func (s *Service) Types(ctx context.Context, activeOnly bool) ([]Type, error) {
	q := `SELECT ` + typeCols + ` FROM leave_types`
	if activeOnly {
		q += ` WHERE active = 1`
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY sort, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Type
	for rows.Next() {
		t, err := scanType(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Service) Type(ctx context.Context, id int64) (Type, error) {
	return scanType(s.db.QueryRowContext(ctx, `SELECT `+typeCols+` FROM leave_types WHERE id = ?`, id))
}

// BackdateMessage is the error for a backdating limit outside 0 to MaxBackdateDays.
var BackdateMessage = fmt.Sprintf("Enter a whole number of days from 0 to %d.", MaxBackdateDays)

// ValidateType checks t as SaveType does (after it trims the code and name),
// so a form with a field that did not even parse can still report every other
// problem in one pass. It fills in the default rules like SaveType.
func ValidateType(t *Type) FieldErrors {
	t.Code = strings.TrimSpace(strings.ToLower(t.Code))
	t.Name = strings.TrimSpace(t.Name)
	errs := FieldErrors{}
	if t.Code == "" {
		errs["code"] = "Enter a short code, e.g. annual."
	}
	if t.Name == "" {
		errs["name"] = "Enter a name."
	}
	if t.Unit != UnitWorkdays && t.Unit != UnitCalendar && t.Unit != UnitWeekend {
		errs["unit"] = "Choose how days are counted."
	}
	if t.Restricted == "" {
		t.Restricted = RestrictMin
	}
	if t.Restricted != RestrictAny && t.Restricted != RestrictMin && t.Restricted != RestrictNever {
		errs["restricted"] = "Choose a rule for the restricted day."
	}
	if t.CoverageRule == "" {
		t.CoverageRule = CoverageLimit
	}
	if t.CoverageRule != CoverageLimit && t.CoverageRule != CoverageCount {
		errs["coverage_rule"] = "Choose how department limits treat this type."
	}
	if t.CycleMonths != 12 && t.CycleMonths != 36 {
		errs["cycle_months"] = "The cycle is 12 or 36 months."
	}
	if t.BackdateDays < 0 || t.BackdateDays > MaxBackdateDays {
		errs["backdate_days"] = BackdateMessage
	}
	if t.Allowance != nil && *t.Allowance < 0 {
		errs["allowance"] = "The allowance cannot be negative."
	}
	return errs
}

// SaveType inserts (ID 0) or updates a type.
func (s *Service) SaveType(ctx context.Context, t Type) (int64, error) {
	errs := ValidateType(&t)
	if len(errs) > 0 {
		return 0, errs
	}
	var allowance any
	if t.Allowance != nil {
		allowance = *t.Allowance
	}
	if t.ID == 0 {
		res, err := s.db.ExecContext(ctx, `INSERT INTO leave_types
			(code, name, allowance_days, cycle_months, unit, backdate_days, requires_note, active, sort, restricted_rule, coverage_rule)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT(code) DO NOTHING`,
			t.Code, t.Name, allowance, t.CycleMonths, t.Unit, t.BackdateDays, t.RequiresNote, t.Active, t.Sort, t.Restricted, t.CoverageRule)
		if err != nil {
			return 0, err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return 0, FieldErrors{"code": "Another leave type already uses that code."}
		}
		return res.LastInsertId()
	}
	res, err := s.db.ExecContext(ctx, `UPDATE leave_types SET code = ?, name = ?, allowance_days = ?,
		cycle_months = ?, unit = ?, backdate_days = ?, requires_note = ?, active = ?, sort = ?, restricted_rule = ?,
		coverage_rule = ? WHERE id = ?`,
		t.Code, t.Name, allowance, t.CycleMonths, t.Unit, t.BackdateDays, t.RequiresNote, t.Active, t.Sort, t.Restricted,
		t.CoverageRule, t.ID)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return 0, FieldErrors{"code": "Another leave type already uses that code."}
		}
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrNotFound
	}
	return t.ID, nil
}

// HolidayRow is a stored public holiday.
type HolidayRow struct {
	Date   string
	Name   string
	Source string
}

// EnsureHolidays generates the computed holidays for a year the first time the
// year is needed. A year that already has computed rows is left alone, so an
// admin who deleted one (a holiday moved by proclamation) is not overruled.
func (s *Service) EnsureHolidays(ctx context.Context, year int) error {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM holidays
		WHERE source = 'computed' AND date BETWEEN ? AND ?`,
		fmt.Sprintf("%d-01-01", year), fmt.Sprintf("%d-12-31", year)).Scan(&n); err != nil || n > 0 {
		return err
	}
	return s.GenerateHolidays(ctx, year)
}

// GenerateHolidays (re)inserts the computed holidays for a year, leaving any
// existing row -- manual or computed -- as it is.
func (s *Service) GenerateHolidays(ctx context.Context, year int) error {
	for _, h := range SAPublicHolidays(year) {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO holidays (date, name, source) VALUES (?, ?, 'computed')
			ON CONFLICT(date) DO NOTHING`, fmtDate(h.Date), h.Name); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) AddHoliday(ctx context.Context, date, name string) error {
	t, err := ParseDate(date)
	errs := FieldErrors{}
	if err != nil {
		errs["date"] = "Enter a date."
	}
	if strings.TrimSpace(name) == "" {
		errs["name"] = "Enter a name."
	}
	if len(errs) > 0 {
		return errs
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO holidays (date, name, source) VALUES (?, ?, 'manual')
		ON CONFLICT(date) DO UPDATE SET name = excluded.name`, fmtDate(t), strings.TrimSpace(name))
	return err
}

func (s *Service) DeleteHoliday(ctx context.Context, date string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM holidays WHERE date = ?`, date)
	return err
}

func (s *Service) HolidaysInYear(ctx context.Context, year int) ([]HolidayRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT date, name, source FROM holidays
		WHERE date BETWEEN ? AND ? ORDER BY date`, fmt.Sprintf("%d-01-01", year), fmt.Sprintf("%d-12-31", year))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HolidayRow
	for rows.Next() {
		var h HolidayRow
		if err := rows.Scan(&h.Date, &h.Name, &h.Source); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// holidaySet returns the stored holidays between start and end. Callers run
// ensureYears first and outside any transaction: with _txlock=immediate a
// write on s.db from inside a transaction would wait on its own lock.
func (s *Service) holidaySet(ctx context.Context, q querier, start, end time.Time) (map[time.Time]bool, error) {
	out := map[time.Time]bool{}
	rows, err := q.QueryContext(ctx, `SELECT date FROM holidays WHERE date BETWEEN ? AND ?`, fmtDate(start), fmtDate(end))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var ds string
		if err := rows.Scan(&ds); err != nil {
			return nil, err
		}
		if t, err := ParseDate(ds); err == nil {
			out[t] = true
		}
	}
	return out, rows.Err()
}

type querier interface {
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
}

// FieldErrors maps a form field to what is wrong with it. It is an error so
// store methods can return it, and handlers render it next to each field.
type FieldErrors map[string]string

func (e FieldErrors) Error() string {
	parts := make([]string, 0, len(e))
	for k, v := range e {
		parts = append(parts, k+": "+v)
	}
	return "invalid: " + strings.Join(parts, "; ")
}
