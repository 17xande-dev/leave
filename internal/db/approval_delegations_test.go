package db

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestApprovalDelegationsMigration(t *testing.T) {
	ctx := context.Background()
	d, p, _ := contractFixture(t)
	if _, err := p.UpTo(ctx, 8); err != nil {
		t.Fatal(err)
	}
	// A second person and a decision step, so there is something for the new
	// columns to sit beside.
	contractExec(t, d, `INSERT INTO users(id,ext_id,dn,username,name,synced_at) VALUES(2,'b','b','b','B','original')`)
	contractExec(t, d, `INSERT INTO request_steps (request_id, approver_id, decision, note, decided_at) VALUES (1, 2, 'approved', 'before', 'original')`)

	// requests and request_steps gain a column, so they are compared by the
	// columns they had; every other table by the hash of all its columns.
	hashes := func() map[string][32]byte {
		h := contractHashes(t, d, false)
		delete(h, "requests")
		delete(h, "request_steps")
		return h
	}
	oldCols := func(table string) string {
		var cols []string
		for _, c := range contractRows(t, d, `SELECT name FROM pragma_table_info('`+table+`') ORDER BY cid`) {
			cols = append(cols, `"`+c[0].(string)+`"`)
		}
		return strings.Join(cols, ",")
	}
	reqCols, stepCols := oldCols("requests"), oldCols("request_steps")
	rows := func() [2][][]any {
		return [2][][]any{
			contractRows(t, d, `SELECT `+reqCols+` FROM requests ORDER BY id`),
			contractRows(t, d, `SELECT `+stepCols+` FROM request_steps ORDER BY id`),
		}
	}
	before, beforeRows, schema := hashes(), rows(), contractSchema(t, d)
	// Keep a v8 copy for the real command, rather than testing only goose.
	path := filepath.Join(t.TempDir(), "v8.db")
	contractExec(t, d, `VACUUM INTO ?`, path)

	count := func(q string) int64 { t.Helper(); return contractRows(t, d, q)[0][0].(int64) }
	up := func() {
		t.Helper()
		if _, err := p.UpTo(ctx, 9); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(before, hashes()) || !reflect.DeepEqual(beforeRows, rows()) {
			t.Fatal("0009 changed existing application data")
		}
		if n := count(`SELECT count(*) FROM approval_delegations`); n != 0 {
			t.Fatalf("new table not empty: %d", n)
		}
		if n := count(`SELECT count(*) FROM requests WHERE decided_for_id IS NOT NULL`) +
			count(`SELECT count(*) FROM request_steps WHERE on_behalf_of_id IS NOT NULL`); n != 0 {
			t.Fatalf("new columns not NULL: %d", n)
		}
		if n := count(`SELECT count(*) FROM pragma_index_list('approval_delegations') WHERE name IN ('idx_delegations_delegator','idx_delegations_delegate')`); n != 2 {
			t.Fatalf("indexes: %d", n)
		}
	}
	check8 := func(what string) {
		t.Helper()
		if n := count(`SELECT count(*) FROM sqlite_master WHERE name LIKE '%delegations%'`); n != 0 {
			t.Fatalf("%s: delegation objects remain: %d", what, n)
		}
		if n := count(`SELECT count(*) FROM pragma_table_info('requests') WHERE name='decided_for_id'`) +
			count(`SELECT count(*) FROM pragma_table_info('request_steps') WHERE name='on_behalf_of_id'`); n != 0 {
			t.Fatalf("%s: new columns remain: %d", what, n)
		}
		if !reflect.DeepEqual(schema, contractSchema(t, d)) || !reflect.DeepEqual(before, hashes()) || !reflect.DeepEqual(beforeRows, rows()) {
			t.Fatalf("%s did not restore the v8 schema and data", what)
		}
	}
	up()

	// The three CHECKs and the foreign keys.
	const ins = `INSERT INTO approval_delegations (delegator_id, delegate_id, start_date, end_date, created_by_id, created_at, revoked_by_id, revoked_at) VALUES (?, ?, ?, ?, ?, 'now', ?, ?)`
	for _, c := range []struct {
		name string
		args []any
		want string
	}{
		{"self", []any{1, 1, "2026-12-01", "2026-12-02", 1, nil, nil}, "CHECK constraint failed"},
		{"end before start", []any{1, 2, "2026-12-02", "2026-12-01", 1, nil, nil}, "CHECK constraint failed"},
		{"revoked by without at", []any{1, 2, "2026-12-01", "2026-12-02", 1, 1, nil}, "CHECK constraint failed"},
		{"revoked at without by", []any{1, 2, "2026-12-01", "2026-12-02", 1, nil, "now"}, "CHECK constraint failed"},
		{"unknown delegator", []any{9999, 2, "2026-12-01", "2026-12-02", 1, nil, nil}, "FOREIGN KEY constraint failed"},
		{"unknown delegate", []any{1, 9999, "2026-12-01", "2026-12-02", 1, nil, nil}, "FOREIGN KEY constraint failed"},
		{"unknown creator", []any{1, 2, "2026-12-01", "2026-12-02", 9999, nil, nil}, "FOREIGN KEY constraint failed"},
		{"unknown revoker", []any{1, 2, "2026-12-01", "2026-12-02", 1, 9999, "now"}, "FOREIGN KEY constraint failed"},
	} {
		if _, err := d.Exec(ins, c.args...); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s accepted or wrong error: %v", c.name, err)
		}
	}
	if _, err := d.Exec(`UPDATE requests SET decided_for_id = 9999 WHERE id = 1`); err == nil || !strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
		t.Errorf("unknown decided_for_id accepted: %v", err)
	}
	if _, err := d.Exec(`UPDATE request_steps SET on_behalf_of_id = 9999`); err == nil || !strings.Contains(err.Error(), "FOREIGN KEY constraint failed") {
		t.Errorf("unknown on_behalf_of_id accepted: %v", err)
	}
	contractExec(t, d, ins, 1, 2, "2026-12-01", "2026-12-02", 1, nil, nil)
	contractExec(t, d, ins, 1, 2, "2026-12-10", "2026-12-12", 2, 1, "now")

	// An old binary: the INSERTs it knows name explicit columns (Submit's list
	// as of 0008, logStep's), so the new columns stay NULL.
	contractExec(t, d, `INSERT INTO requests (user_id, type_id, kind, start_date, end_date, days_x100,
		half_start, half_end, cycle_start, note, status, approver_id, escalate, created_at)
		VALUES (1, 1, 'leave', '2026-12-01', '2026-12-01', 100, 0, 0, '2026-01-01', '', 'pending', NULL, 0, 'old binary')`)
	contractExec(t, d, `INSERT INTO request_steps (request_id, approver_id, decision, note, decided_at) VALUES (1, 2, 'approved', '', 'old binary')`)
	if n := count(`SELECT count(*) FROM requests WHERE decided_for_id IS NULL AND created_at = 'old binary'`); n != 1 {
		t.Fatalf("old-binary request insert: %d", n)
	}
	if n := count(`SELECT count(*) FROM request_steps WHERE on_behalf_of_id IS NULL AND decided_at = 'old binary'`); n != 1 {
		t.Fatalf("old-binary step insert: %d", n)
	}
	contractExec(t, d, `DELETE FROM request_ranges WHERE request_id NOT IN (SELECT id FROM requests)`)
	contractExec(t, d, `DELETE FROM requests WHERE created_at = 'old binary'`)
	contractExec(t, d, `DELETE FROM request_steps WHERE decided_at = 'old binary'`)

	// Down discards the delegations and the on-behalf audit, nothing else.
	contractExec(t, d, `UPDATE requests SET decided_for_id = 1 WHERE id = 1`)
	contractExec(t, d, `UPDATE request_steps SET on_behalf_of_id = 1`)
	if _, err := p.Down(ctx); err != nil {
		t.Fatal(err)
	}
	check8("Down")
	up()
	script, err := os.ReadFile("testdata/undo_0009.sql")
	if err != nil {
		t.Fatal(err)
	}
	contractExec(t, d, string(script))
	check8("undo_0009")
	if version, err := p.GetDBVersion(ctx); err != nil || version != 8 {
		t.Fatalf("undo version=%d: %v", version, err)
	}
	// The script is Down, statement for statement, plus the version row.
	src, err := migrations.ReadFile("migrations/0009_approval_delegations.sql")
	if err != nil {
		t.Fatal(err)
	}
	_, down, _ := strings.Cut(string(src), "-- +goose Down\n")
	body := strings.TrimSuffix(strings.TrimPrefix(string(script), "BEGIN IMMEDIATE;\n"), "DELETE FROM goose_db_version WHERE version_id = 9;\nCOMMIT;\n")
	if strings.TrimSpace(body) != strings.TrimSpace(down) {
		t.Fatalf("undo_0009 differs from Down:\n%s\nvs\n%s", body, down)
	}
	up()

	binary := filepath.Join(t.TempDir(), "leave")
	if out, err := exec.Command("go", "build", "-o", binary, "../..").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	for _, want := range []string{"8 -> 10", "10 -> 10"} {
		cmd := exec.Command(binary, "-migrate")
		cmd.Env = append(os.Environ(), "DB_PATH="+path)
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), ": "+want+"\n") || !strings.Contains(string(out), "0009 applied 0009_approval_delegations.sql") {
			t.Fatalf("migrate %s: %v\n%s", want, err, out)
		}
	}
}
