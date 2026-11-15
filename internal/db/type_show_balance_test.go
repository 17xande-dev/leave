package db

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Extend the Phase 4 hashes to include every range. Leave types retain the
// pre-0005 projection so an added column cannot mask a changed existing field.
func balanceMigrationHashes(t *testing.T, d *sql.DB) map[string][32]byte {
	t.Helper()
	out := rangeMigrationHashes(t, d)
	rows, err := d.Query(`SELECT * FROM request_ranges ORDER BY rowid`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	var data bytes.Buffer
	for rows.Next() {
		values, pointers := make([]any, len(cols)), make([]any, len(cols))
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
	out["request_ranges"] = sha256.Sum256(data.Bytes())
	return out
}

func TestTypeShowBalanceMigration(t *testing.T) {
	ctx := context.Background()
	d, p, err := openProvider(filepath.Join(t.TempDir(), "production-shape.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := p.UpTo(ctx, 4); err != nil {
		t.Fatal(err)
	}
	// Use the old binary's complete INSERT list, including an HR-edited type.
	const oldInsert = `INSERT INTO leave_types(code,name,allowance_x100,cycle_months,unit,backdate_days,requires_note,active,sort,restricted_rule,coverage_rule) VALUES(?,?,?,?,?,?,?,?,?,?,?)`
	codes := []string{"annual", "sick", "family", "maternity", "parental", "compassionate", "unpaid", "floating", "weekend", "closure"}
	for i, code := range codes {
		allowance, cycle, unit, active := any(1500), 12, "workdays", i < 7
		if code == "sick" {
			allowance, cycle = 3000, 36
		}
		if code == "maternity" {
			allowance, unit = 12000, "calendar"
		}
		if code == "parental" {
			allowance, unit = 1000, "calendar"
		}
		if code == "unpaid" || code == "compassionate" || code == "closure" {
			allowance = nil
		}
		if code == "weekend" {
			unit = "weekend"
		}
		if _, err := d.Exec(oldInsert, code, "Original "+code, allowance, cycle, unit, 92, false, active, i, "any", "count"); err != nil {
			t.Fatal(err)
		}
	}
	for _, q := range []string{
		`UPDATE leave_types SET name='HR parental policy',allowance_x100=1400,backdate_days=45,sort=24 WHERE code='parental'`,
		`INSERT INTO users(id,ext_id,dn,username,name,synced_at) VALUES(1,'a','a','a','A','original')`,
		`INSERT INTO requests(user_id,type_id,kind,start_date,end_date,days_x100,cycle_start,status,calendar_event_id,created_at,halves_set_by_id,halves_set_at) VALUES(1,1,'leave','2026-11-16','2026-11-17',200,'2026-01-01','approved','original-event','original',1,'original-audit')`,
		`INSERT INTO balance_adjustments(user_id,type_id,cycle_start,days_x100,reason,by_user_id,created_at) VALUES(1,1,'2026-01-01',125,'original adjustment',1,'original')`,
		`UPDATE settings SET half_days_enabled=1,updated_by_id=1,updated_at='original'`,
	} {
		if _, err := d.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	before := balanceMigrationHashes(t, d)
	check := func() {
		t.Helper()
		if !reflect.DeepEqual(before, balanceMigrationHashes(t, d)) {
			t.Fatal("visibility migration changed existing application data")
		}
		rows, err := d.Query(`SELECT code,show_balance FROM leave_types ORDER BY sort`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			var code string
			var got int
			if err := rows.Scan(&code, &got); err != nil {
				t.Fatal(err)
			}
			want := 1
			if code == "compassionate" || code == "unpaid" || code == "parental" || code == "maternity" {
				want = 0
			}
			if got != want {
				t.Errorf("%s show_balance=%d, want %d", code, got, want)
			}
			n++
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		if n != len(codes) {
			t.Fatalf("type count=%d, want %d", n, len(codes))
		}
	}
	if _, err := p.UpTo(ctx, 5); err != nil {
		t.Fatal(err)
	}
	check()
	if _, err := d.Exec(`UPDATE leave_types SET show_balance=2 WHERE code='annual'`); err == nil || !strings.Contains(err.Error(), "CHECK constraint failed") {
		t.Fatalf("CHECK accepted invalid visibility: %v", err)
	}
	check()
	if _, err := p.Down(ctx); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := d.QueryRow(`SELECT count(*) FROM pragma_table_info('leave_types') WHERE name='show_balance'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("Down visibility columns=%d: %v", n, err)
	}
	if !reflect.DeepEqual(before, balanceMigrationHashes(t, d)) {
		t.Fatal("Down changed existing data")
	}
	if _, err := p.UpTo(ctx, 5); err != nil {
		t.Fatal(err)
	}
	check()
	// During a container swap an old INSERT defaults visible, and an old
	// UPDATE preserves the hidden flag even when HR changes the type's name.
	if _, err := d.Exec(oldInsert, "new-policy", "New policy", 100, 12, "workdays", 92, false, true, 30, "any", "count"); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT show_balance FROM leave_types WHERE code='new-policy'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("old INSERT visibility=%d: %v", n, err)
	}
	if _, err := d.Exec(`UPDATE leave_types SET name='Renamed by old binary' WHERE code='parental'`); err != nil {
		t.Fatal(err)
	}
	if err := d.QueryRow(`SELECT show_balance FROM leave_types WHERE code='parental'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("old UPDATE visibility=%d: %v", n, err)
	}
	beforeRepeat := balanceMigrationHashes(t, d)
	if _, err := p.Up(ctx); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(beforeRepeat, balanceMigrationHashes(t, d)) {
		t.Fatal("repeated Up changed data")
	}
	var integrity string
	if err := d.QueryRow(`PRAGMA integrity_check`).Scan(&integrity); err != nil || integrity != "ok" {
		t.Fatalf("integrity=%q: %v", integrity, err)
	}
	rows, err := d.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("foreign key violation")
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}
