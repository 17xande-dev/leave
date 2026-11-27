package db

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const legacyOutboxRows = `SELECT id, kind, payload, attempts, next_attempt_at,
	last_error, done_at, failed_at, created_at, lease_until, claim_token FROM outbox ORDER BY id`

func TestOutboxAdoptionAppliesAppMigrationsFirst(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	d, _, err := openProvider(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	// Force the app's baseline to fail after its provider initialized goose.
	contractExec(t, d, `CREATE TABLE users (id INTEGER PRIMARY KEY) STRICT`)
	if opened, err := Open(path); err == nil {
		opened.Close()
		t.Fatal("app baseline accepted a conflicting table")
	}
	if got := contractRows(t, d, `SELECT name FROM sqlite_schema WHERE type = 'table' AND name GLOB 'outbox_*'`); len(got) != 0 {
		t.Errorf("module tables created before the app migration succeeded: %v", got)
	}
}

func TestOutboxAdoptionRefusesNewerModuleSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	contractExec(t, d, `INSERT INTO outbox_goose_version (version_id, is_applied) VALUES (2, 1)`)
	if opened, err := Open(path); err == nil {
		opened.Close()
		t.Fatal("startup accepted a newer module schema")
	} else if !strings.Contains(err.Error(), "outbox: migration 2 is newer") {
		t.Fatal(err)
	}
}

func TestOutboxAdoptionRehearsal(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	source := filepath.Join(dir, "legacy.db")
	copyPath := filepath.Join(dir, "rehearsal.db")
	conn, provider, err := openProvider(source)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if _, err := provider.Up(ctx); err != nil {
		t.Fatal(err)
	}
	contractExec(t, conn, `INSERT INTO outbox (id, kind, payload, attempts, next_attempt_at, last_error, done_at, failed_at, created_at) VALUES
		(4, 'calendar_create', '{"request_id":42,"delete_event_ids":["old"]}', 3, '2026-01-02T12:00:00Z', 'temporary', NULL, NULL, '2026-01-01T12:00:00Z'),
		(7, 'email', '{"To":["fixture@example.test"],"Subject":"fixture"}', 12, '2026-01-02T12:00:00Z', 'exhausted', NULL, '2026-01-03T12:00:00Z', '2026-01-01T12:00:00Z'),
		(8, 'email', '{}', 1, '2026-01-01T12:00:00Z', '', '2026-01-01T12:01:00Z', NULL, '2026-01-01T12:00:00Z'),
		(9, 'calendar_delete', '{"request_id":43}', 12, '2026-01-03T12:00:00Z', '', NULL, NULL, '2026-01-01T12:00:00Z')`)
	contractExec(t, conn, `UPDATE outbox SET lease_until = 123, claim_token = randomblob(16) WHERE id = 9`)
	oldRows := contractRows(t, conn, legacyOutboxRows)
	contractExec(t, conn, `VACUUM INTO ?`, copyPath)

	var versionRows [][]any
	for pass := range 2 {
		t.Run([]string{"operator migration", "server startup"}[pass], func(t *testing.T) {
			open := Open
			if pass == 0 {
				var status strings.Builder
				if err := Migrate(ctx, copyPath, &status); err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(status.String(), "11 -> 11\n") {
					t.Fatalf("app migration changed the app version: %s", &status)
				}
				open = func(path string) (*sql.DB, error) {
					return sql.Open("sqlite", path)
				}
			}
			d, err := open(copyPath)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			// A second startup must neither duplicate nor overwrite jobs that
			// the first worker has since processed.
			want := [][]any{
				{"legacy:outbox:4", "calendar_create", "request:42", `{"request_id":42,"delete_event_ids":["old"]}`, "pending", int64(3), int64(12), "2026-01-02T12:00:00.000000Z", "temporary", nil, "2026-01-01T12:00:00.000000Z"},
				{"legacy:outbox:7", "email", "", `{"To":["fixture@example.test"],"Subject":"fixture"}`, "dead", int64(12), int64(12), "2026-01-02T12:00:00.000000Z", "exhausted", "2026-01-03T12:00:00.000000Z", "2026-01-01T12:00:00.000000Z"},
				{"legacy:outbox:9", "calendar_delete", "request:43", `{"request_id":43}`, "pending", int64(12), int64(12), "2026-01-03T12:00:00.000000Z", "", nil, "2026-01-01T12:00:00.000000Z"},
			}
			if pass == 1 {
				want[0][3], want[0][4], want[0][8], want[0][9] = "", "done", "", "2026-01-03T12:00:00.000000Z"
			}
			got := contractRows(t, d, `SELECT unique_key, kind, subject, CAST(payload AS TEXT),
				state, attempts, max_attempts, run_at, last_error, finished_at, created_at
				FROM outbox_jobs ORDER BY id`)
			if !reflect.DeepEqual(got, want) {
				t.Errorf("imported rows = %v; want %v", got, want)
			}
			if !reflect.DeepEqual(oldRows, contractRows(t, d, legacyOutboxRows)) {
				t.Error("migration changed the legacy table")
			}
			versions := contractRows(t, d, `SELECT version_id FROM outbox_goose_version WHERE is_applied = 1 ORDER BY id`)
			if pass == 0 {
				if !reflect.DeepEqual(versions, [][]any{{int64(0)}, {int64(1)}}) {
					t.Fatalf("module migration versions = %v; want 0 and 1", versions)
				}
				versionRows = versions
				contractExec(t, d, `UPDATE outbox_jobs SET state = 'done', payload = x'', last_error = '',
					finished_at = '2026-01-03T12:00:00.000000Z' WHERE unique_key = 'legacy:outbox:4'`)
			} else if !reflect.DeepEqual(versionRows, versions) {
				t.Fatalf("module version history changed: %v -> %v", versionRows, versions)
			}
		})
	}
}

