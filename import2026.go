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
// -unrecord=unrecord.csv quietly removes entries an earlier run imported (no
// email, calendar event only); it must be given on its own, between two
// -leave/-balances runs.
//
// leave.csv:    row,ext_id,type_code,start,end,half_start,half_end,note
// balances.csv: ext_id,available
// unrecord.csv: row,ext_id
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
	"net/url"
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
const importMarker = leave.ImportedNotePrefix

type leaveRow struct {
	row, ext, typeCode, start, end, halfStart, halfEnd, note string
}

type balanceRow struct{ ext, available string }

type unrecordRow struct{ row, ext string }

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
	unPath := fs.String("unrecord", "", "unrecord.csv: imported entries to remove quietly (no email)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stdout, "ERROR "+format+"\n", a...)
		return 2
	}
	if *by == "" || (*leavePath == "") != (*balPath == "") || (*leavePath == "" && *unPath == "") {
		return fail("-recorded-by is required, with either -leave and -balances together, or -unrecord alone")
	}
	// No single order serves both: a remainder re-booked after an original is
	// refused as an overlap while the original is still there, and the
	// removal cannot come first without the balances seeing the old leave.
	// So the removal is its own run, between the two imports.
	if *unPath != "" && *leavePath != "" {
		return fail("-unrecord must run on its own, not together with -leave/-balances")
	}
	var (
		leaves []leaveRow
		bals   []balanceRow
		unrecs []unrecordRow
		err    error
	)
	if *leavePath != "" {
		if leaves, err = readLeaveCSV(*leavePath); err != nil {
			return fail("%v", err)
		}
		if bals, err = readBalancesCSV(*balPath); err != nil {
			return fail("%v", err)
		}
	}
	if *unPath != "" {
		if unrecs, err = readUnrecordCSV(*unPath); err != nil {
			return fail("%v", err)
		}
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
	return imp.run(leaves, bals, unrecs, *by, *bySelf, *apply, *force)
}

// copyDB writes a consistent copy of the SQLite database at src to dst. src is
// opened read-only and without db.Open's schema step, so nothing can be written
// to it, not even transiently; a missing file is an error rather than an empty
// database quietly created.
func copyDB(src, dst string) (string, error) {
	abs, err := filepath.Abs(src)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(abs); err != nil {
		return "", err
	}
	u := url.URL{Scheme: "file", Path: abs, RawQuery: "mode=ro"}
	d, err := sql.Open("sqlite3", u.String())
	if err != nil {
		return "", err
	}
	defer d.Close()
	if _, err := d.Exec(`VACUUM INTO ?`, dst); err != nil {
		return "", err
	}
	return dst, nil
}

// beforeVerify lets a test change a balance between the writes and the
// verification that reads them back; nil outside tests.
var beforeVerify func()

// What became of one row: imported/applied, skipped as expected messy data,
// held back for a person to look at, or an error that is not the data's fault.
type outcome int

