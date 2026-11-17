package handler

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/17xande-dev/leave/internal/directory"
	"github.com/17xande-dev/leave/internal/leave"
	"github.com/xuri/excelize/v2"
)

func readPayroll(t *testing.T, body []byte) *excelize.File {
	t.Helper()
	f, err := excelize.OpenReader(bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

func payrollValue(t *testing.T, f *excelize.File, sheet, cell string, raw bool) string {
	t.Helper()
	v, err := f.GetCellValue(sheet, cell, excelize.Options{RawCellValue: raw})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestPayrollWorkbook(t *testing.T) {
	hs := newHarness(t)
	ctx := context.Background()
	// Make a fractional available figure that must survive the ZIP/XML round
	// trip without rounding to a whole day or becoming spreadsheet text.
	staff, _ := hs.us.ByUsername(ctx, "staff1")
	hr, _ := hs.us.ByUsername(ctx, "hr")
	ts, _ := hs.svc.Types(ctx, false)
	var annual leave.Type
	for _, ty := range ts {
		if ty.Code == leave.AnnualCode {
			annual = ty
		}
	}
	if err := hs.svc.AddAdjustment(ctx, hr, leave.Adjustment{UserID: staff.ID, TypeID: annual.ID, CycleStart: "2026-01-01", Days: -467, Reason: "Opening balance from payroll, October 2026: D = 12.5."}); err != nil {
		t.Fatal(err)
	}
	c := hs.signIn("hr")
	res, err := c.Get(hs.srv.URL + "/admin/payroll.xlsx")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 200 {
		t.Fatalf("status=%d: %s", res.StatusCode, body)
	}
	for key, want := range map[string]string{
		"Content-Type":        "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
		"Content-Disposition": `attachment; filename="leave-payroll-2026-10-05.xlsx"`,
		"Cache-Control":       "no-store", "Content-Length": strconv.Itoa(len(body)),
	} {
		if got := res.Header.Get(key); got != want {
			t.Errorf("%s=%q want %q", key, got, want)
		}
	}
	f := readPayroll(t, body)
	if !reflect.DeepEqual(f.GetSheetList(), []string{"Annual leave", "All types", "About"}) {
		t.Fatal(f.GetSheetList())
	}
	// Literal golden headers, independent of the production slices.
	for sheet, want := range map[string][]string{
		"Annual leave": {"Name", "Username", "Email", "Department", "Status", "Allowance", "Adjustments", "Entitlement", "Approved", "Approved not yet started", "Pending", "Available", "Balance to date", "Payroll opening balance", "Change since opening"},
		"All types":    {"Name", "Username", "Department", "Status", "Leave type", "Unit", "Cycle start", "Cycle end", "Allowance", "Adjustments", "Approved", "Pending", "Available"},
	} {
		rows, err := f.GetRows(sheet)
		if err != nil || len(rows) < 2 {
			t.Fatalf("%s rows: %v", sheet, err)
		}
		if !reflect.DeepEqual(rows[0], want) {
			t.Errorf("%s headers=%v", sheet, rows[0])
		}
		panes, err := f.GetPanes(sheet)
		if err != nil || !panes.Freeze || panes.YSplit != 1 || panes.TopLeftCell != "A2" {
			t.Errorf("panes=%+v %v", panes, err)
		}
	}
	assertFormat := func(sheet, cell, want string) {
		t.Helper()
		id, err := f.GetCellStyle(sheet, cell)
		if err != nil {
			t.Fatal(err)
		}
		style, err := f.GetStyle(id)
		if err != nil || style.CustomNumFmt == nil || *style.CustomNumFmt != want {
			t.Errorf("%s/%s style=%+v err=%v", sheet, cell, style, err)
		}
	}
	rows, _ := f.GetRows("Annual leave")
	found := false
	for i, row := range rows[1:] {
		if row[1] != "staff1" {
			continue
		}
		found = true
		cell := fmt.Sprintf("L%d", i+2)
		if got := payrollValue(t, f, "Annual leave", cell, true); got != "10.33" {
			t.Errorf("fraction=%s", got)
		}
		typ, err := f.GetCellType("Annual leave", cell)
		_, numericErr := strconv.ParseFloat(payrollValue(t, f, "Annual leave", cell, true), 64)
		// OOXML may omit t="n" for a number; excelize then reports Unset.
		if err != nil || (typ != excelize.CellTypeNumber && (typ != excelize.CellTypeUnset || numericErr != nil)) {
			t.Errorf("number type=%v %v", typ, err)
		}
		assertFormat("Annual leave", cell, "0.00;[Red]-0.00")
		if got := payrollValue(t, f, "Annual leave", fmt.Sprintf("O%d", i+2), true); got != "-2.17" {
			t.Errorf("change=%s", got)
		}
	}
	if !found {
		t.Fatal("staff missing")
	}
	if got := payrollValue(t, f, "About", "B2", false); got != "2026-10-05" {
		t.Errorf("as at=%s", got)
	}
	if _, err := strconv.ParseFloat(payrollValue(t, f, "About", "B2", true), 64); err != nil {
		t.Error("date not serial", err)
	}
	assertFormat("About", "B2", "yyyy-mm-dd")
	assertFormat("About", "B3", "yyyy-mm-dd hh:mm")
	all, _ := f.GetRows("All types")
	unlimited := 0
	for i, row := range all[1:] {
		if row[4] == "Unpaid leave" {
			unlimited++
			for _, col := range []string{"I", "M"} {
				if got := payrollValue(t, f, "All types", fmt.Sprintf("%s%d", col, i+2), true); got != "" {
					t.Errorf("unlimited=%q", got)
				}
			}
		}
	}
	if unlimited == 0 {
		t.Fatal("no unlimited rows")
	}
	props, err := f.GetDocProps()
	if err != nil || props.Creator != "Leave" || props.Title != "Leave balances for payroll" {
		t.Errorf("props=%+v %v", props, err)
	}
}

func TestPayrollStringsNeverFormulas(t *testing.T) {
	hs := newHarness(t)
	rep, err := hs.svc.Payroll(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	for i := range rep.People {
		rep.People[i].User.Name = `=HYPERLINK("http://x","y")`
		rep.People[i].User.Username = "+27 82..."
		rep.People[i].User.Email = "-1"
		rep.People[i].User.Department = "@SUM(A1)"
	}
	buf, err := payrollWorkbook(rep, hs.svc.Now(), "=admin")
	if err != nil {
		t.Fatal(err)
	}
	f := readPayroll(t, buf.Bytes())
	for cell, want := range map[string]string{"A2": `=HYPERLINK("http://x","y")`, "B2": "+27 82...", "C2": "-1", "D2": "@SUM(A1)"} {
		if got := payrollValue(t, f, "Annual leave", cell, false); got != want {
			t.Errorf("%s=%q", cell, got)
		}
		formula, err := f.GetCellFormula("Annual leave", cell)
		if err != nil || formula != "" {
			t.Errorf("formula=%q %v", formula, err)
		}
	}
	z, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatal(err)
	}
	sheets := 0
	for _, entry := range z.File {
		if !strings.HasPrefix(entry.Name, "xl/worksheets/sheet") || !strings.HasSuffix(entry.Name, ".xml") {
			continue
		}
		r, err := entry.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(r)
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte("<f>")) || bytes.Contains(data, []byte("<f ")) {
			t.Errorf("formula XML in %s", entry.Name)
		}
		sheets++
	}
	if sheets != 3 {
		t.Fatalf("checked %d sheets", sheets)
	}
	rep.People[0].User.Name = "control\x01name"
	buf, err = payrollWorkbook(rep, hs.svc.Now(), "Admin")
	if err != nil {
		t.Fatal(err)
	}
	readPayroll(t, buf.Bytes())
}

func TestPayrollAccessAndFailure(t *testing.T) {
	hs := newHarness(t)
	for _, tc := range []struct {
		who, path string
		want      int
	}{
		{"", "/admin/payroll.xlsx", 303}, {"staff1", "/admin/payroll.xlsx", 403},
		{"hr", "/admin/payroll.xlsx?include_disabled=yes", 400},
	} {
		c := hs.client()
		if tc.who != "" {
			c = hs.signIn(tc.who)
		}
		res, err := c.Get(hs.srv.URL + tc.path)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != tc.want {
			t.Errorf("%+v got %d", tc, res.StatusCode)
		}
		if tc.who == "" && !strings.HasPrefix(res.Header.Get("Location"), "/login?") {
			t.Error("missing login redirect")
		}
	}
	c := hs.signIn("hr")
	if _, err := hs.db.Exec(`UPDATE users SET disabled=1 WHERE username='staff1'`); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "?include_disabled=", "?include_disabled=1"} {
		res, err := c.Get(hs.srv.URL + "/admin/payroll.xlsx" + suffix)
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(res.Body)
		res.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		f := readPayroll(t, body)
		rows, _ := f.GetRows("Annual leave")
		found := false
		for _, row := range rows[1:] {
			if row[1] == "staff1" {
				found = true
				if row[4] != "Disabled" {
					t.Error(row)
				}
			}
		}
		if found != (suffix == "?include_disabled=1") {
			t.Errorf("disabled found=%v suffix=%s", found, suffix)
		}
	}
	// Drop only the test database's table, retaining the session lookup so the
	// error is in report construction, before any download headers are written.
	if _, err := hs.db.Exec(`DROP TABLE balance_adjustments`); err != nil {
		t.Fatal(err)
	}
	res, err := c.Get(hs.srv.URL + "/admin/payroll.xlsx")
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != 500 || strings.Contains(res.Header.Get("Content-Type"), "spreadsheet") || res.Header.Get("Content-Disposition") != "" {
		t.Errorf("failure=%d %v", res.StatusCode, res.Header)
	}
}

func TestPayrollNoAnnual(t *testing.T) {
	buf, err := payrollWorkbook(leave.PayrollReport{AsAt: time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)}, time.Now(), "Admin")
	if err != nil {
		t.Fatal(err)
	}
	f := readPayroll(t, buf.Bytes())
	rows, err := f.GetRows("Annual leave")
	if err != nil || len(rows) != 1 {
		t.Fatalf("header only: %v %v", rows, err)
	}
	rows, _ = f.GetRows("About")
	found := false
	for _, r := range rows {
		if len(r) > 1 && strings.Contains(r[1], "No annual leave type") {
			found = true
		}
	}
	if !found {
		t.Error("no annual explanation missing")
	}
}