// rollbackFixture is a database after cut-over: legacy rows 1 (imported, now
// done) and 2 (historic), plus native jobs of every unfinished state.
func rollbackFixture(t *testing.T) (*sql.DB, string, string) {
	path := filepath.Join(t.TempDir(), "test.db")
	d, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	contractExec(t, d, `INSERT INTO outbox (id, kind, payload, attempts, next_attempt_at, created_at, done_at) VALUES
		(1, 'email', '{"fixture":"imported"}', 2, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', NULL),
		(2, 'email', '{"fixture":"historic"}', 1, '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`)
	contractExec(t, d, `INSERT INTO outbox_jobs (kind, subject, unique_key, payload, state, attempts, max_attempts, run_at, created_at, updated_at, finished_at, last_error, lease_until, claim_token) VALUES
		('email', '', 'legacy:outbox:1', x'', 'done', 3, 12, '2026-01-01T00:00:00.000000Z', '2026-01-01T00:00:00.000000Z', '2026-01-02T00:00:00.000000Z', '2026-01-02T00:00:00.000000Z', '', NULL, NULL),
		('email', '', NULL, CAST('{"fixture":"pending"}' AS BLOB), 'pending', 2, 12, '2026-01-02T01:00:00.234567Z', '2026-01-02T00:00:00.123456Z', '2026-01-02T00:00:00.000000Z', NULL, 'temporary', NULL, NULL),
		('calendar_delete', 'request:42', NULL, CAST('{"request_id":42}' AS BLOB), 'dead', 12, 12, '2026-01-02T00:00:00.000000Z', '2026-01-02T00:00:00.000000Z', '2026-01-02T00:01:00.000000Z', '2026-01-02T00:01:00.000000Z', 'exhausted', NULL, NULL),
		('email', '', NULL, CAST('{"fixture":"running"}' AS BLOB), 'running', 1, 12, '2026-01-02T00:00:00.000000Z', '2026-01-02T00:00:00.000000Z', '2026-01-02T00:00:00.000000Z', NULL, '', '2026-01-02T00:02:00.000000Z', randomblob(16)),
		('email', '', NULL, x'', 'done', 1, 12, '2026-01-02T00:00:00.000000Z', '2026-01-02T00:00:00.000000Z', '2026-01-02T00:00:00.000000Z', '2026-01-02T00:01:00.000000Z', '', NULL, NULL)`)
	script, err := os.ReadFile("testdata/undo_outbox_adoption.sql")
	if err != nil {
		t.Fatal(err)
	}
	return d, path, string(script)
}

