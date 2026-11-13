package db

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
)

// Exercise real goose Up from v2 and Down with a populated nullable FK while
// foreign-key enforcement is on. SQLite permits DROP of this REFERENCES column.
func TestHalvesAuditMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "v2.db")
	d, p, err := openProvider(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if _, err := p.UpTo(ctx, 2); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO users(id,ext_id,dn,username,name,synced_at) VALUES(1,'a','a','a','A','')`,
		`INSERT INTO leave_types(id,code,name,cycle_months,unit) VALUES(1,'annual','Annual',12,'workdays')`,
		`INSERT INTO requests(id,user_id,type_id,kind,start_date,end_date,days_x100,half_start,cycle_start,status,created_at) VALUES(1,1,1,'leave','2026-11-16','2026-11-16',50,1,'2026-01-01','pending','original')`,
	} {
		if _, err := d.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	before := applicationRows(t, d)
	if _, err := p.UpTo(ctx, 3); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, applicationRows(t, d)) {
		t.Fatal("Up changed original rows")
	}
	var by, at any
	if err := d.QueryRow(`SELECT halves_set_by_id,halves_set_at FROM requests WHERE id=1`).Scan(&by, &at); err != nil || by != nil || at != nil {
		t.Fatalf("new audit=%v/%v: %v", by, at, err)
	}
	if _, err := d.Exec(`UPDATE requests SET halves_set_by_id=999 WHERE id=1`); err == nil {
		t.Fatal("audit FK not enforced")
	}
	if _, err := d.Exec(`UPDATE requests SET halves_set_by_id=1,halves_set_at='audit' WHERE id=1`); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Down(ctx); err != nil {
		t.Fatalf("Down with populated REFERENCES: %v", err)
	}
	var n int
	if err := d.QueryRow(`SELECT count(*) FROM pragma_table_info('requests') WHERE name IN ('halves_set_by_id','halves_set_at')`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("Down columns=%d: %v", n, err)
	}
	if !reflect.DeepEqual(before, applicationRows(t, d)) {
		t.Fatal("Down changed original rows")
	}
	if _, err := p.UpTo(ctx, 3); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, applicationRows(t, d)) {
		t.Fatal("Down/Up changed original rows")
	}
	if err := d.QueryRow(`SELECT halves_set_by_id,halves_set_at FROM requests WHERE id=1`).Scan(&by, &at); err != nil || by != nil || at != nil {
		t.Fatalf("recreated audit=%v/%v: %v", by, at, err)
	}
	rows, err := d.Query(`PRAGMA foreign_key_check`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("foreign key violation")
	}
}
