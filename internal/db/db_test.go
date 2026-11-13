package db

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Check what SQLite actually recorded, including goose's version table.
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
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	// Count application tables and migration tracking so the check cannot pass vacuously.
	if n != 21 {
		t.Fatalf("found %d tables, want 20 application tables and goose_db_version", n)
	}
}

func TestMigrationStatus(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "fresh.db")
	var out bytes.Buffer
	if err := Status(context.Background(), path, &out); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "0001 pending 0001_baseline.sql\n0002 pending 0002_half_days_enabled.sql\n0003 pending 0003_halves_audit.sql\n" {
		t.Fatalf("before Open: %q", got)
	}
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	d.Close()
	out.Reset()
	if err := Status(context.Background(), path, &out); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "0001 applied 0001_baseline.sql\n0002 applied 0002_half_days_enabled.sql\n0003 applied 0003_halves_audit.sql\n" {
		t.Fatalf("after Open: %q", got)
	}
}

func TestNewerMigrationVersionRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`INSERT INTO goose_db_version (version_id, is_applied) VALUES (4, 1)`); err != nil {
		t.Fatal(err)
	}
	d.Close()
	if d, err := Open(path); err == nil {
		d.Close()
		t.Fatal("newer schema accepted")
	} else if !strings.Contains(err.Error(), "newer than this binary") {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := Status(context.Background(), path, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "newer than this binary") {
		t.Fatalf("status on newer schema: %v", err)
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
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "predates migrations") {
		t.Fatalf("old database: err = %v", err)
	}
}

// A partial unversioned requests schema must fail before migration runs.
func TestOldDatabaseMissingNewerColumnRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	old.Exec(`CREATE TABLE requests (id INTEGER PRIMARY KEY, status TEXT, shutdown_part_id INTEGER) STRICT`)
	old.Close()
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "predates migrations") {
		t.Fatalf("old database: err = %v", err)
	}
}

// A users table from before users.sex must not be silently adopted.
func TestOldDatabaseWithoutUsersSexRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	old.Exec(`CREATE TABLE users (id INTEGER PRIMARY KEY, ext_id TEXT, long_weekend INTEGER) STRICT`)
	old.Close()
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "predates migrations") {
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
	old.Exec(`CREATE TABLE requests (id INTEGER PRIMARY KEY, days_x100 INTEGER, shutdown_part_id INTEGER, escalate INTEGER,
		allow_negative INTEGER, recorded_by_id INTEGER, half_start INTEGER, half_end INTEGER, status TEXT CHECK (status IN ('draft', 'pending'))) STRICT`)
	old.Close()
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "predates migrations") {
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
	old.Exec(`CREATE TABLE requests (id INTEGER PRIMARY KEY, days_x100 INTEGER, shutdown_part_id INTEGER, escalate INTEGER,
		allow_negative INTEGER, recorded_by_id INTEGER, half_start INTEGER, half_end INTEGER, status TEXT CHECK (status IN ('draft', 'pending', 'awaiting_hr'))) STRICT`)
	old.Exec(`CREATE TABLE coverage_rules (department TEXT PRIMARY KEY) STRICT`)
	old.Exec(`CREATE TABLE request_steps (id INTEGER PRIMARY KEY) STRICT`)
	old.Close()
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "predates migrations") {
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
	old.Exec(`CREATE TABLE requests (id INTEGER PRIMARY KEY, days_x100 INTEGER, shutdown_part_id INTEGER, escalate INTEGER,
		allow_negative INTEGER, recorded_by_id INTEGER, half_start INTEGER, half_end INTEGER, status TEXT CHECK (status IN ('draft', 'pending', 'awaiting_hr'))) STRICT`)
	old.Exec(`CREATE TABLE coverage_rules (department TEXT PRIMARY KEY) STRICT`)
	old.Exec(`CREATE TABLE request_steps (id INTEGER PRIMARY KEY) STRICT`)
	old.Exec(`CREATE TABLE settings (id INTEGER PRIMARY KEY, team_default_percent INTEGER) STRICT`)
	old.Close()
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "predates migrations") {
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
	old.Exec(`CREATE TABLE requests (id INTEGER PRIMARY KEY, days_x100 INTEGER, shutdown_part_id INTEGER, escalate INTEGER,
		allow_negative INTEGER, recorded_by_id INTEGER, half_start INTEGER, half_end INTEGER, status TEXT CHECK (status IN ('draft', 'pending', 'awaiting_hr'))) STRICT`)
	old.Exec(`CREATE TABLE settings (id INTEGER PRIMARY KEY) STRICT`)
	old.Close()
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "predates migrations") {
		t.Fatalf("old database without the settings column: err = %v", err)
	}

	path = filepath.Join(t.TempDir(), "old2.db")
	old, _ = sql.Open("sqlite3", "file:"+path)
	for _, q := range []string{
		`CREATE TABLE requests (id INTEGER PRIMARY KEY, days_x100 INTEGER, shutdown_part_id INTEGER, escalate INTEGER,
			allow_negative INTEGER, recorded_by_id INTEGER, half_start INTEGER, half_end INTEGER, status TEXT CHECK (status IN ('draft', 'pending', 'awaiting_hr'))) STRICT`,
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
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "predates migrations") {
		t.Fatalf("old database without the team tables: err = %v", err)
	}
}

