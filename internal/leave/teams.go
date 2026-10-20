package leave

import (
	"context"
	"database/sql"

	"github.com/17xande-dev/leave/internal/users"
)

// Volunteer teams: the same percentage rule as a department, per team. The
// teams themselves come from the directory sync (users.Store.SyncTeams); an
// admin only picks which have a limit.

// Team is a synced team, how many enabled staff are in it and its own limit.
type Team struct {
	ID   int64
	Name string
	// Size counts the members who are enabled users here.
	Size int
	// MaxPercent is the team's own limit; 0 means none (the default, if any,
	// applies).
	MaxPercent int
	// Disabled: gone from the source.
	Disabled bool
}

// Teams lists the teams by name. Disabled ones are left out unless inactive
// is set.
func (s *Service) Teams(ctx context.Context, inactive bool) ([]Team, error) {
	return s.queryTeams(ctx, `WHERE ? OR t.disabled = 0`, inactive)
}

// TeamsFor lists the live teams a person is in, for their admin page.
func (s *Service) TeamsFor(ctx context.Context, userID int64) ([]Team, error) {
	return s.queryTeams(ctx, `WHERE t.disabled = 0 AND t.id IN (SELECT team_id FROM team_members WHERE user_id = ?)`, userID)
}

func (s *Service) queryTeams(ctx context.Context, where string, args ...any) ([]Team, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT t.id, t.name, t.disabled,
		(SELECT count(*) FROM team_members m JOIN users x ON x.id = m.user_id
			WHERE m.team_id = t.id AND x.disabled = 0),
		COALESCE(r.max_percent, 0)
		FROM teams t LEFT JOIN team_rules r ON r.team_id = t.id `+where+` ORDER BY t.disabled, t.name, t.id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Team
	for rows.Next() {
		var t Team
		if err := rows.Scan(&t.ID, &t.Name, &t.Disabled, &t.Size, &t.MaxPercent); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// SetTeamRule sets a team's own limit; 0 removes it. A team that does not
// exist is ErrNotFound.
func (s *Service) SetTeamRule(ctx context.Context, teamID int64, maxPercent int) error {
	if maxPercent < 0 || maxPercent > 100 {
		return FieldErrors{"percent": "Enter a whole number from 1 to 100, or leave it empty for no limit."}
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM teams WHERE id = ?`, teamID).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		if maxPercent == 0 {
			_, err := tx.ExecContext(ctx, `DELETE FROM team_rules WHERE team_id = ?`, teamID)
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO team_rules (team_id, max_percent) VALUES (?, ?)
			ON CONFLICT(team_id) DO UPDATE SET max_percent = excluded.max_percent`, teamID, maxPercent)
		return err
	})
}

// SetTeamDefault sets the limit for teams with none of their own; 0 removes
// it. It is the same field as on the settings page, kept apart from
// SaveSettings so the Teams page need not post every other setting.
func (s *Service) SetTeamDefault(ctx context.Context, admin users.User, percent int) error {
	if !admin.Admin {
		return ErrNotAllowed
	}
	if percent < 0 || percent > 100 {
		return FieldErrors{"team_default": "Enter a whole number from 1 to 100, or leave it empty for no default."}
	}
	_, err := s.db.ExecContext(ctx, `UPDATE settings SET team_default_percent = ?, updated_by_id = ?, updated_at = ?
		WHERE id = 1`, percent, admin.ID, stampNow(s.Now()))
	return err
}