func TestOutboxAdoptionRollback(t *testing.T) {
	d, _, script := rollbackFixture(t)
	contractExec(t, d, script)
	want := [][]any{
		{int64(1), "email", `{"fixture":"imported"}`, int64(3), "2026-01-01T00:00:00Z", "", "2026-01-02T00:00:00Z", nil, "2026-01-01T00:00:00Z", nil, nil},
		{int64(2), "email", `{"fixture":"historic"}`, int64(1), "2026-01-01T00:00:00Z", "", "2026-01-01T00:00:00Z", nil, "2026-01-01T00:00:00Z", nil, nil},
		{int64(3), "email", `{"fixture":"pending"}`, int64(2), "2026-01-02T01:00:01Z", "temporary", nil, nil, "2026-01-02T00:00:00Z", nil, nil},
		{int64(4), "calendar_delete", `{"request_id":42}`, int64(12), "2026-01-02T00:00:00Z", "exhausted", nil, "2026-01-02T00:01:00Z", "2026-01-02T00:00:00Z", nil, nil},
		{int64(5), "email", `{"fixture":"running"}`, int64(1), "2026-01-02T00:02:00Z", "", nil, nil, "2026-01-02T00:00:00Z", nil, nil},
	}
	if got := contractRows(t, d, legacyOutboxRows); !reflect.DeepEqual(got, want) {
		t.Errorf("rollback rows = %v; want %v", got, want)
	}
	if got := contractRows(t, d, `SELECT count(*) FROM outbox_jobs`); !reflect.DeepEqual(got, [][]any{{int64(0)}}) {
		t.Errorf("rollback left module jobs behind: %v", got)
	}
	if got := contractRows(t, d, `SELECT max(version_id) FROM goose_db_version WHERE is_applied = 1`); !reflect.DeepEqual(got, [][]any{{int64(11)}}) {
		t.Errorf("rollback changed app version: %v", got)
	}
	if got := contractRows(t, d, `PRAGMA integrity_check`); !reflect.DeepEqual(got, [][]any{{"ok"}}) {
		t.Errorf("integrity check: %v", got)
	}
	if got := contractRows(t, d, `PRAGMA foreign_key_check`); len(got) != 0 {
		t.Errorf("foreign key check: %v", got)
	}
}

func TestOutboxAdoptionRollbackIsRerunnable(t *testing.T) {
	d, _, script := rollbackFixture(t)
	contractExec(t, d, script)
	first := contractRows(t, d, legacyOutboxRows)
	contractExec(t, d, script)
	if second := contractRows(t, d, legacyOutboxRows); !reflect.DeepEqual(first, second) {
		t.Errorf("second undo changed the legacy table (duplicate rows?):\n%v\n%v", first, second)
	}
}

func TestOutboxAdoptionRedeployAfterRollbackImportsOnce(t *testing.T) {
	d, path, script := rollbackFixture(t)
	contractExec(t, d, script)
	// The old binary meanwhile delivers legacy 3 (a native pending job copied by
	// the undo) and legacy 1 stays done; 4 (dead) and 5 (running) remain open.
	contractExec(t, d, `UPDATE outbox SET done_at = '2026-01-03T00:00:00Z' WHERE id = 3`)
	before := contractRows(t, d, `SELECT id, done_at FROM outbox ORDER BY id`)
	d.Close()
	for range 2 { // a restart must not import twice either
		again, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		got := contractRows(t, again, `SELECT unique_key, state FROM outbox_jobs ORDER BY id`)
		want := [][]any{{"legacy:outbox:4", "dead"}, {"legacy:outbox:5", "pending"}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("module jobs after redeploy = %v; want %v", got, want)
		}
		if after := contractRows(t, again, `SELECT id, done_at FROM outbox ORDER BY id`); !reflect.DeepEqual(before, after) {
			t.Errorf("redeploy changed done rows in the legacy table: %v -> %v", before, after)
		}
		again.Close()
	}
}