// Historical pre-goose schemas are refused regardless of which feature is missing.
func TestOldBackdatingAndRecordedShapesRefused(t *testing.T) {
	for name, tc := range map[string]struct{ mutate, want string }{
		"no backdate_days":       {`ALTER TABLE leave_types DROP COLUMN backdate_days`, "leave_types.backdate_days"},
		"no recorded_by_id":      {`ALTER TABLE requests DROP COLUMN recorded_by_id`, "requests.recorded_by_id"},
		"old backdate flag":      {`ALTER TABLE leave_types ADD COLUMN backdate INTEGER`, "leave_types.backdate"},
		"old sick_backdate_days": {`ALTER TABLE settings ADD COLUMN sick_backdate_days INTEGER`, "settings.sick_backdate_days"},
		// Whole-day columns from before every figure became hundredths of a day:
		// a database from that build would read 15 as 0.15 of a day.
		"old requests.days":              {`ALTER TABLE requests ADD COLUMN days INTEGER`, "requests.days"},
		"old balance_adjustments.days":   {`ALTER TABLE balance_adjustments ADD COLUMN days INTEGER`, "balance_adjustments.days"},
		"old leave_types.allowance_days": {`ALTER TABLE leave_types ADD COLUMN allowance_days INTEGER`, "leave_types.allowance_days"},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "old.db")
			d, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := d.Exec(`DROP TABLE goose_db_version`); err != nil {
				t.Fatal(err)
			}
			if _, err := d.Exec(tc.mutate); err != nil {
				t.Fatal(err)
			}
			d.Close()
			_, err = Open(path)
			if err == nil || !strings.Contains(err.Error(), "predates migrations") || !strings.Contains(err.Error(), "make recreate-db") {
				t.Fatalf("err = %v, want an unversioned database refusal for %s", err, tc.want)
			}
		})
	}
}

// Reopening must not rerun the baseline or append another applied version.
func TestFreshDatabaseOpensTwice(t *testing.T) {
	path := filepath.Join(t.TempDir(), "fresh.db")
	for i := 0; i < 2; i++ {
		d, err := Open(path)
		if err != nil {
			t.Fatalf("open %d: %v", i+1, err)
		}

		var version, applied, rows int
		if err := d.QueryRow(`SELECT max(version_id), sum(CASE WHEN version_id=1 AND is_applied=1 THEN 1 ELSE 0 END), count(*) FROM goose_db_version`).Scan(&version, &applied, &rows); err != nil {
			t.Fatal(err)
		}
		if version != 3 || applied != 1 || rows != 4 {
			t.Fatalf("version=%d applied=%d rows=%d", version, applied, rows)
		}
		d.Close()
	}
}

func TestVersionTableWithoutZeroRowOpens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "no-zero.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`DELETE FROM goose_db_version WHERE version_id=0`); err != nil {
		t.Fatal(err)
	}
	d.Close()
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	var count, version, applied int
	if err := d.QueryRow(`SELECT count(*), max(version_id), sum(is_applied) FROM goose_db_version`).Scan(&count, &version, &applied); err != nil {
		t.Fatal(err)
	}
	if count != 3 || version != 3 || applied != 3 {
		t.Fatalf("version table changed: count=%d version=%d applied=%d", count, version, applied)
	}
}

