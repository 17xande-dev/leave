package main

// The import2026 subcommand: back-fill the 2026 leave log and each person's
// October payroll balance from two CSV files (see the format below). It runs
// through the leave service, so every entry is an ordinary approved, recorded
// request that lands on the shared calendar, and the balance correction is an
// ordinary adjustment.
//
//	leave import2026 -leave=leave.csv -balances=balances.csv \
//	    -recorded-by=ADMIN -recorded-by-self=OTHER_ADMIN [-apply] [-force]
//
// leave.csv:    row,ext_id,type_code,start,end,half_start,half_end,note
// balances.csv: ext_id,available
//
// Without -apply nothing is written to the database: the run is made against
// a private copy of it (VACUUM INTO), which is thrown away, so the report is
// exactly what -apply would do and "dry run" is a fact about the file, not a
// promise in the code.

import (
	"context"
	"database/sql"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/17xande-dev/leave/internal/config"
	"github.com/17xande-dev/leave/internal/db"
	"github.com/17xande-dev/leave/internal/leave"
	"github.com/17xande-dev/leave/internal/notify"
	"github.com/17xande-dev/leave/internal/users"
)

// importMarker starts the decision note of every imported entry; it is how a
// second -apply is noticed.
const importMarker = "Imported from the 2026 leave log (row "

type leaveRow struct {
	row, ext, typeCode, start, end, halfStart, halfEnd, note string
}

type balanceRow struct{ ext, available string }

// runImport2026 is the whole subcommand; the return value is the exit code:
// 0 done (skips included), 1 an -apply whose verification failed, 2 the run
// could not start or stopped.
func runImport2026(args []string, stdout io.Writer, now func() time.Time) int {
	fs := flag.NewFlagSet("import2026", flag.ContinueOnError)
	fs.SetOutput(stdout)
	leavePath := fs.String("leave", "", "leave.csv")
	balPath := fs.String("balances", "", "balances.csv")
	by := fs.String("recorded-by", "", "admin (ext_id, username or email) who records every entry except their own")
	bySelf := fs.String("recorded-by-self", "", "a second admin, who records the first admin's own entries")
	apply := fs.Bool("apply", false, "write to the database (default: dry run)")
	force := fs.Bool("force", false, "apply even though entries were imported before")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stdout, "ERROR "+format+"\n", a...)
		return 2
	}
	if *leavePath == "" || *balPath == "" || *by == "" {
		return fail("-leave, -balances and -recorded-by are required")
	}
	leaves, err := readLeaveCSV(*leavePath)
	if err != nil {
		return fail("%v", err)
	}
	bals, err := readBalancesCSV(*balPath)
	if err != nil {
		return fail("%v", err)
	}

	cfg, err := config.Load()
	if err != nil {
		return fail("%v", err)
	}
	dbPath := cfg.DBPath
	if !*apply {
		tmp, err := os.MkdirTemp("", "leave-import-dryrun-")
		if err != nil {
			return fail("%v", err)
		}
		defer os.RemoveAll(tmp)
		if dbPath, err = copyDB(cfg.DBPath, filepath.Join(tmp, "copy.db")); err != nil {
			return fail("copy the database for the dry run: %v", err)
		}
	}
	database, err := db.Open(dbPath)
	if err != nil {
		return fail("%v", err)
	}
	defer database.Close()

	us := users.NewStore(database)
	svc := leave.New(database, us, leave.Config{Location: cfg.Location})
	svc.Now = now
	svc.OnEvent(notify.New(cfg.BaseURL).Hook)
	ctx := context.Background()
	if err := svc.SeedTypes(ctx); err != nil {
		return fail("%v", err)
	}
	imp := &importer{ctx: ctx, out: stdout, db: database, svc: svc, users: us}
	return imp.run(leaves, bals, *by, *bySelf, *apply, *force)
}

// copyDB writes a consistent copy of the SQLite database at src to dst. src is
// opened only to be read; a missing file is an error rather than an empty
// database quietly created.
func copyDB(src, dst string) (string, error) {
	if _, err := os.Stat(src); err != nil {
		return "", err
	}
	d, err := db.Open(src)
	if err != nil {
		return "", err
	}
	defer d.Close()
	if _, err := d.Exec(`VACUUM INTO ?`, dst); err != nil {
		return "", err
	}
	return dst, nil
}

