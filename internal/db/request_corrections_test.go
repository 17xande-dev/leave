package db

import (
	"context"
	"database/sql"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func tableGone(t *testing.T, d interface {
	QueryRow(string, ...any) *sql.Row
}) {
	t.Helper()
	var n int
	if err := d.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='request_corrections' OR name='idx_request_corrections_request'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("request_corrections objects remain: %d %v", n, err)
	}
}

func TestRequestCorrectionsMigration(t *testing.T) {
	ctx := context.Background()
	d, p, _ := contractFixture(t)
	if _, err := p.UpTo(ctx, 7); err != nil {
		t.Fatal(err)
	}
	before := contractHashes(t, d, false)
	schema := contractSchema(t, d)
	// Keep a v7 copy for the real command, rather than testing only goose.
	path := filepath.Join(t.TempDir(), "v7.db")
	contractExec(t, d, `VACUUM INTO ?`, path)
	up := func() {
		t.Helper()
		if _, err := p.UpTo(ctx, 8); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(before, contractHashes(t, d, false)) {
			t.Fatal("0008 changed existing application data")
		}
		if n := contractRows(t, d, `SELECT count(*) FROM request_corrections`); n[0][0] != int64(0) {
			t.Fatalf("new table not empty: %v", n)
		}
		if idx := contractRows(t, d, `SELECT name FROM pragma_index_list('request_corrections') WHERE name='idx_request_corrections_request'`); len(idx) != 1 {
			t.Fatalf("index: %v", idx)
		}
	}
	up()
	insert := `INSERT INTO request_corrections (request_id, admin_id, corrected_at, reason, before_json, after_json) VALUES (?, 1, 'now', ?, '{}', '{}')`
	if _, err := d.Exec(insert, 1, ""); err == nil || !strings.Contains(err.Error(), "CHECK constraint failed") {
		t.Fatalf("empty reason accepted: %v", err)
	}
	if _, err := d.Exec(insert, 9999, "why"); err == nil || !strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
		t.Fatalf("unknown request accepted: %v", err)
	}
	if _, err := d.Exec(`INSERT INTO request_corrections (request_id, admin_id, corrected_at, reason, before_json, after_json) VALUES (1, 9999, 'now', 'why', '{}', '{}')`); err == nil || !strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
		t.Fatalf("unknown admin accepted: %v", err)
	}
	contractExec(t, d, insert, 1, "a real reason")
	// Down drops the table (and the history with it); Up brings it back empty.
	if _, err := p.Down(ctx); err != nil {
		t.Fatal(err)
	}
	tableGone(t, d)
	down := contractHashes(t, d, false)
	if !reflect.DeepEqual(before, down) || !reflect.DeepEqual(schema, contractSchema(t, d)) {
		t.Fatal("Down did not restore the v7 schema and data")
	}
	up()
	script, err := os.ReadFile("testdata/undo_0008.sql")
	if err != nil {
		t.Fatal(err)
	}
	contractExec(t, d, string(script))
	if !reflect.DeepEqual(down, contractHashes(t, d, false)) || !reflect.DeepEqual(schema, contractSchema(t, d)) {
		t.Fatal("undo_0008 differs from Down")
	}
	tableGone(t, d)
	if version, err := p.GetDBVersion(ctx); err != nil || version != 7 {
		t.Fatalf("undo version=%d: %v", version, err)
	}
	up()
	binary := filepath.Join(t.TempDir(), "leave")
	if out, err := exec.Command("go", "build", "-o", binary, "../..").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	for _, want := range []string{"7 -> 11", "11 -> 11"} {
		cmd := exec.Command(binary, "-migrate")
		cmd.Env = append(os.Environ(), "DB_PATH="+path)
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), ": "+want+"\n") || !strings.Contains(string(out), "0008 applied 0008_request_corrections.sql") {
			t.Fatalf("migrate %s: %v\n%s", want, err, out)
		}
	}
}