// Exercise the operator's exact SQL, including production's appended users.sex,
// and compare every application row before adoption and after the first boot.
func TestAdoptBaselinePreservesData(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unversioned.db")
	d, p, err := openProvider(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.UpTo(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, q := range []string{
		`DROP TABLE goose_db_version`,
		`ALTER TABLE users DROP COLUMN sex`,
		`ALTER TABLE users ADD COLUMN sex TEXT NOT NULL DEFAULT '' CHECK (sex IN ('', 'f', 'm'))`,
		`INSERT INTO users (id, ext_id, dn, username, name, sex, synced_at) VALUES
		 (1, 'person-1', 'dn1', 'staff', 'Staff Member', 'f', '2026-11-12T08:00:00Z'),
		 (2, 'person-2', 'dn2', 'admin', 'Administrator', 'm', '2026-11-12T08:00:00Z')`,
		`INSERT INTO leave_types (id, code, name, allowance_x100, cycle_months, unit) VALUES (1, 'annual', 'Annual', 1500, 12, 'workdays')`,
		`INSERT INTO requests (id, user_id, type_id, kind, start_date, end_date, days_x100, half_start, cycle_start, status, created_at)
		 VALUES (1, 1, 1, 'leave', '2026-11-16', '2026-11-16', 50, 1, '2026-01-01', 'pending', '2026-11-12T08:00:00Z')`,
		`INSERT INTO sessions (token_hash, user_id, created_at, expires_at) VALUES ('session-hash', 1, '2026-11-12T08:00:00Z', '2026-11-13T08:00:00Z')`,
		`UPDATE settings SET team_default_percent=40, updated_by_id=2, updated_at='2026-11-12T08:00:00Z' WHERE id=1`,
		`INSERT INTO outbox (kind, payload, next_attempt_at, created_at) VALUES ('email', '{"to":"staff@example.org"}', '2026-11-12T08:00:00Z', '2026-11-12T08:00:00Z')`,
	} {
		if _, err := d.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	var lastColumn string
	if err := d.QueryRow(`SELECT name FROM pragma_table_info('users') ORDER BY cid DESC LIMIT 1`).Scan(&lastColumn); err != nil || lastColumn != "sex" {
		t.Fatalf("production-shaped users last column=%q, err=%v", lastColumn, err)
	}
	before := applicationRows(t, d)
	if refused, err := Open(path); err == nil {
		refused.Close()
		t.Fatal("unversioned database accepted before adoption")
	} else if !strings.Contains(err.Error(), "predates migrations") {
		t.Fatal(err)
	}
	adoptionSQL, err := os.ReadFile("testdata/adopt_baseline.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(string(adoptionSQL)); err != nil {
		t.Fatal(err)
	}
	if err := d.Close(); err != nil {
		t.Fatal(err)
	}
	d, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if after := applicationRows(t, d); !reflect.DeepEqual(before, after) {
		t.Fatalf("application rows changed during adoption/boot:\nbefore: %v\nafter: %v", before, after)
	}
	assertHalfDaysOff(t, d)
	var out bytes.Buffer
	if err := Status(context.Background(), path, &out); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != "0001 applied 0001_baseline.sql\n0002 applied 0002_half_days_enabled.sql\n0003 applied 0003_halves_audit.sql\n" {
		t.Fatalf("after adoption: %q", got)
	}
}

func applicationRows(t *testing.T, d *sql.DB) map[string][32]byte {
	t.Helper()
	tables, err := d.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name<>'goose_db_version' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for tables.Next() {
		var name string
		if err := tables.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := tables.Err(); err != nil {
		t.Fatal(err)
	}
	tables.Close()
	if len(names) != 20 {
		t.Fatalf("found %d application tables, want 20", len(names))
	}
	snapshot := make(map[string][32]byte, len(names))
	for _, name := range names {
		columnsSQL := "*"
		if name == "requests" {
			// Audit columns are nullable additions, checked separately in the v3 test.
			columnsSQL = "id,user_id,type_id,kind,cancels_request_id,start_date,end_date,days_x100,half_start,half_end,cycle_start,note,status,approver_id,decided_by_id,decided_at,decision_note,calendar_event_id,created_at,shutdown_part_id,escalate,allow_negative,recorded_by_id,removed_by_id,removed_at"
		}
		if name == "settings" {
			// Compare every baseline field across adoption plus the v2 ADD COLUMN;
			// the new field's default is asserted separately.
			columnsSQL = "id,work_days,restricted_day,restricted_min_days,final_approver_id,sick_cycle_start_year,team_default_percent,updated_by_id,updated_at"
		}
		rows, err := d.Query(`SELECT ` + columnsSQL + ` FROM "` + strings.ReplaceAll(name, `"`, `""`) + `" ORDER BY rowid`)
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(err)
		}
		var data bytes.Buffer
		for rows.Next() {
			values := make([]any, len(columns))
			pointers := make([]any, len(columns))
			for i := range values {
				pointers[i] = &values[i]
			}
			if err := rows.Scan(pointers...); err != nil {
				t.Fatal(err)
			}
			if err := json.NewEncoder(&data).Encode(values); err != nil {
				t.Fatal(err)
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		snapshot[name] = sha256.Sum256(data.Bytes())
	}
	return snapshot
}

// A requests table from before half-day requests has every other column; it is
// refused as unversioned rather than failing at the first insert.
func TestOldDatabaseWithoutHalfDaysRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	old.Exec(`CREATE TABLE requests (id INTEGER PRIMARY KEY, days_x100 INTEGER, shutdown_part_id INTEGER, escalate INTEGER,
		allow_negative INTEGER, recorded_by_id INTEGER, status TEXT) STRICT`)
	old.Close()
	if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "predates migrations") {
		t.Fatalf("old database: err = %v", err)
	}
}

// half_end on a one-day request is refused by the schema itself, and the flags
// accept only 0 and 1, so no code path can store a nonsense half day.
func TestHalfDayColumnsChecked(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for _, q := range []string{
		`INSERT INTO users (id, ext_id, dn, username, name, synced_at) VALUES (1, 'x', 'dn', 'x', 'X', '')`,
		`INSERT INTO leave_types (id, code, name, cycle_months, unit) VALUES (1, 'annual', 'Annual', 12, 'workdays')`,
	} {
		if _, err := d.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	ins := func(start, end string, hs, he int) error {
		_, err := d.Exec(`INSERT INTO requests (user_id, type_id, kind, start_date, end_date, days_x100, half_start, half_end,
			cycle_start, status, created_at) VALUES (1, 1, 'leave', ?, ?, 50, ?, ?, '2026-01-01', 'pending', '')`, start, end, hs, he)
		return err
	}
	if err := ins("2026-03-03", "2026-03-03", 1, 0); err != nil {
		t.Fatalf("half_start on one day: %v", err)
	}
	if err := ins("2026-03-10", "2026-03-10", 0, 1); err == nil {
		t.Fatal("half_end on a one-day request was accepted")
	}
	if err := ins("2026-03-10", "2026-03-11", 0, 1); err != nil {
		t.Fatalf("half_end on two days: %v", err)
	}
	if err := ins("2026-03-17", "2026-03-18", 2, 0); err == nil {
		t.Fatal("half_start = 2 was accepted")
	}
}

// users.sex is checked by the schema itself, not only by users.SetSex: a bad
// value written by raw SQL is refused by SQLite with a constraint error.
func TestUsersSexColumnChecked(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "fresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := d.Exec(`INSERT INTO users (id, ext_id, dn, username, name, synced_at) VALUES (1, 'x', 'dn', 'x', 'X', '')`); err != nil {
		t.Fatal(err)
	}
	for _, ok := range []string{"", "f", "m"} {
		if _, err := d.Exec(`UPDATE users SET sex = ? WHERE id = 1`, ok); err != nil {
			t.Errorf("sex %q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"x", "M", "F", "male", " "} {
		_, err := d.Exec(`UPDATE users SET sex = ? WHERE id = 1`, bad)
		if err == nil || !strings.Contains(err.Error(), "CHECK constraint failed") {
			t.Errorf("sex %q: err = %v, want a CHECK constraint failure", bad, err)
		}
	}
	var got string
	if err := d.QueryRow(`SELECT sex FROM users WHERE id = 1`).Scan(&got); err != nil || got != "m" {
		t.Errorf("stored sex = %q, %v; a refused write must leave the last good value", got, err)
	}
}
