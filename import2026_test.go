package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/17xande-dev/leave/internal/db"
	"github.com/17xande-dev/leave/internal/directory"
	"github.com/17xande-dev/leave/internal/leave"
	"github.com/17xande-dev/leave/internal/users"
)

type impEnv struct {
	t    *testing.T
	path string
}

// newImpEnv makes a database file holding two admins (A1 "alex", M1 "mel") and
// two staff (S1, S2), and points the subcommand's config at it.
func newImpEnv(t *testing.T) *impEnv {
	t.Helper()
	path := filepath.Join(t.TempDir(), "leave.db")
	t.Setenv("DB_PATH", path)
	t.Setenv("DIRECTORY", "fake")
	t.Setenv("COOKIE_SECURE", "false")
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	us := users.NewStore(d)
	for _, p := range []directory.User{
		{ID: "A1", DN: "CN=alex", Username: "alex", Name: "Alex Admin", Email: "alex@x.test", Admin: true},
		{ID: "M1", DN: "CN=mel", Username: "mel", Name: "Mel Admin", Email: "mel@x.test", Admin: true},
		{ID: "S1", DN: "CN=sam", Username: "sam", Name: "Sam Staff", Email: "sam@x.test"},
		{ID: "S2", DN: "CN=tess", Username: "tess", Name: "Tess Staff", Email: "tess@x.test"},
	} {
		if _, err := us.Upsert(context.Background(), p); err != nil {
			t.Fatal(err)
		}
	}
	return &impEnv{t: t, path: path}
}

func now() time.Time { return time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC) }

func (e *impEnv) run(args ...string) (int, string) {
	e.t.Helper()
	var out bytes.Buffer
	code := runImport2026(args, &out, now)
	return code, out.String()
}

