package main

import (
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/17xande-dev/leave/internal/db"
)

// Exercise the actual entrypoint: a typo must exit unsuccessfully without
// creating a database, while a rehearsal copy migrates without server config.
func TestMigrateCommand(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "leave")
	if out, err := exec.Command("go", "build", "-o", binary, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	for _, existing := range []bool{false, true} {
		name := "missing"
		if existing {
			name = "existing"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "rehearsal.db")
			if existing {
				d, err := db.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				// Recreate the pre-0002 schema to exercise a real upgrade.
				if _, err := d.Exec(`BEGIN; ALTER TABLE requests DROP COLUMN halves_set_at; ALTER TABLE requests DROP COLUMN halves_set_by_id; ALTER TABLE settings DROP COLUMN half_days_enabled; DELETE FROM goose_db_version WHERE version_id>=2; COMMIT;`); err != nil {
					d.Close()
					t.Fatal(err)
				}
				d.Close()
			}
			cmd := exec.Command(binary, "-migrate")
			// A relative DB_PATH must be reported as its absolute location.
			cmd.Dir = filepath.Dir(path)
			t.Setenv("DB_PATH", filepath.Base(path))
			out, err := cmd.CombinedOutput()
			if !existing {
				var exit *exec.ExitError
				if !errors.As(err, &exit) || exit.ExitCode() != 1 ||
					!strings.Contains(string(out), "file must already exist") || !strings.Contains(string(out), path) {
					t.Fatalf("missing DB: err=%v output=%s", err, out)
				}
				entries, err := os.ReadDir(filepath.Dir(path))
				if err != nil || len(entries) != 0 {
					t.Fatalf("missing DB created files: %v, err=%v", entries, err)
				}
				return
			}
			if err != nil || !strings.Contains(string(out), "migrating "+path+": 1 -> 3\n") {
				t.Fatalf("existing DB: err=%v output=%s", err, out)
			}
			d, err := sql.Open("sqlite3", path)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			var version, enabled int
			if err := d.QueryRow(`SELECT max(version_id) FROM goose_db_version WHERE is_applied=1`).Scan(&version); err != nil || version != 3 {
				t.Fatalf("version=%d err=%v", version, err)
			}
			if err := d.QueryRow(`SELECT half_days_enabled FROM settings WHERE id=1`).Scan(&enabled); err != nil || enabled != 0 {
				t.Fatalf("half_days_enabled=%d err=%v", enabled, err)
			}
		})
	}
}
