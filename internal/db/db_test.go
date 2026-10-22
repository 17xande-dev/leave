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
		allow_negative INTEGER, status TEXT CHECK (status IN ('draft', 'pending'))) STRICT`)
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
		allow_negative INTEGER, status TEXT CHECK (status IN ('draft', 'pending', 'awaiting_hr'))) STRICT`)
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
		allow_negative INTEGER, status TEXT CHECK (status IN ('draft', 'pending', 'awaiting_hr'))) STRICT`)
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
		allow_negative INTEGER, status TEXT CHECK (status IN ('draft', 'pending', 'awaiting_hr'))) STRICT`)
	old.Exec(`CREATE TABLE settings (id INTEGER PRIMARY KEY) STRICT`)
	old.Close()
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "team_default_percent") {
		t.Fatalf("old database without the settings column: err = %v", err)
	}

	path = filepath.Join(t.TempDir(), "old2.db")
	old, _ = sql.Open("sqlite3", "file:"+path)
	for _, q := range []string{
		`CREATE TABLE requests (id INTEGER PRIMARY KEY, shutdown_part_id INTEGER, escalate INTEGER,
			allow_negative INTEGER, status TEXT CHECK (status IN ('draft', 'pending', 'awaiting_hr'))) STRICT`,
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

// A database from before per-type backdating (a 0/1 flag on each type and a
// global sick setting) opens, keeps its rows and its STRICT tables, gives every
// existing type the 90-day policy whatever the old flag said, and loses both
// old columns. Opening it a second time changes nothing.
func TestMigrateBackdating(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	fresh, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// Rebuild the two tables as the previous build defined them.
	for _, stmt := range []string{
		`DROP TABLE leave_types`,
		`CREATE TABLE leave_types (
			id INTEGER PRIMARY KEY, code TEXT NOT NULL UNIQUE, name TEXT NOT NULL,
			allowance_days INTEGER CHECK (allowance_days IS NULL OR allowance_days >= 0),
			cycle_months INTEGER NOT NULL CHECK (cycle_months IN (12, 36)),
			unit TEXT NOT NULL CHECK (unit IN ('workdays', 'calendar', 'weekend')),
			restricted_rule TEXT NOT NULL DEFAULT 'min' CHECK (restricted_rule IN ('any', 'min', 'never')),
			backdate INTEGER NOT NULL DEFAULT 0 CHECK (backdate IN (0, 1)),
			requires_note INTEGER NOT NULL DEFAULT 0 CHECK (requires_note IN (0, 1)),
			active INTEGER NOT NULL DEFAULT 1 CHECK (active IN (0, 1)),
			sort INTEGER NOT NULL DEFAULT 0,
			coverage_rule TEXT NOT NULL DEFAULT 'limit' CHECK (coverage_rule IN ('limit', 'count'))
		) STRICT`,
		`INSERT INTO leave_types (code, name, cycle_months, unit, backdate) VALUES ('annual', 'Annual', 12, 'workdays', 0), ('sick', 'Sick', 36, 'workdays', 1)`,
		`ALTER TABLE settings ADD COLUMN sick_backdate_days INTEGER NOT NULL DEFAULT 14`,
	} {
		if _, err := fresh.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	fresh.Close()

	for pass := 1; pass <= 2; pass++ {
		d, err := Open(path)
		if err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
		rows, err := d.Query(`SELECT code, backdate_days FROM leave_types ORDER BY code`)
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]int{}
		for rows.Next() {
			var c string
			var n int
			rows.Scan(&c, &n)
			got[c] = n
		}
		rows.Close()
		if len(got) != 2 || got["annual"] != 90 || got["sick"] != 90 {
			t.Errorf("pass %d: backdate_days = %v, want 90 for both", pass, got)
		}
		for _, c := range [][2]string{{"leave_types", "backdate"}, {"settings", "sick_backdate_days"}} {
			var n int
			d.QueryRow(`SELECT count(*) FROM pragma_table_info(?) WHERE name = ?`, c[0], c[1]).Scan(&n)
			if n != 0 {
				t.Errorf("pass %d: %s.%s was not dropped", pass, c[0], c[1])
			}
		}
		var ddl string
		d.QueryRow(`SELECT sql FROM sqlite_master WHERE name = 'leave_types'`).Scan(&ddl)
		if !strings.HasSuffix(strings.TrimSpace(strings.ToUpper(ddl)), ") STRICT") {
			t.Errorf("pass %d: leave_types lost STRICT", pass)
		}
		// The CHECK came with the column.
		if _, err := d.Exec(`UPDATE leave_types SET backdate_days = 366`); err == nil {
			t.Errorf("pass %d: backdate_days 366 accepted", pass)
		}
		d.Close()
	}
}