func (e *impEnv) count(q string, args ...any) int {
	e.t.Helper()
	d, err := db.Open(e.path)
	if err != nil {
		e.t.Fatal(err)
	}
	defer d.Close()
	var n int
	if err := d.QueryRow(q, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *impEnv) available(ext string) *leave.Days {
	e.t.Helper()
	d, err := db.Open(e.path)
	if err != nil {
		e.t.Fatal(err)
	}
	defer d.Close()
	us := users.NewStore(d)
	svc := leave.New(d, us, leave.Config{})
	svc.Now = now
	u, err := us.ByExtID(context.Background(), ext)
	if err != nil {
		e.t.Fatal(err)
	}
	ts, _ := svc.Types(context.Background(), false)
	for _, t := range ts {
		if t.Code == "annual" {
			cs, _ := leave.ParseDate("2026-01-01")
			b, err := svc.BalanceIn(context.Background(), u.ID, t, cs)
			if err != nil {
				e.t.Fatal(err)
			}
			return b.Available()
		}
	}
	e.t.Fatal("no annual type")
	return nil
}

func writeCSV(t *testing.T, name, body string) string {
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const fixtureLeave, fixtureBal = "testdata/import2026/leave.csv", "testdata/import2026/balances.csv"

func (e *impEnv) fixture(extra ...string) (int, string) {
	return e.run(append([]string{"-leave=" + fixtureLeave, "-balances=" + fixtureBal,
		"-recorded-by=alex@x.test", "-recorded-by-self=M1"}, extra...)...)
}

func want(t *testing.T, out string, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if !strings.Contains(out, s) {
			t.Errorf("output lacks %q:\n%s", s, out)
		}
	}
}

func TestImport2026DryRunWritesNothing(t *testing.T) {
	e := newImpEnv(t)
	req0 := e.count(`SELECT count(*) FROM requests`)
	code, out := e.fixture()
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	want(t, out, "DRY RUN", "OK  row 2  ext_id=S1 sick 2026-03-04..2026-03-04 (1 working day)",
		"OK  row 3  ext_id=S1 annual 2026-05-04..2026-05-08 (4.5 working days)",
		"Leave:    4 imported, 6 skipped", "Balances: 3 applied, 1 skipped", "Verification: 3 balances equal the file exactly.")
	for _, table := range []string{"requests", "balance_adjustments", "outbox"} {
		if n := e.count(`SELECT count(*) FROM ` + table); n != 0 {
			t.Errorf("dry run left %d rows in %s", n, table)
		}
	}
	if n := e.count(`SELECT count(*) FROM requests`); n != req0 {
		t.Errorf("requests changed from %d to %d", req0, n)
	}
}

func TestImport2026Apply(t *testing.T) {
	e := newImpEnv(t)
	code, out := e.fixture("-apply")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	want(t, out, "APPLY",
		"SKIP row 4  unknown ext_id S9",
		`SKIP row 5  unknown type_code "bogus"`,
		"SKIP row 6  They already have leave booked",
		"SKIP row 7  half_start/half_end must be",
		"OK  row 8  ext_id=S2 maternity 2026-09-01..2026-11-29", // ends after today: allowed
		"SKIP row 9  The last day is more than a year away",
		"OK  row 10  ext_id=A1 annual 2026-07-06..2026-07-07",
		"SKIP row 11  A single request can cover at most",
		"SKIP BAL ext_id=S9 not found")

	for ext, d := range map[string]string{"S1": "14.6", "S2": "-1.44", "A1": "10.33"} {
		w, _ := leave.ParseDays(d)
		if got := e.available(ext); got == nil || *got != w {
			t.Errorf("%s available = %v, want %s", ext, got, d)
		}
	}
	// Alex's own row is recorded by Mel, everyone else's by Alex.
	if n := e.count(`SELECT count(*) FROM requests r JOIN users a ON a.id = r.recorded_by_id JOIN users u ON u.id = r.user_id
		WHERE (u.ext_id = 'A1' AND a.ext_id = 'M1') OR (u.ext_id <> 'A1' AND a.ext_id = 'A1')`); n != 4 {
		t.Errorf("%d of 4 imported rows have the right recorder", n)
	}
	if n := e.count(`SELECT count(*) FROM requests WHERE status = 'approved' AND decision_note LIKE 'Imported from the 2026 leave log (row %' AND note <> ''`); n != 4 {
		t.Errorf("%d imported requests with note and marker, want 4", n)
	}
	// Calendar only: one job per entry, never an email.
	if n := e.count(`SELECT count(*) FROM outbox WHERE kind = 'calendar_create'`); n != 4 {
		t.Errorf("%d calendar jobs, want 4", n)
	}
	if n := e.count(`SELECT count(*) FROM outbox WHERE kind <> 'calendar_create'`); n != 0 {
		t.Errorf("%d non-calendar jobs: someone was emailed", n)
	}
	if n := e.count(`SELECT count(*) FROM balance_adjustments WHERE reason LIKE 'Opening balance from payroll, October 2026: D = %.'`); n != 3 {
		t.Errorf("%d adjustments, want 3", n)
	}
}

func TestImport2026SecondApplyRefusedUntilForced(t *testing.T) {
	e := newImpEnv(t)
	if code, out := e.fixture("-apply"); code != 0 {
		t.Fatalf("first: %d\n%s", code, out)
	}
	reqs, adjs := e.count(`SELECT count(*) FROM requests`), e.count(`SELECT count(*) FROM balance_adjustments`)
	code, out := e.fixture("-apply")
	if code != 2 {
		t.Fatalf("second apply exit %d, want 2:\n%s", code, out)
	}
	want(t, out, "4 entries were already imported", "-force")
	if e.count(`SELECT count(*) FROM requests`) != reqs {
		t.Error("the refused run wrote requests")
	}
	// A dry run still works and mentions it.
	if code, out := e.fixture(); code != 0 {
		t.Fatalf("dry run after: %d\n%s", code, out)
	} else {
		want(t, out, "4 entries were already imported")
	}
	// -force: duplicates overlap and are skipped, the balances already match so
	// no second adjustment is made.
	code, out = e.fixture("-apply", "-force")
	if code != 0 {
		t.Fatalf("forced: %d\n%s", code, out)
	}
	want(t, out, "Leave:    0 imported, 10 skipped", "adjustment 0")
	if e.count(`SELECT count(*) FROM requests`) != reqs || e.count(`SELECT count(*) FROM balance_adjustments`) != adjs {
		t.Error("the forced rerun duplicated rows")
	}
}

func TestImport2026RecorderRules(t *testing.T) {
	leaveCSV := func(ext string) string {
		return "row,ext_id,type_code,start,end,half_start,half_end,note\n7," + ext + ",annual,2026-07-06,2026-07-07,,,\n"
	}
	bal := writeCSV(t, "b.csv", "ext_id,available\n")

	t.Run("nobody's own rows: only -recorded-by needed, and a bad -recorded-by-self still fails", func(t *testing.T) {
		e := newImpEnv(t)
		l := writeCSV(t, "l.csv", leaveCSV("S1"))
		if code, out := e.run("-leave="+l, "-balances="+bal, "-recorded-by=alex"); code != 0 {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		if code, out := e.run("-leave="+l, "-balances="+bal, "-recorded-by=alex", "-recorded-by-self=sam"); code != 2 {
			t.Fatalf("non-admin self: exit %d:\n%s", code, out)
		}
	})
	t.Run("same admin twice is fine without a row for them", func(t *testing.T) {
		e := newImpEnv(t)
		l := writeCSV(t, "l.csv", leaveCSV("S1"))
		if code, out := e.run("-leave="+l, "-balances="+bal, "-recorded-by=alex", "-recorded-by-self=A1"); code != 0 {
			t.Fatalf("exit %d:\n%s", code, out)
		}
	})
	t.Run("same admin twice errors when their own row is there", func(t *testing.T) {
		e := newImpEnv(t)
		l := writeCSV(t, "l.csv", leaveCSV("A1"))
		code, out := e.run("-leave="+l, "-balances="+bal, "-recorded-by=alex", "-recorded-by-self=A1", "-apply")
		if code != 2 || !strings.Contains(out, "the same admin") {
			t.Fatalf("exit %d:\n%s", code, out)
		}
		if e.count(`SELECT count(*) FROM requests`) != 0 {
			t.Error("wrote before refusing")
		}
	})
	t.Run("their own row with no -recorded-by-self errors", func(t *testing.T) {
		e := newImpEnv(t)
		l := writeCSV(t, "l.csv", leaveCSV("A1"))
		code, out := e.run("-leave="+l, "-balances="+bal, "-recorded-by=alex")
		if code != 2 || !strings.Contains(out, "-recorded-by-self is required") {
			t.Fatalf("exit %d:\n%s", code, out)
		}
	})
	t.Run("an unknown admin stops the run", func(t *testing.T) {
		e := newImpEnv(t)
		l := writeCSV(t, "l.csv", leaveCSV("S1"))
		if code, out := e.run("-leave="+l, "-balances="+bal, "-recorded-by=nobody"); code != 2 {
			t.Fatalf("exit %d:\n%s", code, out)
		}
	})
}

func TestImport2026UnreadableCSVStopsTheRun(t *testing.T) {
	e := newImpEnv(t)
	bad := writeCSV(t, "l.csv", "row,ext_id\n1,S1,extra\n")
	if code, out := e.run("-leave="+bad, "-balances="+fixtureBal, "-recorded-by=alex"); code != 2 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	noCol := writeCSV(t, "l.csv", "row,ext_id\n1,S1\n")
	if code, _ := e.run("-leave="+noCol, "-balances="+fixtureBal, "-recorded-by=alex"); code != 2 {
		t.Fatalf("missing columns: exit %d", code)
	}
}

// A balance that already equals the file needs no adjustment; one far from it
// gets exactly the difference, and an absurd one is refused, not mangled.
func TestImport2026BalanceArithmetic(t *testing.T) {
	e := newImpEnv(t)
	l := writeCSV(t, "l.csv", "row,ext_id,type_code,start,end,half_start,half_end,note\n")
	b := writeCSV(t, "b.csv", "ext_id,available\nS1,15\nS2,100000\n")
	code, out := e.run("-leave="+l, "-balances="+b, "-recorded-by=alex", "-apply")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	want(t, out, "ext_id=S1 available was 15, D=15, adjustment 0", "SKIP BAL ext_id=S2 adjustment refused")
	if n := e.count(`SELECT count(*) FROM balance_adjustments`); n != 0 {
		t.Errorf("%d adjustments, want none", n)
	}
}
