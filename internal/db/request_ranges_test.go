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

// Hash every column of every pre-existing table, including the v2/v3 fields.
// This is deliberately independent of applicationRows' baseline projection.
func rangeMigrationHashes(t *testing.T, d *sql.DB) map[string][32]byte {
	t.Helper()
	rows, err := d.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' AND name NOT IN ('goose_db_version','request_ranges') ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if len(names) != 20 {
		t.Fatalf("preexisting tables=%d", len(names))
	}
	out := make(map[string][32]byte)
	for _, name := range names {
		columns := "*"
		if name == "leave_types" {
			// The v5 visibility flag is checked separately from preexisting data.
			columns = "id,code,name,allowance_x100,cycle_months,unit,restricted_rule,backdate_days,requires_note,active,sort,coverage_rule"
		}
		r, err := d.Query(`SELECT ` + columns + ` FROM "` + strings.ReplaceAll(name, `"`, `""`) + `" ORDER BY rowid`)
		if err != nil {
			t.Fatal(err)
		}
		cols, err := r.Columns()
		if err != nil {
			t.Fatal(err)
		}
		var b bytes.Buffer
		for r.Next() {
			values, ptrs := make([]any, len(cols)), make([]any, len(cols))
			for i := range values {
				ptrs[i] = &values[i]
			}
			if err := r.Scan(ptrs...); err != nil {
				t.Fatal(err)
			}
			if err := json.NewEncoder(&b).Encode(values); err != nil {
				t.Fatal(err)
			}
		}
		if err := r.Err(); err != nil {
			t.Fatal(err)
		}
		r.Close()
		out[name] = sha256.Sum256(b.Bytes())
	}
	return out
}

