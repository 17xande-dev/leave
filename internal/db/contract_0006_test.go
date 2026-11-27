package db

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"
)

func contractExec(t *testing.T, d *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := d.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func contractFixture(t *testing.T) (*sql.DB, *goose.Provider, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "v5.db")
	d, p, err := openProvider(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	ctx := context.Background()
	if _, err := p.UpTo(ctx, 1); err != nil {
		t.Fatal(err)
	}
	contractExec(t, d, `DROP TABLE goose_db_version`)
	contractExec(t, d, `ALTER TABLE users DROP COLUMN sex`)
	contractExec(t, d, `ALTER TABLE users ADD COLUMN sex TEXT NOT NULL DEFAULT '' CHECK (sex IN ('','f','m'))`)
	adopt, err := os.ReadFile("testdata/adopt_baseline.sql")
	if err != nil {
		t.Fatal(err)
	}
	contractExec(t, d, string(adopt))
	if _, err := p.UpTo(ctx, 4); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO users(id,ext_id,dn,username,name,synced_at,sex) VALUES(1,'a','a','a','A','original','f')`,
		`INSERT INTO leave_types(id,code,name,cycle_months,unit) VALUES(1,'annual','Annual',12,'workdays')`,
		`INSERT INTO shutdowns(id,name,created_by_id,created_at) VALUES(1,'Shutdown',1,'original')`,
		`INSERT INTO shutdown_parts(id,shutdown_id,type_id,start_date,end_date) VALUES(1,1,1,'2026-12-14','2026-12-15')`,
		`INSERT INTO requests(id,user_id,type_id,kind,cancels_request_id,start_date,end_date,days_x100,cycle_start,status,calendar_event_id,created_at,shutdown_part_id,half_start,halves_set_by_id,halves_set_at) VALUES
		(1,1,1,'leave',NULL,'2026-11-02','2026-11-03',200,'2026-01-01','approved','same','original',NULL,0,NULL,NULL),
		(2,1,1,'leave',NULL,'2026-11-04','2026-11-07',200,'2026-01-01','approved','adopt','original',NULL,0,NULL,NULL),
		(3,1,1,'leave',NULL,'2026-11-06','2026-11-06',100,'2026-01-01','pending','','original',NULL,0,NULL,NULL),
		(4,1,1,'leave',NULL,'2026-11-09','2026-11-13',300,'2026-01-01','approved','','original',NULL,0,NULL,NULL),
		(5,1,1,'cancellation',4,'2026-11-09','2026-11-13',300,'2026-01-01','pending','','original',NULL,0,NULL,NULL),
		(6,1,1,'leave',NULL,'2026-12-14','2026-12-15',200,'2026-01-01','draft','','original',1,0,NULL,NULL),
		(7,1,1,'leave',NULL,'2026-11-16','2026-11-17',150,'2026-01-01','approved','','original',NULL,1,1,'audit'),
		(8,1,1,'leave',NULL,'2026-11-18','2026-11-18',100,'2026-01-01','cancelled','','original',NULL,0,NULL,NULL)`,
		`UPDATE request_ranges SET calendar_event_id='same' WHERE request_id=1`,
		`DELETE FROM request_ranges WHERE request_id IN (2,4,5)`,
		`INSERT INTO request_ranges(request_id,start_date,end_date,calendar_event_id) VALUES
		(2,'2026-11-04','2026-11-05',''),(2,'2026-11-07','2026-11-07',''),
		(4,'2026-11-09','2026-11-10','first'),(4,'2026-11-13','2026-11-13','last'),
		(5,'2026-11-09','2026-11-10',''),(5,'2026-11-13','2026-11-13','')`,
		`UPDATE settings SET half_days_enabled=1,updated_by_id=1,updated_at='original'`,
		`INSERT INTO balance_adjustments(user_id,type_id,cycle_start,days_x100,reason,by_user_id,created_at) VALUES(1,1,'2026-01-01',125,'original',1,'original')`,
		`INSERT INTO outbox(kind,payload,next_attempt_at,created_at) VALUES('calendar_create','{"request_id":1}','2026-01-02T00:00:00Z','2026-01-01T00:00:00Z')`,
	} {
		contractExec(t, d, q)
	}
	if _, err := p.UpTo(ctx, 5); err != nil {
		t.Fatal(err)
	}
	contractExec(t, d, `UPDATE leave_types SET show_balance=0 WHERE id=1`)
	return d, p, path
}

func contractRows(t *testing.T, d *sql.DB, q string) [][]any {
	t.Helper()
	rows, err := d.Query(q)
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var out [][]any
	for rows.Next() {
		vals, ptrs := make([]any, len(cols)), make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatal(err)
		}
		out = append(out, vals)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// Include every column, including admin-edited show_balance. The projection
// excludes only the intentionally retired column when comparing across Up.
func contractHashes(t *testing.T, d *sql.DB, omitLegacy bool) map[string][32]byte {
	t.Helper()
	out := map[string][32]byte{}
	for _, row := range contractRows(t, d, `SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name NOT IN ('goose_db_version','request_corrections','approval_delegations') ORDER BY name`) {
		name := row[0].(string)
		var cols []string
		for _, col := range contractRows(t, d, `SELECT name FROM pragma_table_info('`+name+`') ORDER BY name`) {
			c := col[0].(string)
			if omitLegacy && name == "requests" && c == "calendar_event_id" {
				continue
			}
			cols = append(cols, `"`+c+`"`)
		}
		data, err := json.Marshal(contractRows(t, d, `SELECT `+strings.Join(cols, ",")+` FROM "`+name+`" ORDER BY rowid`))
		if err != nil {
			t.Fatal(err)
		}
		out[name] = sha256.Sum256(data)
	}
	if len(out) != 21 {
		t.Fatalf("tables hashed: %d", len(out))
	}
	return out
}

func contractUp(t *testing.T, p *goose.Provider) {
	t.Helper()
	if _, err := p.UpTo(context.Background(), 6); err != nil {
		t.Fatal(err)
	}
}

func contractScript(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name + "_0006.sql")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestContract0006Up(t *testing.T) {
	d, p, _ := contractFixture(t)
	before := contractHashes(t, d, true)
	contractUp(t, p)
	after := contractHashes(t, d, true)
	delete(before, "request_ranges")
	delete(after, "request_ranges")
	if !reflect.DeepEqual(before, after) {
		t.Fatal("Up changed application data")
	}
	want := [][]any{
		{int64(1), "2026-11-02", "2026-11-03", "same"}, {int64(2), "2026-11-04", "2026-11-05", "adopt"},
		{int64(2), "2026-11-07", "2026-11-07", ""},
		{int64(3), "2026-11-06", "2026-11-06", ""}, {int64(4), "2026-11-09", "2026-11-10", "first"},
		{int64(4), "2026-11-13", "2026-11-13", "last"}, {int64(5), "2026-11-09", "2026-11-10", ""},
		{int64(5), "2026-11-13", "2026-11-13", ""}, {int64(6), "2026-12-14", "2026-12-15", ""},
		{int64(7), "2026-11-16", "2026-11-17", ""}, {int64(8), "2026-11-18", "2026-11-18", ""},
	}
	if got := contractRows(t, d, `SELECT request_id,start_date,end_date,calendar_event_id FROM request_ranges ORDER BY request_id,start_date`); !reflect.DeepEqual(got, want) {
		t.Fatalf("ranges=%v want %v", got, want)
	}
	verifyContract(t, d)
}

func verifyContract(t *testing.T, d *sql.DB) {
	t.Helper()
	counts := map[string]string{"requests": "8", "request_ranges": "11", "range_event_ids": "4"}
	for _, line := range strings.Split(strings.TrimSpace(contractScript(t, "verify")), "\n") {
		rows := contractRows(t, d, line)
		switch line {
		case "PRAGMA integrity_check;":
			if !reflect.DeepEqual(rows, [][]any{{"ok"}}) {
				t.Fatalf("integrity=%v", rows)
			}
		case "PRAGMA foreign_key_check;":
			if len(rows) != 0 {
				t.Fatalf("foreign keys=%v", rows)
			}
		default:
			if len(rows) != 1 {
				t.Fatalf("missing verification: %s: %v", line, rows)
			}
			r := rows[0]
			expected := fmt.Sprint(r[2])
			if v, ok := counts[r[0].(string)]; ok {
				expected = v
			}
			if fmt.Sprint(r[1]) != expected {
				t.Fatalf("%s: got %v expected %s", line, r[1], expected)
			}
		}
	}
}

func TestContract0006Guards(t *testing.T) {
	cases := []struct{ name, sql, constraint string }{
		{"missing", `DELETE FROM request_ranges WHERE request_id=3`, "a request has no request_ranges row"},
		{"orphan", `INSERT INTO request_ranges(request_id,start_date,end_date) VALUES(999,'2026-11-01','2026-11-01')`, "a request_ranges row has no request"},
		{"span start", `UPDATE request_ranges SET start_date='2026-11-10' WHERE request_id=4 AND start_date='2026-11-09'`, "a request start_date or end_date differs from its ranges"},
		{"span end", `UPDATE request_ranges SET end_date='2026-11-14' WHERE request_id=4 AND start_date='2026-11-13'`, "a request start_date or end_date differs from its ranges"},
		{"overlap", `INSERT INTO request_ranges(request_id,start_date,end_date) VALUES(4,'2026-11-10','2026-11-11')`, "a request has overlapping or adjacent ranges"},
		{"adjacent", `INSERT INTO request_ranges(request_id,start_date,end_date) VALUES(4,'2026-11-11','2026-11-11')`, "a request has overlapping or adjacent ranges"},
		{"legacy", `UPDATE requests SET calendar_event_id='distinct' WHERE id=1`, "requests.calendar_event_id holds an event id no range has"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, p, _ := contractFixture(t)
			d.SetMaxOpenConns(1)
			if tc.name == "orphan" {
				contractExec(t, d, `PRAGMA foreign_keys=OFF`)
			}
			contractExec(t, d, tc.sql)
			contractExec(t, d, `PRAGMA foreign_keys=ON`)
			before := contractHashes(t, d, false)
			versions := contractRows(t, d, `SELECT id,version_id,is_applied,tstamp FROM goose_db_version ORDER BY id`)
			_, err := p.UpTo(context.Background(), 6)
			message := "0006 refused: " + tc.constraint
			if err == nil || !strings.Contains(err.Error(), message) || strings.Count(err.Error(), "0006 refused:") != 1 {
				t.Fatalf("want %q only: %v", message, err)
			}
			if !reflect.DeepEqual(before, contractHashes(t, d, false)) || !reflect.DeepEqual(versions, contractRows(t, d, `SELECT id,version_id,is_applied,tstamp FROM goose_db_version ORDER BY id`)) {
				t.Fatal("refusal changed data/version")
			}
			if got := contractRows(t, d, `SELECT count(*) FROM sqlite_master WHERE type='trigger' AND name IN ('requests_default_range','requests_sync_range')`); got[0][0] != int64(2) {
				t.Fatalf("refusal lost triggers: %v", got)
			}
		})
	}
	t.Run("one day gap", func(t *testing.T) {
		d, p, _ := contractFixture(t)
		contractExec(t, d, `UPDATE request_ranges SET start_date='2026-11-12' WHERE request_id=4 AND start_date='2026-11-13'`)
		contractUp(t, p)
	})
}

func contractSchema(t *testing.T, d *sql.DB) map[string][][]any {
	t.Helper()
	out := map[string][][]any{}
	for _, r := range contractRows(t, d, `SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name NOT IN ('goose_db_version','request_corrections','approval_delegations') ORDER BY name`) {
		name := r[0].(string)
		out[name+" columns"] = contractRows(t, d, `SELECT name,type,"notnull",dflt_value,pk FROM pragma_table_info('`+name+`') ORDER BY name`)
		out[name+" fk"] = contractRows(t, d, `SELECT "table","from","to",on_update,on_delete,match FROM pragma_foreign_key_list('`+name+`') ORDER BY "from"`)
		// Autoindex names and physical column ids may change with a rebuild.
		out[name+" indexes"] = contractRows(t, d, `SELECT name,"unique",origin,partial FROM pragma_index_list('`+name+`') ORDER BY name`)
		for _, idx := range out[name+" indexes"] {
			n := idx[0].(string)
			out[n+" columns"] = contractRows(t, d, `SELECT name FROM pragma_index_info('`+n+`') ORDER BY seqno`)
		}
	}
	out["triggers"] = contractRows(t, d, `SELECT name,sql FROM sqlite_master WHERE type='trigger' ORDER BY name`)
	return out
}

func TestContract0006DownAndUndo(t *testing.T) {
	fresh, fp, err := openProvider(filepath.Join(t.TempDir(), "fresh-v5.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if _, err := fp.UpTo(context.Background(), 5); err != nil {
		t.Fatal(err)
	}
	freshSchema := contractSchema(t, fresh)
	for _, undo := range []bool{false, true} {
		t.Run(fmt.Sprint("undo=", undo), func(t *testing.T) {
			d, p, _ := contractFixture(t)
			v5schema := contractSchema(t, d)
			contractUp(t, p)
			v6data := contractHashes(t, d, true)
			if undo {
				contractExec(t, d, contractScript(t, "undo"))
			} else if _, err := p.Down(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(v5schema, contractSchema(t, d)) {
				t.Fatal("Down schema differs from v5")
			}
			if !reflect.DeepEqual(freshSchema, contractSchema(t, d)) {
				t.Fatal("Down schema differs from fresh v5")
			}
			if !reflect.DeepEqual(v6data, contractHashes(t, d, true)) {
				t.Fatal("Down changed application data")
			}
			if got := contractRows(t, d, `SELECT count(*) FROM requests WHERE calendar_event_id<>''`); got[0][0] != int64(0) {
				t.Fatalf("legacy ids=%v", got)
			}
			if got := contractRows(t, d, `SELECT max(version_id) FROM goose_db_version WHERE is_applied=1`); got[0][0] != int64(5) {
				t.Fatalf("version=%v", got)
			}
			contractUp(t, p)
			if !reflect.DeepEqual(v6data, contractHashes(t, d, true)) {
				t.Fatal("second Up differs")
			}
		})
	}
}

func TestContract0006UndoDrift(t *testing.T) {
	b, err := migrations.ReadFile("migrations/0006_contract_request_ranges.sql")
	if err != nil {
		t.Fatal(err)
	}
	_, down, ok := strings.Cut(string(b), "-- +goose Down")
	if !ok {
		t.Fatal("no Down")
	}
	normalise := func(s string) string {
		var lines []string
		for _, l := range strings.Split(s, "\n") {
			l = strings.TrimSpace(l)
			if strings.HasPrefix(l, "--") || l == "BEGIN IMMEDIATE;" || l == "COMMIT;" || strings.HasPrefix(l, "DELETE FROM goose_db_version") {
				continue
			}
			lines = append(lines, l)
		}
		return strings.Join(strings.Fields(strings.Join(lines, " ")), " ")
	}
	if normalise(down) != normalise(contractScript(t, "undo")) {
		t.Fatal("undo script drifted from Down")
	}
}

func TestContract0006OperatorSQL(t *testing.T) {
	d, p, path := contractFixture(t)
	pre := filepath.Join(t.TempDir(), "pre.db")
	contractExec(t, d, `VACUUM INTO ?`, pre)
	ro, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	want := map[string]int64{"users": 1, "requests": 8, "request_ranges": 11, "balance_adjustments": 1, "to_blank": 1, "to_adopt": 1, "distinct": 0, "requests_without_range": 0, "orphan_ranges": 0, "span_mismatches": 0, "touching_ranges": 0, "legacy_event_ids": 0}
	checkPre := func(distinct bool) {
		t.Helper()
		for _, line := range strings.Split(strings.TrimSpace(contractScript(t, "precheck")), "\n") {
			rows := contractRows(t, ro, line)
			if strings.HasPrefix(line, "SELECT r.id,") {
				if distinct {
					if !reflect.DeepEqual(rows, [][]any{{int64(1), "approved", "distinct", "same"}}) {
						t.Fatalf("distinct listing=%v", rows)
					}
				} else if len(rows) != 0 {
					t.Fatalf("unexpected listing=%v", rows)
				}
			} else if len(rows) != 1 || rows[0][1] != want[rows[0][0].(string)] {
				t.Fatalf("precheck %s: %v", line, rows)
			}
		}
	}
	checkPre(false)
	contractExec(t, d, `UPDATE requests SET calendar_event_id='distinct' WHERE id=1`)
	want["to_blank"], want["distinct"], want["legacy_event_ids"] = 0, 1, 1
	checkPre(true)
	contractExec(t, d, `UPDATE requests SET calendar_event_id='same' WHERE id=1`)
	contractUp(t, p)
	verifyContract(t, d)
	d.SetMaxOpenConns(1)
	compare := strings.Replace(contractScript(t, "compare"), "/data/leave-pre-0006.db", pre, 1)
	lines := strings.Split(strings.TrimSpace(compare), "\n")
	contractExec(t, d, lines[0])
	covered := map[string]int{}
	for _, line := range lines[1:] {
		r := contractRows(t, d, line)
		if len(r) != 1 || r[0][1] != int64(0) {
			t.Fatalf("compare %s: %v", line, r)
		}
		covered[r[0][0].(string)]++
	}
	for _, r := range contractRows(t, d, `SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name<>'goose_db_version'`) {
		if covered[r[0].(string)] != 2 {
			t.Fatalf("table not compared both ways: %v", r)
		}
	}
	contractExec(t, d, `UPDATE users SET name='corrupt' WHERE id=1`)
	for _, line := range lines[1:] {
		if strings.HasPrefix(line, "SELECT 'users',") {
			if r := contractRows(t, d, line); r[0][1] != int64(1) {
				t.Fatalf("compare missed corruption: %v", r)
			}
		}
	}
}

func TestContract0006OldBinaryPins(t *testing.T) {
	d, p, _ := contractFixture(t)
	contractUp(t, p)
	if _, err := d.Query(`SELECT calendar_event_id FROM requests WHERE id=1`); err == nil || !strings.Contains(err.Error(), "no such column") {
		t.Fatalf("old calendar read: %v", err)
	}
	contractExec(t, d, `INSERT INTO requests(id,user_id,type_id,kind,start_date,end_date,days_x100,cycle_start,status,created_at) VALUES(9,1,1,'leave','2027-01-01','2027-01-01',100,'2027-01-01','pending','')`)
	if r := contractRows(t, d, `SELECT count(*) FROM request_ranges WHERE request_id=9`); r[0][0] != int64(0) {
		t.Fatalf("old insert got a trigger range: %v", r)
	}
	if _, err := d.Exec(`DELETE FROM requests WHERE id=3`); err == nil || !strings.Contains(err.Error(), "FOREIGN KEY") {
		t.Fatalf("raw delete: %v", err)
	}
	contractExec(t, d, `DELETE FROM request_ranges WHERE request_id=3`)
	contractExec(t, d, `DELETE FROM requests WHERE id=3`)
}

func TestContract0006MigrateCommand(t *testing.T) {
	d, _, _ := contractFixture(t)
	path := filepath.Join(t.TempDir(), "rehearsal.db")
	contractExec(t, d, `VACUUM INTO ?`, path)
	binary := filepath.Join(t.TempDir(), "leave")
	build := exec.Command("go", "build", "-o", binary, "../..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	cmd := exec.Command(binary, "-migrate")
	cmd.Env = append(os.Environ(), "DB_PATH="+path)
	out, err := cmd.CombinedOutput()
	t.Logf("%s", out)
	if err != nil || !strings.Contains(string(out), "migrating "+path+": 5 -> 11\n") ||
		!strings.Contains(string(out), "0006 applied 0006_contract_request_ranges.sql\n0007 applied 0007_type_staff_can_apply.sql\n") {
		t.Fatalf("v5 rehearsal: %v\n%s", err, out)
	}
}

// Pin the replacement guard set, not an invalid SQL recomputation of days_x100.
func TestContract0006GuardColumns(t *testing.T) {
	b, err := migrations.ReadFile("migrations/0006_contract_request_ranges.sql")
	if err != nil {
		t.Fatal(err)
	}
	up, _, _ := strings.Cut(string(b), "-- +goose Down")
	for _, name := range []string{"requests_without_range", "orphan_ranges", "span_mismatches", "touching_ranges", "legacy_event_ids"} {
		if !strings.Contains(up, name+" INTEGER NOT NULL CONSTRAINT") {
			t.Errorf("missing guard column %s", name)
		}
	}
	if slices.Contains(strings.Fields(up), "days_x100") {
		t.Fatal("days are authoritative, not a range sum")
	}
}
