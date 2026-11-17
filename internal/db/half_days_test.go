package db

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func assertHalfDaysOff(t *testing.T, d *sql.DB) {
	t.Helper()
	var enabled int
	if err := d.QueryRow(`SELECT half_days_enabled FROM settings WHERE id=1`).Scan(&enabled); err != nil {
		t.Fatal(err)
	}
	if enabled != 0 {
		t.Fatalf("half_days_enabled=%d, want OFF (0)", enabled)
	}
}

func TestHalfDaysMigration(t *testing.T) {
	ctx := context.Background()
	for _, fromV1 := range []bool{false, true} {
		name := "fresh"
		if fromV1 {
			name = "v1"
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "test.db")
			d, p, err := openProvider(path)
			if err != nil {
				t.Fatal(err)
			}
			defer d.Close()
			if fromV1 {
				if _, err := p.UpTo(ctx, 1); err != nil {
					t.Fatal(err)
				}
				for _, q := range []string{
					`INSERT INTO users(id,ext_id,dn,username,name,synced_at) VALUES(1,'a','a','a','A','')`,
					`INSERT INTO leave_types(id,code,name,cycle_months,unit) VALUES(1,'annual','Annual',12,'workdays')`,
					`INSERT INTO requests(user_id,type_id,kind,start_date,end_date,days_x100,half_start,cycle_start,status,created_at) VALUES(1,1,'leave','2026-11-16','2026-11-16',50,1,'2026-01-01','pending','')`,
					`UPDATE settings SET team_default_percent=40,updated_by_id=1,updated_at='before'`,
				} {
					if _, err := d.Exec(q); err != nil {
						t.Fatal(err)
					}
				}
			}
			var before map[string][32]byte
			if fromV1 {
				before = applicationRows(t, d)
			}
			var out bytes.Buffer
			if err := Migrate(ctx, path, &out); err != nil {
				t.Fatal(err)
			}
			const applied = "0001 applied 0001_baseline.sql\n0002 applied 0002_half_days_enabled.sql\n0003 applied 0003_halves_audit.sql\n0004 applied 0004_request_ranges.sql\n0005 applied 0005_type_show_balance.sql\n0006 applied 0006_contract_request_ranges.sql\n"
			beforeVersion := 0
			if fromV1 {
				beforeVersion = 1
			}
			if out.String() != fmt.Sprintf("migrating %s: %d -> 6\n", path, beforeVersion)+applied {
				t.Fatalf("migrate status: %q", out.String())
			}
			assertHalfDaysOff(t, d)
			if fromV1 && !reflect.DeepEqual(before, applicationRows(t, d)) {
				t.Fatal("v1 application data changed")
			}
			if _, err := d.Exec(`UPDATE settings SET half_days_enabled=2`); err == nil {
				t.Fatal("CHECK accepted 2")
			}
			if _, err := d.Exec(`UPDATE settings SET half_days_enabled=1`); err != nil {
				t.Fatal(err)
			}
			before = applicationRows(t, d)
			if _, err := p.Down(ctx); err != nil {
				t.Fatal(err)
			} // v6 contract
			if _, err := p.Down(ctx); err != nil {
				t.Fatal(err)
			} // v5 balance visibility
			if _, err := p.Down(ctx); err != nil {
				t.Fatal(err)
			} // v4 ranges
			if _, err := p.Down(ctx); err != nil {
				t.Fatal(err)
			} // v3 audit columns
			if _, err := p.Down(ctx); err != nil {
				t.Fatal(err)
			}
			var columns int
			if err := d.QueryRow(`SELECT count(*) FROM pragma_table_info('settings') WHERE name='half_days_enabled'`).Scan(&columns); err != nil || columns != 0 {
				t.Fatalf("Down: column count=%d, err=%v", columns, err)
			}
			if _, err := p.Up(ctx); err != nil {
				t.Fatal(err)
			}
			assertHalfDaysOff(t, d)
			if !reflect.DeepEqual(before, applicationRows(t, d)) {
				t.Fatal("Down/Up changed existing application data")
			}
			out.Reset()
			if err := Migrate(ctx, path, &out); err != nil || out.String() != fmt.Sprintf("migrating %s: 6 -> 6\n", path)+applied {
				t.Fatalf("repeated migrate: %q, err=%v", out.String(), err)
			}
		})
	}
}

func TestMigrateUsesOpenSafeguards(t *testing.T) {
	for _, future := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "test.db")
		d, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		query, want := `DROP TABLE goose_db_version`, "predates migrations"
		if future {
			query, want = `INSERT INTO goose_db_version(version_id,is_applied) VALUES(7,1)`, "newer than this binary"
		}
		if _, err := d.Exec(query); err != nil {
			t.Fatal(err)
		}
		d.Close()
		if err := Migrate(context.Background(), path, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("Migrate error=%v, want %q", err, want)
		}
	}
}