type importer struct {
	ctx   context.Context
	out   io.Writer
	db    *sql.DB
	svc   *leave.Service
	users *users.Store
}

func (im *importer) printf(format string, a ...any) { fmt.Fprintf(im.out, format+"\n", a...) }

// resolveAdmin finds an enabled admin by ext_id, username or email.
func (im *importer) resolveAdmin(key string) (users.User, error) {
	for _, find := range []func(context.Context, string) (users.User, error){
		im.users.ByExtID, im.users.ByUsername, im.users.ByEmail} {
		u, err := find(im.ctx, key)
		if errors.Is(err, users.ErrNotFound) {
			continue
		}
		if err != nil {
			return users.User{}, err
		}
		if !u.Admin || u.Disabled {
			return users.User{}, fmt.Errorf("%q is not an enabled admin", key)
		}
		return u, nil
	}
	return users.User{}, fmt.Errorf("no user %q (tried ext_id, username, email)", key)
}

func (im *importer) run(leaves []leaveRow, bals []balanceRow, byKey, selfKey string, apply, force bool) int {
	fail := func(format string, a ...any) int {
		im.printf("ERROR "+format, a...)
		return 2
	}
	if apply {
		im.printf("APPLY: writing to the database.")
	} else {
		im.printf("DRY RUN: nothing is written to the database (the run is made on a throwaway copy).")
	}

	by, err := im.resolveAdmin(byKey)
	if err != nil {
		return fail("-recorded-by: %v", err)
	}
	var self users.User
	if selfKey != "" {
		if self, err = im.resolveAdmin(selfKey); err != nil {
			return fail("-recorded-by-self: %v", err)
		}
	}
	// The admin's own rows need a second admin; find out before writing anything.
	needSelf := false
	for _, r := range leaves {
		if u, err := im.users.ByExtID(im.ctx, r.ext); err == nil && u.ID == by.ID {
			needSelf = true
		}
	}
	for _, b := range bals {
		if u, err := im.users.ByExtID(im.ctx, b.ext); err == nil && u.ID == by.ID {
			needSelf = true
		}
	}
	if needSelf {
		switch {
		case selfKey == "":
			return fail("-recorded-by-self is required: the file has entries for %s, who cannot record their own", by.Name)
		case self.ID == by.ID:
			return fail("-recorded-by and -recorded-by-self are the same admin, but the file has entries for %s", by.Name)
		}
	}

	var prior int
	if err := im.db.QueryRowContext(im.ctx, `SELECT count(*) FROM requests WHERE substr(decision_note, 1, ?) = ?`,
		len(importMarker), importMarker).Scan(&prior); err != nil {
		return fail("%v", err)
	}
	if prior > 0 {
		if apply && !force {
			return fail("%d entries were already imported (decision note starts %q). Refusing to import again; -force overrides.", prior, importMarker)
		}
		im.printf("NOTE %d entries were already imported before this run.", prior)
	}

	annual, cycle, err := im.annualCycle()
	if err != nil {
		return fail("%v", err)
	}
	types, err := im.svc.Types(im.ctx, false)
	if err != nil {
		return fail("%v", err)
	}
	byCode := map[string]leave.Type{}
	for _, t := range types {
		byCode[t.Code] = t
	}
	actor := func(u users.User) users.User {
		if u.ID == by.ID {
			return self
		}
		return by
	}

	// People in order of first appearance, each person's entries by start date,
	// so earlier leave is in before later leave is checked against it.
	var order []string
	perPerson := map[string][]leaveRow{}
	for _, r := range leaves {
		if _, ok := perPerson[r.ext]; !ok {
			order = append(order, r.ext)
		}
		perPerson[r.ext] = append(perPerson[r.ext], r)
	}
	var imported, skipped int
	for _, ext := range order {
		rows := perPerson[ext]
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].start < rows[j].start })
		for _, r := range rows {
			if im.importRow(r, byCode, actor) {
				imported++
			} else {
				skipped++
			}
		}
	}

	var applied, balSkipped, mismatches int
	seen := map[string]bool{}
	type check struct {
		ext   string
		user  users.User
		want  leave.Days
		label string
	}
	var checks []check
	for _, b := range bals {
		if seen[b.ext] {
			im.printf("SKIP BAL ext_id=%s duplicate balance row", b.ext)
			balSkipped++
			continue
		}
		seen[b.ext] = true
		want, u, ok := im.balanceRow(b, annual, cycle, actor)
		if !ok {
			balSkipped++
			continue
		}
		applied++
		checks = append(checks, check{b.ext, u, want, b.available})
	}

	for _, c := range checks {
		bal, err := im.svc.BalanceIn(im.ctx, c.user.ID, annual, cycle)
		var got *leave.Days
		if err == nil {
			got = bal.Available()
		}
		if got == nil || *got != c.want {
			mismatches++
			im.printf("MISMATCH ext_id=%s available is %s, the file says %s: this is a bug, not something to fix by hand", c.ext, daysOrNone(got), c.want)
		}
	}

	im.printf("")
	im.printf("Leave:    %d imported, %d skipped", imported, skipped)
	im.printf("Balances: %d applied, %d skipped", applied, balSkipped)
	if mismatches > 0 {
		im.printf("Verification FAILED: %d balances do not equal the file.", mismatches)
		return 1
	}
	im.printf("Verification: %d balances equal the file exactly.", len(checks))
	if !apply {
		im.printf("This was a dry run. Add -apply to write it.")
	}
	return 0
}