func TestRequestRangesMigration(t *testing.T) {
	ctx := context.Background()
	d, p, err := openProvider(filepath.Join(t.TempDir(), "production-shape.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := p.UpTo(ctx, 1); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`DROP TABLE goose_db_version`,
		`ALTER TABLE users DROP COLUMN sex`,
		`ALTER TABLE users ADD COLUMN sex TEXT NOT NULL DEFAULT '' CHECK (sex IN ('','f','m'))`,
	} {
		if _, err := d.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	adopt, err := os.ReadFile("testdata/adopt_baseline.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(string(adopt)); err != nil {
		t.Fatal(err)
	}
	if _, err := p.UpTo(ctx, 3); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO users(id,ext_id,dn,username,name,synced_at,sex) VALUES(1,'a','a','a','A','original','f')`,
		`INSERT INTO leave_types(id,code,name,cycle_months,unit) VALUES(1,'annual','Annual',12,'workdays')`,
		`INSERT INTO requests(id,user_id,type_id,kind,start_date,end_date,days_x100,half_start,cycle_start,status,calendar_event_id,created_at,halves_set_by_id,halves_set_at) VALUES
		(1,1,1,'leave','2026-11-16','2026-11-17',150,1,'2026-01-01','approved','graph-original','original',1,'audit'),
		(2,1,1,'leave','2026-11-18','2026-11-18',100,0,'2026-01-01','pending','','original',NULL,NULL),
		(3,1,1,'leave','2026-11-19','2026-11-20',200,0,'2026-01-01','cancelled','graph-cancelled','original',NULL,NULL)`,
		`UPDATE settings SET half_days_enabled=1,updated_by_id=1,updated_at='original'`,
		`INSERT INTO balance_adjustments(user_id,type_id,cycle_start,days_x100,reason,by_user_id,created_at) VALUES(1,1,'2026-01-01',125,'original',1,'original')`,
		`INSERT INTO outbox(kind,payload,next_attempt_at,created_at) VALUES('calendar_create','{"request_id":1}','original','original')`,
	} {
		if _, err := d.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	before := rangeMigrationHashes(t, d)
	if _, err := p.UpTo(ctx, 4); err != nil {
		t.Fatal(err)
	}
	check := func() {
		t.Helper()
		if !reflect.DeepEqual(before, rangeMigrationHashes(t, d)) {
			t.Fatal("migration changed preexisting table bytes")
		}
		for _, q := range []string{
			`SELECT count(*) FROM requests r WHERE (SELECT count(*) FROM request_ranges rr WHERE rr.request_id=r.id) <> 1`,
			`SELECT count(*) FROM requests r JOIN request_ranges rr ON rr.request_id=r.id WHERE rr.start_date<>r.start_date OR rr.end_date<>r.end_date OR rr.calendar_event_id<>r.calendar_event_id`,
			`SELECT count(*) FROM requests r WHERE r.calendar_event_id<>'' AND (SELECT count(*) FROM request_ranges rr WHERE rr.calendar_event_id=r.calendar_event_id)<>1`,
		} {
			var n int
			if err := d.QueryRow(q).Scan(&n); err != nil || n != 0 {
				t.Fatalf("check %s: %d %v", q, n, err)
			}
		}
		var n int
		if err := d.QueryRow(`SELECT count(*) FROM request_ranges`).Scan(&n); err != nil || n != 3 {
			t.Fatalf("ranges=%d %v", n, err)
		}
	}
	check()
	if _, err := p.Down(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := p.UpTo(ctx, 4); err != nil {
		t.Fatal(err)
	}
	check()
	// The unchanged v3 INSERT column list used by the old binary must work.
	if _, err := d.Exec(`INSERT INTO requests (id,user_id,type_id,kind,cancels_request_id,start_date,end_date,days_x100,half_start,half_end,cycle_start,note,status,approver_id,decided_by_id,decided_at,decision_note,calendar_event_id,created_at,shutdown_part_id,escalate,allow_negative,recorded_by_id,removed_by_id,removed_at,halves_set_by_id,halves_set_at)
	VALUES(4,1,1,'leave',NULL,'2026-12-01','2026-12-03',300,0,0,'2026-01-01','','pending',NULL,NULL,NULL,'','','old-binary',NULL,0,0,NULL,NULL,NULL,NULL,NULL)`); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := d.QueryRow(`SELECT count(*) FROM request_ranges WHERE request_id=4 AND start_date='2026-12-01' AND end_date='2026-12-03' AND calendar_event_id=''`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("old-binary range=%d %v", n, err)
	}
	// An old binary edits only the span; its event must survive that edit.
	if _, err := d.Exec(`UPDATE request_ranges SET calendar_event_id='old-event' WHERE request_id=4`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE requests SET start_date='2026-12-02',end_date='2026-12-04' WHERE id=4`); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT count(*) FROM request_ranges WHERE request_id=4 AND start_date='2026-12-02' AND end_date='2026-12-04' AND calendar_event_id='old-event'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("old edit range=%d %v", n, err)
	}
	// The sync trigger also repairs a missing single range.
	if _, err := d.Exec(`DELETE FROM request_ranges WHERE request_id=4`); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Exec(`UPDATE requests SET start_date='2026-12-02',end_date='2026-12-04' WHERE id=4`); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT count(*) FROM request_ranges WHERE request_id=4 AND start_date='2026-12-02' AND end_date='2026-12-04' AND calendar_event_id=''`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("missing range repair=%d %v", n, err)
	}
	if _, err := d.Exec(`INSERT INTO request_ranges(request_id,start_date,end_date) VALUES(4,'2026-12-10','2026-12-10')`); err != nil {
		t.Fatal(err)
	}
	// A span update must not collapse ranges created by the new binary.
	if _, err := d.Exec(`UPDATE requests SET start_date='2026-12-01',end_date='2026-12-11' WHERE id=4`); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT count(*) FROM request_ranges WHERE request_id=4 AND ((start_date='2026-12-02' AND end_date='2026-12-04') OR (start_date='2026-12-10' AND end_date='2026-12-10'))`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("multi-range span edit=%d %v", n, err)
	}
	beforeRefusal := rangeMigrationHashes(t, d)
	if _, err := p.Down(ctx); err == nil || !strings.Contains(err.Error(), "CHECK constraint failed") {
		t.Fatalf("Down with two ranges: %v", err)
	}
	if !reflect.DeepEqual(beforeRefusal, rangeMigrationHashes(t, d)) {
		t.Fatal("refused Down mutated data")
	}
	if err := d.QueryRow(`SELECT count(*) FROM request_ranges WHERE request_id=4`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("refused Down lost ranges: %d %v", n, err)
	}
	if err := d.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='trigger' AND name='requests_default_range'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("refused Down lost trigger: %d %v", n, err)
	}
	if err := d.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='trigger' AND name='requests_sync_range'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("refused Down lost sync trigger: %d %v", n, err)
	}
	if _, err := d.Exec(`DELETE FROM request_ranges WHERE request_id=4 AND start_date='2026-12-10'`); err != nil {
		t.Fatal(err)
	}
	// An old worker can write only the legacy column after the range was created.
	if _, err := d.Exec(`UPDATE requests SET calendar_event_id='legacy-worker-event' WHERE id=4`); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Down(ctx); err != nil {
		t.Fatalf("Down after correcting refusal: %v", err)
	}
	var eventID string
	if err := d.QueryRow(`SELECT calendar_event_id FROM requests WHERE id=4`).Scan(&eventID); err != nil || eventID != "legacy-worker-event" {
		t.Fatalf("Down lost old worker event: %q %v", eventID, err)
	}
	if err := d.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='trigger' AND name IN ('requests_sync_range','requests_default_range')`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("Down left transitional triggers: %d %v", n, err)
	}
	if _, err := p.UpTo(ctx, 4); err != nil {
		t.Fatal(err)
	}
	var integrity string
	if err := d.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("integrity=%s %v", integrity, err)
	}
	r, err := d.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.Next() {
		t.Fatal("foreign key violation")
	}
}
