// Package users is the local copy of the directory: every person the app knows
// about, refreshed by a periodic sync and on each sign-in.
//
// The copy exists because approvals need people who are not signed in -- an
// approver's email address, a department head's reports -- and asking the DC
// on every page view would make the app as available as the DC is.
package users

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/17xande-dev/leave/internal/directory"
)

type User struct {
	ID         int64
	DN         string
	Username   string
	Name       string
	Email      string
	Department string
	ManagerDN  string
	Admin      bool
	Disabled   bool
	// LongWeekend: this person's weekend off runs Friday to Sunday. Set by an
	// admin; the directory sync never touches it.
	LongWeekend bool
}

var ErrNotFound = errors.New("users: not found")

type Store struct{ db *sql.DB }

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

const cols = `id, dn, username, name, email, department, manager_dn, is_admin, disabled, long_weekend`

func scan(row interface{ Scan(...any) error }) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.DN, &u.Username, &u.Name, &u.Email, &u.Department, &u.ManagerDN, &u.Admin, &u.Disabled, &u.LongWeekend)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	return u, err
}

// Upsert records one person as the directory describes them, re-enabling the
// row if it had been disabled. Keyed on the directory's permanent id.
func (s *Store) Upsert(ctx context.Context, d directory.User) (User, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()
	u, err := s.upsert(ctx, tx, d, stamp(time.Now()))
	if err != nil {
		return User{}, err
	}
	return u, tx.Commit()
}

// stamp formats synced_at. Nanosecond precision because Sync identifies the
// rows it did not touch by comparing stamps, and a sign-in in the same second
// must not be mistaken for part of the sync.
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func (s *Store) upsert(ctx context.Context, tx *sql.Tx, d directory.User, syncedAt string) (User, error) {
	if d.ID == "" {
		return User{}, fmt.Errorf("users: %s has no directory id", d.DN)
	}
	// A username is unique, but not for ever: a leaver's account deleted and
	// the name reused for a new starter is a different person with a new id.
	// The old row keeps its history under a retired name ("sam#12") so the
	// new person can have theirs.
	if _, err := tx.ExecContext(ctx, `UPDATE users SET username = username || '#' || id
		WHERE username = ? AND ext_id <> ?`, d.Username, d.ID); err != nil {
		return User{}, err
	}
	return scan(tx.QueryRowContext(ctx, `
		INSERT INTO users (ext_id, dn, username, name, email, department, manager_dn, is_admin, disabled, synced_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, ?)
		ON CONFLICT(ext_id) DO UPDATE SET
			dn = excluded.dn, username = excluded.username, name = excluded.name, email = excluded.email,
			department = excluded.department, manager_dn = excluded.manager_dn,
			is_admin = excluded.is_admin, disabled = 0, synced_at = excluded.synced_at
		RETURNING `+cols,
		d.ID, d.DN, d.Username, d.Name, d.Email, d.Department, d.ManagerDN, d.Admin, syncedAt))
}

