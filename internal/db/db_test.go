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
