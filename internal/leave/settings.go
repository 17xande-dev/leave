package leave

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/17xande-dev/leave/internal/users"
)

// Settings is the policy an admin changes on Admin -> Settings, as opposed to
// the deployment wiring in the environment. It is read afresh by each
// operation and never cached across requests, so a change applies to the very
// next quote.
type Settings struct {
	// Week is the days people normally work; leave costs only these.
	Week Week
	// RestrictedDay, when set, is a working day that cannot be taken off on
	// its own: leave including it must cover RestrictedMinDays working days.
	RestrictedDay     *time.Weekday
	RestrictedMinDays int
	// FinalApproverID is the user whose approval ends the chain and who
	// approves for anyone with no usable manager; 0 means nobody, and those
	// people go to the admins' queue.
	FinalApproverID int64
	// SickCycleAnchor is the year the BCEA 36-month sick-leave cycle counts from.
	SickCycleAnchor int
	// TeamDefaultPercent is the coverage limit for a volunteer team with no
	// rule of its own; 0 means no default.
	TeamDefaultPercent int

	// UpdatedBy and UpdatedAt record the last change, for the settings page.
	// They are ignored by SaveSettings.
	UpdatedBy string
	UpdatedAt string
}

// Day is one weekday as the settings form and the database name it.
type Day struct {
	Code    string // the stored and posted value: mon, tue, ...
	Name    string
	Weekday time.Weekday
}

// Days lists the week Monday first, the order people read it in.
var Days = []Day{
	{"mon", "Monday", time.Monday}, {"tue", "Tuesday", time.Tuesday}, {"wed", "Wednesday", time.Wednesday},
	{"thu", "Thursday", time.Thursday}, {"fri", "Friday", time.Friday}, {"sat", "Saturday", time.Saturday},
	{"sun", "Sunday", time.Sunday},
}

// ParseDay maps a day code to its weekday. Only the exact three-letter codes
// are accepted: a form value that is anything else is a bad request, not a
// day to guess at.
func ParseDay(code string) (time.Weekday, bool) {
	for _, d := range Days {
		if d.Code == code {
			return d.Weekday, true
		}
	}
	return 0, false
}

// dayCode is the stored code for a weekday.
func dayCode(w time.Weekday) string {
	for _, d := range Days {
		if d.Weekday == w {
			return d.Code
		}
	}
	return ""
}

// Has reports whether the week works on the given weekday.
func (w Week) Has(d time.Weekday) bool { return w[d] }

func (w Week) codes() string {
	var out []string
	for _, d := range Days {
		if w[d.Weekday] {
			out = append(out, d.Code)
		}
	}
	return strings.Join(out, ",")
}

func parseWeek(s string) (Week, error) {
	var w Week
	for _, c := range strings.Split(s, ",") {
		d, ok := ParseDay(c)
		if !ok {
			return Week{}, fmt.Errorf("leave: stored work_days has unknown day %q", c)
		}
		w[d] = true
	}
	return w, nil
}

// Settings loads the current settings.
func (s *Service) Settings(ctx context.Context) (Settings, error) { return s.settings(ctx, s.db) }

// settings loads through q so an operation inside a transaction reads the
// same state it commits against.
func (s *Service) settings(ctx context.Context, q querier) (Settings, error) {
	var (
		st       Settings
		days, rd string
		approver sql.NullInt64
	)
	err := q.QueryRowContext(ctx, `SELECT s.work_days, s.restricted_day, s.restricted_min_days, s.final_approver_id,
		s.sick_cycle_start_year, s.team_default_percent, COALESCE(u.name, ''), s.updated_at
		FROM settings s LEFT JOIN users u ON u.id = s.updated_by_id WHERE s.id = 1`).
		Scan(&days, &rd, &st.RestrictedMinDays, &approver, &st.SickCycleAnchor,
			&st.TeamDefaultPercent, &st.UpdatedBy, &st.UpdatedAt)
	if err != nil {
		return Settings{}, err
	}
	if st.Week, err = parseWeek(days); err != nil {
		return Settings{}, err
	}
	if rd != "" {
		d, _ := ParseDay(rd)
		st.RestrictedDay = &d
	}
	st.FinalApproverID = approver.Int64
	return st, nil
}

// SaveSettings validates and stores s on behalf of admin. Validation failures
// come back as FieldErrors keyed days, restricted_day, restricted_min,
// final_approver, sick_cycle and team_default.
func (s *Service) SaveSettings(ctx context.Context, admin users.User, in Settings) error {
	if !admin.Admin {
		return ErrNotAllowed
	}
	errs := FieldErrors{}
	if in.Week == (Week{}) {
		errs["days"] = "Choose at least one working day."
	}
	if rd := in.RestrictedDay; rd != nil && !in.Week[*rd] {
		errs["restricted_day"] = "The restricted day must be one of the working days: a day nobody works is never taken off anyway."
	}
	if in.RestrictedMinDays < 1 || in.RestrictedMinDays > 31 {
		errs["restricted_min"] = "Enter a whole number from 1 to 31."
	}
	if in.SickCycleAnchor < 2000 || in.SickCycleAnchor > 2100 {
		errs["sick_cycle"] = "Enter a year from 2000 to 2100."
	}
	if in.TeamDefaultPercent < 0 || in.TeamDefaultPercent > 100 {
		errs["team_default"] = "Enter a whole number from 1 to 100, or leave it empty for no default."
	}
	if in.FinalApproverID != 0 {
		u, err := s.users.ByID(ctx, in.FinalApproverID)
		if err != nil || u.Disabled {
			errs["final_approver"] = "Choose someone who is currently enabled, or none."
		}
	}
	if len(errs) > 0 {
		return errs
	}
	rd := ""
	if in.RestrictedDay != nil {
		rd = dayCode(*in.RestrictedDay)
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE settings SET work_days = ?, restricted_day = ?, restricted_min_days = ?,
			final_approver_id = ?, sick_cycle_start_year = ?, team_default_percent = ?, updated_by_id = ?, updated_at = ?
			WHERE id = 1`, in.Week.codes(), rd, in.RestrictedMinDays, nullID(in.FinalApproverID),
			in.SickCycleAnchor, in.TeamDefaultPercent, admin.ID, stampNow(s.Now()))
		return err
	})
}