func TestPayrollSize(t *testing.T) {
	hs := newHarness(t)
	ctx := context.Background()
	ts, err := hs.svc.Types(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	var annual int64
	for _, ty := range ts {
		if ty.Code == leave.AnnualCode {
			annual = ty.ID
		}
	}
	if _, err := hs.db.Exec(`UPDATE users SET disabled=1`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 200; i++ {
		u, err := hs.us.Upsert(ctx, directory.User{ID: fmt.Sprintf("payroll-%d", i), Username: fmt.Sprintf("payroll-%03d", i), Name: fmt.Sprintf("Payroll %03d", i), DN: fmt.Sprintf("CN=payroll-%d", i)})
		if err != nil {
			t.Fatal(err)
		}
		for j := 0; j < 3; j++ {
			_, err := hs.db.Exec(`INSERT INTO requests(user_id,type_id,kind,start_date,end_date,days_x100,cycle_start,status,created_at)
				VALUES (?,?,'leave',?,?,50,'2026-01-01','approved','2026-10-01')`, u.ID, annual, fmt.Sprintf("2026-09-%02d", j+1), fmt.Sprintf("2026-09-%02d", j+1))
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	start := time.Now()
	rep, err := hs.svc.Payroll(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	buf, err := payrollWorkbook(rep, hs.svc.Now(), "Admin")
	if err != nil {
		t.Fatal(err)
	}
	f := readPayroll(t, buf.Bytes())
	rows, err := f.GetRows("Annual leave")
	if err != nil || len(rows) != 201 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	for i := 2; i <= 201; i++ {
		if got := payrollValue(t, f, "Annual leave", fmt.Sprintf("L%d", i), true); got != "13.50" {
			t.Fatalf("row %d=%s", i, got)
		}
	}
	elapsed := time.Since(start)
	t.Logf("200-person payroll export, open and check: %s (%d bytes)", elapsed, buf.Len())
	if elapsed > 5*time.Second {
		t.Errorf("export exceeded loose 5s sanity bound: %s", elapsed)
	}
}