func daysOrNone(d *leave.Days) string {
	if d == nil {
		return "unlimited"
	}
	return d.String()
}

// annualCycle is the annual type and the start of the cycle containing
// 1 January 2026, the one the opening balance belongs to.
func (im *importer) annualCycle() (leave.Type, time.Time, error) {
	types, err := im.svc.Types(im.ctx, false)
	if err != nil {
		return leave.Type{}, time.Time{}, err
	}
	st, err := im.svc.Settings(im.ctx)
	if err != nil {
		return leave.Type{}, time.Time{}, err
	}
	for _, t := range types {
		if t.Code == "annual" {
			if t.Allowance == nil {
				return leave.Type{}, time.Time{}, errors.New("the annual type has no allowance, so it has no balance to correct")
			}
			jan := time.Date(2026, time.January, 1, 0, 0, 0, 0, time.UTC)
			return t, leave.CycleStart(jan, t.CycleMonths, st.SickCycleAnchor), nil
		}
	}
	return leave.Type{}, time.Time{}, errors.New("there is no leave type with code \"annual\"")
}

// importRow records one entry and prints its line; false means skipped.
func (im *importer) importRow(r leaveRow, types map[string]leave.Type, actor func(users.User) users.User) bool {
	skip := func(format string, a ...any) bool {
		im.printf("SKIP row %s  "+format, append([]any{r.row}, a...)...)
		return false
	}
	t, ok := types[r.typeCode]
	if !ok {
		return skip("unknown type_code %q", r.typeCode)
	}
	u, err := im.users.ByExtID(im.ctx, r.ext)
	switch {
	case errors.Is(err, users.ErrNotFound):
		return skip("unknown ext_id %s", r.ext)
	case err != nil:
		return skip("%v", err)
	case u.Disabled:
		return skip("user is disabled (ext_id %s)", r.ext)
	}
	end := r.end
	if end == "" {
		end = r.start
	}
	hs, ok1 := halfFlag(r.halfStart)
	he, ok2 := halfFlag(r.halfEnd)
	if !ok1 || !ok2 {
		return skip(`half_start/half_end must be "1" or empty, got %q and %q`, r.halfStart, r.halfEnd)
	}
	note := fmt.Sprintf("%s%s).", importMarker, r.row)
	if n := strings.TrimSpace(r.note); n != "" {
		note += " " + n
	}
	in := leave.Input{TypeID: t.ID, Start: r.start, End: end, HalfStart: hs, HalfEnd: he}
	req, err := im.svc.ImportLeave(im.ctx, actor(u), u.ID, in, note)
	if err != nil {
		return skip("%s", reason(err))
	}
	im.printf("OK  row %s  ext_id=%s %s %s..%s (%s)", r.row, r.ext, t.Code, req.Start, req.End, req.Days.Label(t.Unit))
	return true
}

