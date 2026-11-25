package db

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/17xande-dev/sqlitekit/goosestrict"
)

func TestOutboxLeaseMigrationRoundTrip(t *testing.T) {
	ctx := context.Background()
	d, p, err := openProvider(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := p.UpTo(ctx, 10); err != nil {
		t.Fatal(err)
	}
	const oldInsert = `INSERT INTO outbox (id,kind,payload,next_attempt_at,created_at)
		VALUES (?, 'email', '{}', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`
	contractExec(t, d, oldInsert, 1)
	contractExec(t, d, `UPDATE outbox SET attempts = 3, last_error = 'fixture failure' WHERE id = 1`)
	const legacyColumns = `SELECT id, kind, payload, attempts, next_attempt_at, last_error,
		done_at, failed_at, created_at FROM outbox ORDER BY id`
	const ddl = `SELECT type, name, sql FROM sqlite_schema WHERE tbl_name = 'outbox' ORDER BY name`
	oldRows, oldShape := contractRows(t, d, legacyColumns), contractRows(t, d, ddl)
	oldMigrations := schema10Migrations(t)
	var newShape [][]any
	for pass := range 2 {
		if _, err := p.Up(ctx); err != nil {
			t.Fatal(err)
		}
		if v, err := p.GetDBVersion(ctx); err != nil || v != 11 {
			t.Fatalf("Up version = %d (%v), want 11", v, err)
		}
		if !reflect.DeepEqual(oldRows, contractRows(t, d, legacyColumns)) {
			t.Fatal("Up changed existing queue data")
		}
		shape := contractRows(t, d, ddl)
		if pass == 0 {
			newShape = shape
		} else if !reflect.DeepEqual(newShape, shape) {
			t.Fatal("second Up changed the migrated table shape")
		}
		if _, err := goosestrict.NewProvider(d, oldMigrations); err == nil || !strings.Contains(err.Error(), "written by a newer build") {
			t.Fatalf("old migration provider against v11: %v, want newer-build refusal", err)
		}
		contractExec(t, d, oldInsert, 2)
		if got := contractRows(t, d, `SELECT id, lease_until, claim_token FROM outbox ORDER BY id`); !reflect.DeepEqual(got, [][]any{
			{int64(1), nil, nil}, {int64(2), nil, nil},
		}) {
			t.Fatal("existing and legacy-inserted rows must start without a lease")
		}
		// An already-running old worker's query has no lease predicate. Its
		// SQL remains valid but cannot safely coexist with a new worker.
		contractExec(t, d, `UPDATE outbox SET lease_until = 123, claim_token = randomblob(16) WHERE id = 2`)
		if due := contractRows(t, d, `SELECT id FROM outbox WHERE done_at IS NULL AND failed_at IS NULL
			AND next_attempt_at <= '2026-01-01T00:00:00Z' ORDER BY id LIMIT 20`); len(due) != 2 {
			t.Fatal("legacy poll did not read the leased row")
		}
		contractExec(t, d, `UPDATE outbox SET done_at = '2026-01-01T00:00:00Z', attempts = attempts + 1, last_error = '' WHERE id = 2`)
		contractExec(t, d, `DELETE FROM outbox WHERE id = 2`)
		if pass == 0 {
			contractExec(t, d, `UPDATE outbox SET lease_until = 123, claim_token = randomblob(16) WHERE id = 1`)
			if _, err := p.Down(ctx); err != nil {
				t.Fatal(err)
			}
			if v, err := p.GetDBVersion(ctx); err != nil || v != 10 {
				t.Fatalf("Down version = %d (%v), want 10", v, err)
			}
			if !reflect.DeepEqual(oldShape, contractRows(t, d, ddl)) || !reflect.DeepEqual(oldRows, contractRows(t, d, legacyColumns)) {
				t.Fatal("Down did not restore the exact old schema and data")
			}
			if _, err := goosestrict.NewProvider(d, oldMigrations); err != nil {
				t.Fatalf("old migration provider after Down: %v", err)
			}
		}
	}
}

func TestOutboxLeaseOperatorUndo(t *testing.T) {
	ctx := context.Background()
	d, p, _ := contractFixture(t)
	if _, err := p.UpTo(ctx, 10); err != nil {
		t.Fatal(err)
	}
	contractExec(t, d, `INSERT INTO sessions (token_hash, user_id, created_at, expires_at, last_seen)
		VALUES ('fixture-token-hash', 1, '2026-01-01', '2026-02-01', '2026-01-02')`)
	contractExec(t, d, `INSERT INTO outbox (kind,payload,attempts,next_attempt_at,created_at,done_at,failed_at,last_error) VALUES
		('email','{}',1,'2026-01-01','2026-01-01','2026-01-01',NULL,''),
		('email','{}',12,'2026-01-01','2026-01-01',NULL,'2026-01-02','fixture dead'),
		('email','{}',3,'2026-01-03','2026-01-01',NULL,NULL,'fixture backoff'),
		('email','{}',0,'2026-01-01','2026-01-01',NULL,NULL,'')`)
	const schema = `SELECT type,name,tbl_name,sql FROM sqlite_schema ORDER BY type,name`
	const tables = `SELECT name,type,ncol,wr,strict FROM pragma_table_list ORDER BY schema,name`
	const indexes = `SELECT * FROM pragma_index_list('outbox') ORDER BY name`
	const versions = `SELECT * FROM goose_db_version ORDER BY id`
	oldSchema, oldTables, oldIndexes := contractRows(t, d, schema), contractRows(t, d, tables), contractRows(t, d, indexes)
	if got := contractRows(t, d, `SELECT strict FROM pragma_table_list WHERE name = 'outbox'`); !reflect.DeepEqual(got, [][]any{{int64(1)}}) {
		t.Fatalf("fixture outbox is not STRICT: %v", got)
	}
	if len(oldIndexes) == 0 {
		t.Fatal("fixture has no outbox indexes")
	}
	oldData, oldVersions := contractHashes(t, d, false), contractRows(t, d, versions)
	if _, err := p.Up(ctx); err != nil {
		t.Fatal(err)
	}
	if v, err := p.GetDBVersion(ctx); err != nil || v != 11 {
		t.Fatalf("Up version = %d (%v), want 11", v, err)
	}
	contractExec(t, d, `UPDATE outbox SET lease_until = 123, claim_token = randomblob(16) WHERE id = (SELECT max(id) FROM outbox)`)
	script, err := os.ReadFile("testdata/undo_0011.sql")
	if err != nil {
		t.Fatal(err)
	}
	contractExec(t, d, string(script))
	for _, check := range []struct {
		name, query string
		want        [][]any
	}{
		{"schema", schema, oldSchema},
		{"table shape and STRICT flags", tables, oldTables},
		{"outbox indexes", indexes, oldIndexes},
		{"goose rows", versions, oldVersions},
		{"integrity", `PRAGMA integrity_check`, [][]any{{"ok"}}},
		{"foreign keys", `PRAGMA foreign_key_check`, nil},
	} {
		if !reflect.DeepEqual(check.want, contractRows(t, d, check.query)) {
			t.Errorf("operator undo did not restore %s", check.name)
		}
	}
	if !reflect.DeepEqual(oldData, contractHashes(t, d, false)) {
		t.Error("operator undo changed application data")
	}
	old, err := goosestrict.NewProvider(d, schema10Migrations(t))
	if err != nil {
		t.Fatalf("schema-10 provider after undo: %v", err)
	}
	if v, err := old.GetDBVersion(ctx); err != nil || v != 10 {
		t.Fatalf("undo version = %d (%v), want 10", v, err)
	}
	if results, err := old.Up(ctx); err != nil || len(results) != 0 {
		t.Fatalf("schema-10 startup migration after undo: got %d migrations, %v; want no-op", len(results), err)
	}
}

func schema10Migrations(t *testing.T) fstest.MapFS {
	t.Helper()
	src := make(fstest.MapFS)
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == "0011_outbox_lease.sql" {
			continue
		}
		b, err := migrations.ReadFile("migrations/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		src[e.Name()] = &fstest.MapFile{Data: b}
	}
	return src
}