const (
	outOK outcome = iota
	outSkip
	outHeld
	outErr
)

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
	finders := []func(context.Context, string) (users.User, error){im.users.ByExtID, im.users.ByUsername}
	if strings.Contains(key, "@") {
		// ByUsername drops everything after an @, so "alex@other.test" would
		// find "alex": an address is looked up as an address only.
		finders = []func(context.Context, string) (users.User, error){im.users.ByEmail}
	}
	for _, find := range finders {
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

func (im *importer) run(leaves []leaveRow, bals []balanceRow, unrecs []unrecordRow, byKey, selfKey string, apply, force bool) int {
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
	im.printf("Recording admin: %s (%s)", by.Name, by.Username)
	var self users.User
	if selfKey != "" {
		if self, err = im.resolveAdmin(selfKey); err != nil {
			return fail("-recorded-by-self: %v", err)
		}
		im.printf("Recording admin for their own entries: %s (%s)", self.Name, self.Username)
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
	for _, r := range unrecs {
		if u, err := im.users.ByExtID(im.ctx, r.ext); err == nil && u.ID == by.ID {
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
		// The refusal guards against importing leave twice; a run that only
		// removes entries (-unrecord alone) imports nothing, so it is exempt.
		if apply && !force && len(leaves)+len(bals) > 0 {
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
	var imported, skipped, errored int
	for _, ext := range order {
		rows := perPerson[ext]
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].start < rows[j].start })
		for _, r := range rows {
			switch im.importRow(r, byCode, actor) {
			case outOK:
				imported++
			case outSkip:
				skipped++
			default:
				errored++
			}
		}
	}

	// -unrecord runs alone (see runImport2026), so there are no imports or
	// balances in the same run.
	var unrecorded, unSkipped, unErrored int
	for _, r := range unrecs {
		switch im.unrecordRow(r, actor) {
		case outOK:
			unrecorded++
		case outSkip:
			unSkipped++
		default:
			unErrored++
		}
	}

	var applied, balSkipped, held, balErrored, mismatches int
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
		want, u, out := im.balanceRow(b, annual, cycle, actor)
		switch out {
		case outSkip:
			balSkipped++
			continue
		case outHeld:
			held++
			continue
		case outErr:
			balErrored++
			continue
		}
		applied++
		checks = append(checks, check{b.ext, u, want, b.available})
	}
	if beforeVerify != nil {
		beforeVerify()
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
	im.printf("Leave:    %d imported, %d skipped, %d errors", imported, skipped, errored)
	im.printf("Balances: %d applied, %d skipped, %d held for review, %d errors", applied, balSkipped, held, balErrored)
	if len(unrecs) > 0 {
		im.printf("Unrecord: %d unrecorded, %d skipped, %d errors", unrecorded, unSkipped, unErrored)
	}
	if errored+balErrored+unErrored > 0 {
		im.printf("FAILED: %d rows hit an error (not a skip): a database problem or a bad file. Fix it before trusting this run.", errored+balErrored+unErrored)
		return 1
	}
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

// importRow records one entry and prints its line. Only what the data
// itself can cause (unknown person or type, a refusal from the service's own
// rules) is a skip; anything else is an error.
func (im *importer) importRow(r leaveRow, types map[string]leave.Type, actor func(users.User) users.User) outcome {
	skip := func(format string, a ...any) outcome {
		im.printf("SKIP row %s  "+format, append([]any{r.row}, a...)...)
		return outSkip
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
		im.printf("ERROR row %s  %v", r.row, err)
		return outErr
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
	var fe leave.FieldErrors
	switch {
	case errors.As(err, &fe):
		return skip("%s", reason(err))
	case err != nil:
		im.printf("ERROR row %s  %v", r.row, err)
		return outErr
	}
	im.printf("OK  row %s  ext_id=%s %s %s..%s (%s)", r.row, r.ext, t.Code, req.Start, req.End, req.Days.Label(t.Unit))
	return outOK
}

// unrecordRow quietly removes the one entry the import made for a log row.
// Anything but exactly one match is a skip, never a guess: a row the first
// pass never imported (so there is nothing to remove, as when its day is not a
// work day), one already removed, or an ambiguous one.
func (im *importer) unrecordRow(r unrecordRow, actor func(users.User) users.User) outcome {
	skip := func(format string, a ...any) outcome {
		im.printf("SKIP UNREC row %s  "+format, append([]any{r.row}, a...)...)
		return outSkip
	}
	fail := func(format string, a ...any) outcome {
		im.printf("ERROR UNREC row %s  "+format, append([]any{r.row}, a...)...)
		return outErr
	}
	u, err := im.users.ByExtID(im.ctx, r.ext)
	switch {
	case errors.Is(err, users.ErrNotFound):
		return skip("unknown ext_id %s", r.ext)
	case err != nil:
		return fail("%v", err)
	case u.Disabled:
		return skip("user is disabled (ext_id %s)", r.ext)
	}
	// The note is "<marker><row>). <text>": match the start of it exactly
	// (substr, so a stray % or _ in the file is not a wildcard), so that row 1
	// never matches row 10.
	prefix := fmt.Sprintf("%s%s).", importMarker, r.row)
	rows, err := im.db.QueryContext(im.ctx, `SELECT id FROM requests WHERE user_id = ? AND status = 'approved'
		AND recorded_by_id IS NOT NULL AND substr(decision_note, 1, ?) = ?`, u.ID, len(prefix), prefix)
	if err != nil {
		return fail("%v", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return fail("%v", err)
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return fail("%v", err)
	}
	switch len(ids) {
	case 0:
		return skip("ext_id=%s: no imported request found to remove (never imported, or already removed)", r.ext)
	case 1:
	default:
		return skip("ext_id=%s: %d imported requests match, not guessing which to remove", r.ext, len(ids))
	}
	req, err := im.svc.UnimportLeave(im.ctx, actor(u), ids[0])
	var fe leave.FieldErrors
	switch {
	case errors.As(err, &fe):
		return skip("%s", reason(err))
	case err != nil:
		return fail("%v", err)
	}
	im.printf("OK  UNREC row %s  ext_id=%s %s..%s removed", r.row, r.ext, req.Start, req.End)
	return outOK
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
// exactly what the file says. It returns the target and the user. A person with
// 2026 annual leave that this run did not import is held back untouched: the
// arithmetic would absorb that leave into the opening balance.
func (im *importer) balanceRow(b balanceRow, annual leave.Type, cycle time.Time, actor func(users.User) users.User) (leave.Days, users.User, outcome) {
	skip := func(format string, a ...any) (leave.Days, users.User, outcome) {
		im.printf("SKIP BAL ext_id=%s "+format, append([]any{b.ext}, a...)...)
		return 0, users.User{}, outSkip
	}
	fail := func(format string, a ...any) (leave.Days, users.User, outcome) {
		im.printf("ERROR BAL ext_id=%s "+format, append([]any{b.ext}, a...)...)
		return 0, users.User{}, outErr
	}
	// A value that will not parse is the converter's bug, not messy data.
	want, err := leave.ParseDays(b.available)
	if err != nil {
		return fail("available %q: %v", b.available, err)
	}
	u, err := im.users.ByExtID(im.ctx, b.ext)
	switch {
	case errors.Is(err, users.ErrNotFound):
		return skip("not found")
	case err != nil:
		return fail("%v", err)
	case u.Disabled:
		return skip("user is disabled")
	}
	var pre int
	if err := im.db.QueryRowContext(im.ctx, `SELECT count(*) FROM requests WHERE user_id = ? AND type_id = ? AND cycle_start = ?
		AND kind = 'leave' AND status IN ('approved', 'pending') AND substr(decision_note, 1, ?) <> ?`,
		u.ID, annual.ID, cycle.Format("2006-01-02"), len(importMarker), importMarker).Scan(&pre); err != nil {
		return fail("%v", err)
	}
	if pre > 0 {
		im.printf("WARN ext_id=%s %d pre-existing 2026 annual request(s) found; balance adjustment skipped, review by hand", b.ext, pre)
		return 0, users.User{}, outHeld
	}
	bal, err := im.svc.BalanceIn(im.ctx, u.ID, annual, cycle)
	if err != nil {
		return fail("%v", err)
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
		var fe leave.FieldErrors
		switch {
		case errors.As(err, &fe):
			return skip("adjustment refused: %s", reason(err))
		case err != nil:
			return fail("adjustment: %v", err)
		}
	}
	im.printf("BAL ext_id=%s available was %s, D=%s, adjustment %s", b.ext, was, want, adj)
	return want, u, outOK
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

func readUnrecordCSV(path string) ([]unrecordRow, error) {
	recs, err := readCSV(path, "row", "ext_id")
	if err != nil {
		return nil, err
	}
	out := make([]unrecordRow, len(recs))
	for i, m := range recs {
		out[i] = unrecordRow{m["row"], m["ext_id"]}
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