func halfFlag(s string) (v, ok bool) {
	switch strings.TrimSpace(s) {
	case "":
		return false, true
	case "1":
		return true, true
	}
	return false, false
}

// reason flattens a service error to one line for the report.
func reason(err error) string {
	var fe leave.FieldErrors
	if errors.As(err, &fe) {
		keys := make([]string, 0, len(fe))
		for k := range fe {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, 0, len(keys))
		for _, k := range keys {
			parts = append(parts, fe[k])
		}
		return strings.Join(parts, " ")
	}
	return err.Error()
}

// balanceRow adds the one adjustment that makes the person's available days
// exactly what the file says. It returns the target and the user, and whether
// the row was applied.
func (im *importer) balanceRow(b balanceRow, annual leave.Type, cycle time.Time, actor func(users.User) users.User) (leave.Days, users.User, bool) {
	skip := func(format string, a ...any) (leave.Days, users.User, bool) {
		im.printf("SKIP BAL ext_id=%s "+format, append([]any{b.ext}, a...)...)
		return 0, users.User{}, false
	}
	want, err := leave.ParseDays(b.available)
	if err != nil {
		return skip("available %q: %v", b.available, err)
	}
	u, err := im.users.ByExtID(im.ctx, b.ext)
	switch {
	case errors.Is(err, users.ErrNotFound):
		return skip("not found")
	case err != nil:
		return skip("%v", err)
	case u.Disabled:
		return skip("user is disabled")
	}
	bal, err := im.svc.BalanceIn(im.ctx, u.ID, annual, cycle)
	if err != nil {
		return skip("%v", err)
	}
	was := bal.Available()
	if was == nil {
		return skip("annual leave is unlimited")
	}
	adj := want - *was
	if adj != 0 {
		err = im.svc.AddAdjustment(im.ctx, actor(u), leave.Adjustment{
			UserID: u.ID, TypeID: annual.ID, CycleStart: cycle.Format("2006-01-02"), Days: adj,
			Reason: fmt.Sprintf("Opening balance from payroll, October 2026: D = %s.", strings.TrimSpace(b.available)),
		})
		if err != nil {
			return skip("adjustment refused: %s", reason(err))
		}
	}
	im.printf("BAL ext_id=%s available was %s, D=%s, adjustment %s", b.ext, was, want, adj)
	return want, u, true
}

// readCSV reads a header row and returns each record keyed by header name. A
// missing required column, a ragged row or bad quoting stops the whole run.
func readCSV(path string, required ...string) ([]map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	recs, err := csv.NewReader(f).ReadAll()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(recs) == 0 {
		return nil, fmt.Errorf("%s: empty file, expected a header row", path)
	}
	idx := map[string]int{}
	for i, h := range recs[0] {
		idx[strings.TrimPrefix(strings.TrimSpace(h), "\ufeff")] = i
	}
	for _, c := range required {
		if _, ok := idx[c]; !ok {
			return nil, fmt.Errorf("%s: header has no %q column", path, c)
		}
	}
	out := make([]map[string]string, 0, len(recs)-1)
	for _, rec := range recs[1:] {
		m := map[string]string{}
		for h, i := range idx {
			m[h] = strings.TrimSpace(rec[i])
		}
		out = append(out, m)
	}
	return out, nil
}

func readLeaveCSV(path string) ([]leaveRow, error) {
	recs, err := readCSV(path, "row", "ext_id", "type_code", "start", "end", "half_start", "half_end", "note")
	if err != nil {
		return nil, err
	}
	out := make([]leaveRow, len(recs))
	for i, m := range recs {
		out[i] = leaveRow{m["row"], m["ext_id"], m["type_code"], m["start"], m["end"], m["half_start"], m["half_end"], m["note"]}
	}
	return out, nil
}

func readBalancesCSV(path string) ([]balanceRow, error) {
	recs, err := readCSV(path, "ext_id", "available")
	if err != nil {
		return nil, err
	}
	out := make([]balanceRow, len(recs))
	for i, m := range recs {
		out[i] = balanceRow{m["ext_id"], m["available"]}
	}
	return out, nil
}
