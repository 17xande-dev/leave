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

func TestTypeStaffCanApplyMigration(t *testing.T) {
	ctx := context.Background()
	d, p, _ := contractFixture(t)
	contractUp(t, p)
	const oldInsert = `INSERT INTO leave_types(code,name,allowance_x100,cycle_months,unit,backdate_days,requires_note,active,sort,restricted_rule,coverage_rule,show_balance) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`
	codes := []string{"sick", "family", "maternity", "parental", "compassionate", "unpaid", "floating", "weekend", "closure"}
	for i, code := range codes {
		allowance, cycle, unit := any(1500), 12, "workdays"
		switch code {
		case "sick":
			allowance, cycle = 3000, 36
		case "maternity":
			allowance, unit = 12000, "calendar"
		case "parental":
			allowance, unit = 1000, "calendar"
		case "compassionate", "unpaid", "closure":
			allowance = nil
		case "weekend":
			unit = "weekend"
		}
		contractExec(t, d, oldInsert, code, "Original "+code, allowance, cycle, unit, 92, true, i < 6, i+1, "any", "count", i < 2)
	}
	contractExec(t, d, `UPDATE leave_types SET name='HR compassionate policy',backdate_days=45 WHERE code='compassionate'`)
	before := contractHashes(t, d, false)
	delete(before, "leave_types")
	const oldCols = `id,code,name,allowance_x100,cycle_months,unit,backdate_days,requires_note,active,sort,restricted_rule,coverage_rule,show_balance`
	typesBefore := contractRows(t, d, `SELECT `+oldCols+` FROM leave_types ORDER BY id`)
	checkData := func() {
		t.Helper()
		after := contractHashes(t, d, false)
		delete(after, "leave_types")
		if !reflect.DeepEqual(before, after) || !reflect.DeepEqual(typesBefore, contractRows(t, d, `SELECT `+oldCols+` FROM leave_types ORDER BY id`)) {
			t.Fatal("0007 changed existing application data")
		}
	}
	checkFlags := func() {
		t.Helper()
		rows := contractRows(t, d, `SELECT code,staff_can_apply FROM leave_types ORDER BY id`)
		if len(rows) != 10 {
			t.Fatalf("types=%d", len(rows))
		}
		for _, row := range rows {
			want := int64(1)
			if row[0] == "compassionate" || row[0] == "unpaid" {
				want = 0
			}
			if row[1] != want {
				t.Errorf("%s staff_can_apply=%v, want %d", row[0], row[1], want)
			}
		}
	}
	up := func() {
		t.Helper()
		if _, err := p.UpTo(ctx, 7); err != nil {
			t.Fatal(err)
		}
		checkData()
		checkFlags()
	}
	// Keep a v6 copy for the real command, rather than testing only goose.
	path := filepath.Join(t.TempDir(), "v6.db")
	contractExec(t, d, `VACUUM INTO ?`, path)
	up()
	if _, err := d.Exec(`UPDATE leave_types SET staff_can_apply=2 WHERE code='annual'`); err == nil || !strings.Contains(err.Error(), "CHECK constraint failed") {
		t.Fatalf("invalid flag accepted: %v", err)
	}
	if _, err := p.Down(ctx); err != nil {
		t.Fatal(err)
	}
	down := contractHashes(t, d, false)
	checkData()
	up()
	script, err := os.ReadFile("testdata/undo_0007.sql")
	if err != nil {
		t.Fatal(err)
	}
	contractExec(t, d, string(script))
	if !reflect.DeepEqual(down, contractHashes(t, d, false)) {
		t.Fatal("undo_0007 differs from Down")
	}
	if version, err := p.GetDBVersion(ctx); err != nil || version != 6 {
		t.Fatalf("undo version=%d: %v", version, err)
	}
	up()
	contractExec(t, d, oldInsert, "new-policy", "New", nil, 12, "workdays", 92, false, true, 30, "any", "count", true)
	if got := contractRows(t, d, `SELECT staff_can_apply FROM leave_types WHERE code='new-policy'`); got[0][0] != int64(1) {
		t.Fatalf("old INSERT: %v", got)
	}
	contractExec(t, d, `UPDATE leave_types SET name='Old binary rename' WHERE code='compassionate'`)
	if got := contractRows(t, d, `SELECT staff_can_apply FROM leave_types WHERE code='compassionate'`); got[0][0] != int64(0) {
		t.Fatalf("old UPDATE: %v", got)
	}
	binary := filepath.Join(t.TempDir(), "leave")
	if out, err := exec.Command("go", "build", "-o", binary, "../..").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	for _, want := range []string{"6 -> 11", "11 -> 11"} {
		cmd := exec.Command(binary, "-migrate")
		cmd.Env = append(os.Environ(), "DB_PATH="+path)
		out, err := cmd.CombinedOutput()
		t.Logf("%s", out)
		if err != nil || !strings.Contains(string(out), ": "+want+"\n") || !strings.Contains(string(out), "0007 applied 0007_type_staff_can_apply.sql") {
			t.Fatalf("migrate %s: %v\n%s", want, err, out)
		}
	}
}