// Sync makes the table match a full directory listing: everyone listed is
// upserted, and everyone not listed is disabled -- never deleted, since their
// requests still reference them. Disabling also ends their sessions (the
// session lookup refuses a disabled user).
//
// An empty listing is refused rather than applied: a misconfigured base DN or
// filter would otherwise disable every account, including every admin, in
// one pass.
func (s *Store) Sync(ctx context.Context, all []directory.User) (upserted, disabled int, err error) {
	if len(all) == 0 {
		return 0, 0, fmt.Errorf("users: directory returned nobody; refusing to disable every account")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()

	at := stamp(time.Now())
	for _, d := range all {
		if _, err := s.upsert(ctx, tx, d, at); err != nil {
			return 0, 0, fmt.Errorf("users: sync %s: %w", d.DN, err)
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE users SET disabled = 1 WHERE disabled = 0 AND synced_at <> ?`, at)
	if err != nil {
		return 0, 0, err
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return len(all), int(n), nil
}

func (s *Store) ByID(ctx context.Context, id int64) (User, error) {
	return scan(s.db.QueryRowContext(ctx, `SELECT `+cols+` FROM users WHERE id = ?`, id))
}

func (s *Store) ByUsername(ctx context.Context, username string) (User, error) {
	return scan(s.db.QueryRowContext(ctx, `SELECT `+cols+` FROM users WHERE username = ?`,
		directory.NormalizeUsername(username)))
}

// ByDN finds whoever sits at dn now. A disabled row can still hold an old DN
// (a leaver whose DN was later reused), so the enabled one wins.
func (s *Store) ByDN(ctx context.Context, dn string) (User, error) {
	return scan(s.db.QueryRowContext(ctx, `SELECT `+cols+` FROM users WHERE dn = ?
		ORDER BY disabled, id DESC LIMIT 1`, strings.TrimSpace(dn)))
}

// SetLongWeekend sets whether someone's weekend off includes the Friday.
func (s *Store) SetLongWeekend(ctx context.Context, id int64, on bool) error {
	res, err := s.db.ExecContext(ctx, `UPDATE users SET long_weekend = ? WHERE id = ?`, on, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Admins lists every enabled admin, for routing requests nobody else can approve.
func (s *Store) Admins(ctx context.Context) ([]User, error) {
	return s.list(ctx, `SELECT `+cols+` FROM users WHERE is_admin = 1 AND disabled = 0 ORDER BY name`)
}

// All lists everyone, enabled first, for the admin's user list.
func (s *Store) All(ctx context.Context) ([]User, error) {
	return s.list(ctx, `SELECT `+cols+` FROM users ORDER BY disabled, name`)
}

// Enabled lists everyone who can currently sign in, for approver pickers.
func (s *Store) Enabled(ctx context.Context) ([]User, error) {
	return s.list(ctx, `SELECT `+cols+` FROM users WHERE disabled = 0 ORDER BY name`)
}

func (s *Store) list(ctx context.Context, q string, args ...any) ([]User, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []User
	for rows.Next() {
		u, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// SyncTeams makes the teams table match a source's full listing: each team is
// upserted by its source id and its memberships replaced, and a team of this
// source that is no longer listed is disabled, never deleted, so its rule
// survives a brief outage in the source. Members who are not users here are
// skipped: only staff in the app count towards a team's size.
//
// Unlike Sync, an empty listing is applied. Whether it is a result or a
// misconfiguration is the source's call (it returns an error for a failed
// search and directory.ErrTeamsOff when unset), and an empty list is how the
// last team disappearing from AD looks.
func (s *Store) SyncTeams(ctx context.Context, source string, teams []directory.Team) (upserted, disabled int, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()

	at := stamp(time.Now())
	for _, t := range teams {
		if t.ID == "" {
			return 0, 0, fmt.Errorf("users: team %q has no id", t.Name)
		}
		var id int64
		if err := tx.QueryRowContext(ctx, `INSERT INTO teams (ext_id, name, source, disabled, synced_at)
			VALUES (?, ?, ?, 0, ?)
			ON CONFLICT(ext_id) DO UPDATE SET name = excluded.name, source = excluded.source,
				disabled = 0, synced_at = excluded.synced_at
			RETURNING id`, t.ID, t.Name, source, at).Scan(&id); err != nil {
			return 0, 0, fmt.Errorf("users: sync team %s: %w", t.Name, err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM team_members WHERE team_id = ?`, id); err != nil {
			return 0, 0, err
		}
		for _, m := range t.MemberIDs {
			if _, err := tx.ExecContext(ctx, `INSERT INTO team_members (team_id, user_id)
				SELECT ?, id FROM users WHERE ext_id = ? ON CONFLICT DO NOTHING`, id, m); err != nil {
				return 0, 0, err
			}
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE teams SET disabled = 1 WHERE disabled = 0 AND source = ? AND synced_at <> ?`, source, at)
	if err != nil {
		return 0, 0, err
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return len(teams), int(n), nil
}
