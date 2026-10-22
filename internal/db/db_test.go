package db

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

// The DDL says STRICT, but CREATE TABLE IF NOT EXISTS reports success against a
// table created before STRICT was added, so the only trustworthy check is what
// SQLite actually recorded.
func TestEveryTableIsStrict(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	rows, err := d.Query(`SELECT name, sql FROM sqlite_master
	                      WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var name, ddl string
		if err := rows.Scan(&name, &ddl); err != nil {
			t.Fatal(err)
		}
		n++
		if !strings.HasSuffix(strings.TrimSpace(strings.ToUpper(ddl)), ") STRICT") {
			t.Errorf("table %s is not STRICT", name)
		}
	}
	// A minimum, so the test cannot pass vacuously if the schema stops applying.
	if n < 8 {
		t.Fatalf("found %d tables, want at least 8", n)
	}
}

// Foreign keys are off in SQLite unless the connection turns them on; without
// the DSN flag every REFERENCES in the schema is decoration.
func TestForeignKeysEnforced(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	_, err = d.Exec(`INSERT INTO sessions (token_hash, user_id, created_at, expires_at)
	                 VALUES ('x', 999, '', '')`)
	if err == nil {
		t.Fatal("insert referencing a missing user succeeded; foreign keys are off")
	}
}

// A database from an older build must be refused at boot with a clear
// message, not fail on the first query that touches a new column.
func TestOldDatabaseRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	old.Exec(`CREATE TABLE requests (id INTEGER PRIMARY KEY, status TEXT) STRICT`)
	old.Close()
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "older build") {
		t.Fatalf("old database: err = %v", err)
	}
}

// An old requests table that has the shutdown column but not the newer ones
// is refused naming the missing column.
func TestOldDatabaseMissingNewerColumnRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	old.Exec(`CREATE TABLE requests (id INTEGER PRIMARY KEY, status TEXT, shutdown_part_id INTEGER) STRICT`)
	old.Close()
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "requests.escalate") {
		t.Fatalf("old database: err = %v", err)
	}
}

// A requests table written before awaiting_hr existed has every column but the
// status CHECK, which only the DDL shows.
func TestOldDatabaseWithoutAwaitingHRRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	old.Exec(`CREATE TABLE requests (id INTEGER PRIMARY KEY, shutdown_part_id INTEGER, escalate INTEGER,
		allow_negative INTEGER, recorded_by_id INTEGER, status TEXT CHECK (status IN ('draft', 'pending'))) STRICT`)
	old.Close()
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "awaiting_hr") {
		t.Fatalf("old database: err = %v", err)
	}
}

// A database that has everything but the settings table was made by a build
// that still read policy from the environment; starting it would silently
// apply the defaults instead of what the operator had configured.
func TestOldDatabaseWithoutSettingsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	old.Exec(`CREATE TABLE requests (id INTEGER PRIMARY KEY, shutdown_part_id INTEGER, escalate INTEGER,
		allow_negative INTEGER, recorded_by_id INTEGER, status TEXT CHECK (status IN ('draft', 'pending', 'awaiting_hr'))) STRICT`)
	old.Exec(`CREATE TABLE coverage_rules (department TEXT PRIMARY KEY) STRICT`)
	old.Exec(`CREATE TABLE request_steps (id INTEGER PRIMARY KEY) STRICT`)
	old.Close()
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "settings") {
		t.Fatalf("old database: err = %v", err)
	}
}

// A database from before the OAuth tables would let the app start and then
// fail the first time someone connects an assistant.
func TestOldDatabaseWithoutOAuthRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	old.Exec(`CREATE TABLE requests (id INTEGER PRIMARY KEY, shutdown_part_id INTEGER, escalate INTEGER,
		allow_negative INTEGER, recorded_by_id INTEGER, status TEXT CHECK (status IN ('draft', 'pending', 'awaiting_hr'))) STRICT`)
	old.Exec(`CREATE TABLE coverage_rules (department TEXT PRIMARY KEY) STRICT`)
	old.Exec(`CREATE TABLE request_steps (id INTEGER PRIMARY KEY) STRICT`)
	old.Exec(`CREATE TABLE settings (id INTEGER PRIMARY KEY, team_default_percent INTEGER) STRICT`)
	old.Close()
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "oauth_clients") {
		t.Fatalf("old database: err = %v", err)
	}
}

// A database from before volunteer teams: the settings column and the team
// tables are both caught, rather than failing at the first leave request.
func TestOldDatabaseWithoutTeamsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	old.Exec(`CREATE TABLE requests (id INTEGER PRIMARY KEY, shutdown_part_id INTEGER, escalate INTEGER,
		allow_negative INTEGER, recorded_by_id INTEGER, status TEXT CHECK (status IN ('draft', 'pending', 'awaiting_hr'))) STRICT`)
	old.Exec(`CREATE TABLE settings (id INTEGER PRIMARY KEY) STRICT`)
	old.Close()
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "team_default_percent") {
		t.Fatalf("old database without the settings column: err = %v", err)
	}

	path = filepath.Join(t.TempDir(), "old2.db")
	old, _ = sql.Open("sqlite3", "file:"+path)
	for _, q := range []string{
		`CREATE TABLE requests (id INTEGER PRIMARY KEY, shutdown_part_id INTEGER, escalate INTEGER,
			allow_negative INTEGER, recorded_by_id INTEGER, status TEXT CHECK (status IN ('draft', 'pending', 'awaiting_hr'))) STRICT`,
		`CREATE TABLE coverage_rules (department TEXT PRIMARY KEY) STRICT`,
		`CREATE TABLE request_steps (id INTEGER PRIMARY KEY) STRICT`,
		`CREATE TABLE settings (id INTEGER PRIMARY KEY, team_default_percent INTEGER) STRICT`,
		`CREATE TABLE oauth_clients (id INTEGER PRIMARY KEY) STRICT`,
		`CREATE TABLE oauth_grants (id INTEGER PRIMARY KEY) STRICT`,
		`CREATE TABLE oauth_codes (id INTEGER PRIMARY KEY) STRICT`,
		`CREATE TABLE oauth_tokens (id INTEGER PRIMARY KEY) STRICT`,
	} {
		old.Exec(q)
	}
	old.Close()
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "teams") {
		t.Fatalf("old database without the team tables: err = %v", err)
	}
}

// A database from before per-type backdating or recorded leave is refused with
// the recreate message rather than migrated.
func TestOldBackdatingAndRecordedShapesRefused(t *testing.T) {
	for name, tc := range map[string]struct{ mutate, want string }{
		"no backdate_days":       {`ALTER TABLE leave_types DROP COLUMN backdate_days`, "leave_types.backdate_days"},
		"no recorded_by_id":      {`ALTER TABLE requests DROP COLUMN recorded_by_id`, "requests.recorded_by_id"},
		"old backdate flag":      {`ALTER TABLE leave_types ADD COLUMN backdate INTEGER`, "leave_types.backdate"},
		"old sick_backdate_days": {`ALTER TABLE settings ADD COLUMN sick_backdate_days INTEGER`, "settings.sick_backdate_days"},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "old.db")
			d, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := d.Exec(tc.mutate); err != nil {
				t.Fatal(err)
			}
			d.Close()
			_, err = Open(path)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "make recreate-db") {
				t.Fatalf("err = %v, want a recreate-db refusal naming %s", err, tc.want)
			}
		})
	}
}

// A fresh database opens, and reopens: checkCurrent accepts today's schema.
func TestFreshDatabaseOpensTwice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.db")
	for i := 0; i < 2; i++ {
		d, err := Open(path)
		if err != nil {
			t.Fatalf("open %d: %v", i+1, err)
		}
		d.Close()
	}
}
