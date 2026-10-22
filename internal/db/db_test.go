package db

import (
	"database/sql"
	"os"
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

// The previous build's leave_types and settings tables, verbatim from commit
// 6dd59d8's schema, so the migration is tested against the real old text
// (including settings.sick_backdate_days and its CHECK) and not a paraphrase.
const oldLeaveTypes = `CREATE TABLE IF NOT EXISTS leave_types (
	id              INTEGER PRIMARY KEY,
	code            TEXT NOT NULL UNIQUE,
	name            TEXT NOT NULL,
	-- NULL means unlimited (unpaid leave): there is no balance to exhaust.
	allowance_days  INTEGER CHECK (allowance_days IS NULL OR allowance_days >= 0),
	cycle_months    INTEGER NOT NULL CHECK (cycle_months IN (12, 36)),
	-- workdays counts the organisation's working days (Admin -> Settings) that are
	-- not public holidays; calendar counts every day (maternity leave is four
	-- consecutive months, holidays included); weekend counts one per weekend
	-- off, which runs Saturday to Sunday, or Friday to Sunday for someone on
	-- long weekends.
	unit            TEXT NOT NULL CHECK (unit IN ('workdays', 'calendar', 'weekend')),
	-- What this type may do with the restricted day (the restricted day in Settings, e.g.
	-- Sunday): any -- no rule; min -- only inside a request of at least
	-- the settings minimum of working days; never -- not at all.
	restricted_rule TEXT NOT NULL DEFAULT 'min' CHECK (restricted_rule IN ('any', 'min', 'never')),
	-- Sick leave is applied for after the fact; nothing else may start in the past.
	backdate        INTEGER NOT NULL DEFAULT 0 CHECK (backdate IN (0, 1)),
	requires_note   INTEGER NOT NULL DEFAULT 0 CHECK (requires_note IN (0, 1)),
	active          INTEGER NOT NULL DEFAULT 1 CHECK (active IN (0, 1)),
	sort            INTEGER NOT NULL DEFAULT 0,
	-- How department coverage limits treat this type. limit: it counts as
	-- someone being away and can be blocked by the limit. count: it counts as
	-- away but is never blocked (nobody should be refused sick leave).
	coverage_rule   TEXT NOT NULL DEFAULT 'limit' CHECK (coverage_rule IN ('limit', 'count'))
) STRICT`

const oldSettings = `CREATE TABLE IF NOT EXISTS settings (
	id                    INTEGER PRIMARY KEY CHECK (id = 1),
	work_days             TEXT NOT NULL DEFAULT 'mon,tue,wed,thu,fri',
	restricted_day        TEXT NOT NULL DEFAULT '' CHECK (restricted_day IN ('', 'sun', 'mon', 'tue', 'wed', 'thu', 'fri', 'sat')),
	restricted_min_days   INTEGER NOT NULL DEFAULT 3 CHECK (restricted_min_days BETWEEN 1 AND 31),
	final_approver_id     INTEGER REFERENCES users(id),
	sick_backdate_days    INTEGER NOT NULL DEFAULT 14 CHECK (sick_backdate_days BETWEEN 0 AND 365),
	sick_cycle_start_year INTEGER NOT NULL DEFAULT 2025 CHECK (sick_cycle_start_year BETWEEN 2000 AND 2100),
	-- The default team limit, a percentage; 0 means a team with no rule of its
	-- own has no limit.
	team_default_percent  INTEGER NOT NULL DEFAULT 0 CHECK (team_default_percent BETWEEN 0 AND 100),
	updated_by_id         INTEGER REFERENCES users(id),
	updated_at            TEXT NOT NULL DEFAULT ''
) STRICT`

// oldDatabase builds a database as the previous build left it, with an
// annual type (flag 0) and a sick type (flag 1).
func oldDatabase(t *testing.T, path string) {
	t.Helper()
	fresh, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	for _, stmt := range []string{
		`DROP TABLE settings`, `DROP TABLE leave_types`, oldLeaveTypes, oldSettings,
		`INSERT INTO settings (id) VALUES (1)`,
		`INSERT INTO leave_types (code, name, cycle_months, unit, backdate) VALUES ('annual', 'Annual', 12, 'workdays', 0), ('sick', 'Sick', 36, 'workdays', 1)`,
	} {
		if _, err := fresh.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
}

func backups(t *testing.T, dir string) []string {
	t.Helper()
	m, err := filepath.Glob(filepath.Join(dir, "*.pre-backdate-*.db"))
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// A database from before per-type backdating (a 0/1 flag on each type and a
// global sick setting) opens, keeps its rows and its STRICT tables, gives every
// existing type the 92-day policy whatever the old flag said, and loses both
// old columns. Opening it a second time changes nothing, and only the first
// open takes a backup.
func TestMigrateBackdating(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "old.db")
	oldDatabase(t, path)
	if n := len(backups(t, dir)); n != 0 {
		t.Fatalf("%d backups before any migration", n)
	}

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
		if len(got) != 2 || got["annual"] != 92 || got["sick"] != 92 {
			t.Errorf("pass %d: backdate_days = %v, want 92 for both", pass, got)
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
		// The CHECK came with the column: 92 is the cap.
		if _, err := d.Exec(`UPDATE leave_types SET backdate_days = 93`); err == nil {
			t.Errorf("pass %d: backdate_days 93 accepted", pass)
		}
		if _, err := d.Exec(`UPDATE leave_types SET backdate_days = 92`); err != nil {
			t.Errorf("pass %d: backdate_days 92 refused: %v", pass, err)
		}
		d.Close()
	}

	// One backup, taken before the ALTER: it still has the old columns and the
	// old flag values, so it is what a rollback restores.
	bs := backups(t, dir)
	if len(bs) != 1 {
		t.Fatalf("%d backups after two opens, want 1: %v", len(bs), bs)
	}
	b, err := sql.Open("sqlite3", "file:"+bs[0]+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	var flag int
	if err := b.QueryRow(`SELECT backdate FROM leave_types WHERE code = 'sick'`).Scan(&flag); err != nil || flag != 1 {
		t.Errorf("backup does not hold the old schema: flag=%d err=%v", flag, err)
	}
}

// A fresh database has nothing to migrate and so nothing to back up.
func TestFreshDatabaseTakesNoBackup(t *testing.T) {
	dir := t.TempDir()
	for range 2 {
		d, err := Open(filepath.Join(dir, "fresh.db"))
		if err != nil {
			t.Fatal(err)
		}
		d.Close()
	}
	if n := len(backups(t, dir)); n != 0 {
		t.Fatalf("%d backups for a fresh database", n)
	}
}

// If the backup cannot be written the open fails and the database is left as
// it was: no half-migrated state.
func TestMigrateBackdatingNeedsItsBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "old.db")
	oldDatabase(t, path)
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	if f, err := os.Create(filepath.Join(dir, "probe")); err == nil {
		f.Close()
		t.Skip("directory is writable despite chmod (running as root?)")
	}
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "back up") {
		t.Fatalf("err = %v, want a backup failure", err)
	}
	os.Chmod(dir, 0o700)
	d, err := sql.Open("sqlite3", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var n int
	d.QueryRow(`SELECT count(*) FROM pragma_table_info('leave_types') WHERE name = 'backdate'`).Scan(&n)
	if n != 1 {
		t.Error("the database was migrated without its backup")
	}
}

// oldRecordedDB builds a database as the previous build left it: today's schema
// with the three recorded-leave columns removed from requests, and one request
// in it that must survive the migration.
func oldRecordedDB(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	fresh, err := Open(filepath.Join(dir, "fresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	rows, err := fresh.Query(`SELECT name, sql FROM sqlite_master WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%'
		ORDER BY CASE type WHEN 'table' THEN 0 ELSE 1 END, rowid`)
	if err != nil {
		t.Fatal(err)
	}
	var ddl []string
	for rows.Next() {
		var name, q string
		if err := rows.Scan(&name, &q); err != nil {
			t.Fatal(err)
		}
		if name == "requests" {
			for _, col := range []string{"recorded_by_id", "removed_by_id", "removed_at"} {
				var kept []string
				for _, line := range strings.Split(q, "\n") {
					if !strings.Contains(line, col+" ") {
						kept = append(kept, line)
					}
				}
				q = strings.Join(kept, "\n")
			}
		}
		ddl = append(ddl, q)
	}
	rows.Close()
	fresh.Close()

	path := filepath.Join(dir, "old.db")
	old, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	for _, q := range ddl {
		if _, err := old.Exec(q); err != nil {
			t.Fatalf("%v\n%s", err, q)
		}
	}
	if _, err := old.Exec(`INSERT INTO users (id, ext_id, dn, username, name, synced_at) VALUES (1, 'a', 'a', 'a', 'A', '')`); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`INSERT INTO leave_types (id, code, name, cycle_months, unit) VALUES (1, 'annual', 'Annual', 12, 'workdays')`); err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`INSERT INTO requests (user_id, type_id, kind, start_date, end_date, days, cycle_start, status, created_at)
		VALUES (1, 1, 'leave', '2026-01-05', '2026-01-05', 1, '2026-01-01', 'approved', '')`); err != nil {
		t.Fatal(err)
	}
	return path
}

func requestColumns(t *testing.T, d *sql.DB) map[string]bool {
	t.Helper()
	rows, err := d.Query(`SELECT name FROM pragma_table_info('requests')`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var n string
		rows.Scan(&n)
		cols[n] = true
	}
	return cols
}

// A database from before "Record leave" gains its columns on open, keeps its
// rows, and a second open changes nothing.
func TestMigrateRecordedAddsColumnsOnce(t *testing.T) {
	path := oldRecordedDB(t)
	probe, _ := sql.Open("sqlite3", "file:"+path)
	if requestColumns(t, probe)["recorded_by_id"] {
		t.Fatal("setup: the old database already has the column")
	}
	probe.Close()

	for i := 0; i < 2; i++ {
		d, err := Open(path)
		if err != nil {
			t.Fatalf("open %d: %v", i+1, err)
		}
		cols := requestColumns(t, d)
		for _, c := range []string{"recorded_by_id", "removed_by_id", "removed_at"} {
			if !cols[c] {
				t.Errorf("open %d: no %s column", i+1, c)
			}
		}
		var n int
		var by sql.NullInt64
		if err := d.QueryRow(`SELECT count(*), max(recorded_by_id) FROM requests`).Scan(&n, &by); err != nil || n != 1 || by.Valid {
			t.Errorf("open %d: %d rows, recorded_by %v, %v", i+1, n, by, err)
		}
		// The foreign key is enforced on the added column too.
		if _, err := d.Exec(`UPDATE requests SET recorded_by_id = 999`); err == nil {
			t.Errorf("open %d: recorded_by_id accepted a missing user", i+1)
		}
		d.Close()
	}
	// Additive: no backup copy is made for it.
	if m, _ := filepath.Glob(path + ".pre-*"); len(m) != 0 {
		t.Errorf("backups = %v, want none", m)
	}
}

// A database as it was before BOTH the per-type backdating limit and recorded
// leave, built from the schema text exactly as commit 6dd59d8 shipped it
// (testdata/schema-6dd59d8.sql, extracted verbatim, not reconstructed from
// today's schema). Opening it runs both migrations in order, once, and leaves a
// database SQLite itself calls healthy: STRICT tables, no corruption, no
// dangling foreign key.
func TestMigrateFromBeforeBackdatingAndRecorded(t *testing.T) {
	schema, err := os.ReadFile("testdata/schema-6dd59d8.sql")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "old.db")
	old, err := sql.Open("sqlite3", "file:"+path+"?_foreign_keys=on")
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{string(schema),
		`INSERT INTO users (id, ext_id, dn, username, name, synced_at) VALUES
			(1, 'a', 'a', 'a', 'Ann', ''), (2, 'b', 'b', 'b', 'Bob', '')`,
		`INSERT INTO leave_types (id, code, name, cycle_months, unit, backdate) VALUES
			(1, 'annual', 'Annual', 12, 'workdays', 0), (2, 'sick', 'Sick', 36, 'workdays', 1)`,
		`INSERT INTO requests (user_id, type_id, kind, start_date, end_date, days, cycle_start, status, approver_id, created_at) VALUES
			(1, 1, 'leave', '2026-01-05', '2026-01-06', 2, '2026-01-01', 'approved', 2, ''),
			(1, 2, 'leave', '2026-02-02', '2026-02-02', 1, '2025-01-01', 'pending', 2, ''),
			(2, 1, 'leave', '2026-03-02', '2026-03-02', 1, '2026-01-01', 'draft', NULL, '')`,
	} {
		if _, err := old.Exec(q); err != nil {
			t.Fatalf("setup: %v\n%.200s", err, q)
		}
	}
	old.Close()

	for pass := 1; pass <= 2; pass++ {
		d, err := Open(path)
		if err != nil {
			t.Fatalf("open %d: %v", pass, err)
		}
		// Backdating migration ran: the old columns are gone, the new one is there.
		for _, c := range []struct {
			table, col string
			want       int
		}{
			{"leave_types", "backdate", 0}, {"settings", "sick_backdate_days", 0}, {"leave_types", "backdate_days", 1},
			// Recorded-leave migration ran: its three columns exist.
			{"requests", "recorded_by_id", 1}, {"requests", "removed_by_id", 1}, {"requests", "removed_at", 1},
		} {
			var n int
			d.QueryRow(`SELECT count(*) FROM pragma_table_info(?) WHERE name = ?`, c.table, c.col).Scan(&n)
			if n != c.want {
				t.Errorf("open %d: %s.%s present = %d, want %d", pass, c.table, c.col, n, c.want)
			}
		}
		var reqs int
		if err := d.QueryRow(`SELECT count(*) FROM requests WHERE recorded_by_id IS NULL`).Scan(&reqs); err != nil || reqs != 3 {
			t.Errorf("open %d: %d requests kept, want 3 (%v)", pass, reqs, err)
		}
		var annual, sick int
		d.QueryRow(`SELECT (SELECT backdate_days FROM leave_types WHERE code = 'annual'),
			(SELECT backdate_days FROM leave_types WHERE code = 'sick')`).Scan(&annual, &sick)
		if annual != 92 || sick != 92 {
			t.Errorf("open %d: backdate_days = %d, %d, want 92", pass, annual, sick)
		}
		// Every table, including the ones rebuilt or altered, is still STRICT.
		rows, err := d.Query(`SELECT name, sql FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`)
		if err != nil {
			t.Fatal(err)
		}
		tables := 0
		for rows.Next() {
			var name, ddl string
			rows.Scan(&name, &ddl)
			tables++
			if !strings.HasSuffix(strings.TrimSpace(strings.ToUpper(ddl)), "STRICT") {
				t.Errorf("open %d: %s is not STRICT", pass, name)
			}
		}
		rows.Close()
		if tables < 10 {
			t.Errorf("open %d: only %d tables", pass, tables)
		}
		var check string
		if err := d.QueryRow(`PRAGMA integrity_check`).Scan(&check); err != nil || check != "ok" {
			t.Errorf("open %d: integrity_check = %q, %v", pass, check, err)
		}
		fk, err := d.Query(`PRAGMA foreign_key_check`)
		if err != nil {
			t.Fatal(err)
		}
		if fk.Next() {
			t.Errorf("open %d: foreign_key_check reports a violation", pass)
		}
		fk.Close()
		d.Close()
	}
}
