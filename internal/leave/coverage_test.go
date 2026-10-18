package leave

import (
	"strings"
	"testing"
	"time"

	"github.com/17xande-dev/leave/internal/directory"
)

// dept puts people in a department, keeping everything else the fixture gave
// them (Upsert writes the whole directory record).
func (f *fixture) dept(name string, who ...string) {
	f.t.Helper()
	for _, w := range who {
		p := f.people[w]
		u, err := f.us.Upsert(f.ctx, directory.User{ID: p.DN, DN: p.DN, Username: p.Username, Name: p.Name,
			Email: p.Email, ManagerDN: p.ManagerDN, Admin: p.Admin, Department: name})
		if err != nil {
			f.t.Fatal(err)
		}
		f.people[w] = u
	}
}

func (f *fixture) rule(department string, percent int) {
	f.t.Helper()
	if err := f.svc.SetCoverageRule(f.ctx, department, percent); err != nil {
		f.t.Fatal(err)
	}
}

// insertAway books leave for someone straight into the table, in any status,
// for the cases the service itself would not produce (a draft, a weekend).
func (f *fixture) insertAway(who, code, start, end, status string) {
	f.t.Helper()
	ty := f.types[code]
	_, err := f.db.Exec(`INSERT INTO requests (user_id, type_id, kind, start_date, end_date, days, cycle_start,
		status, created_at) VALUES (?, ?, 'leave', ?, ?, 1, '2026-01-01', ?, '2026-10-01T00:00:00Z')`,
		f.people[who].ID, ty.ID, start, end, status)
	if err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) override(who, code, start, end string) (Request, error) {
	return f.svc.Submit(f.ctx, f.people[who], Input{TypeID: f.types[code].ID, Start: start, End: end, Note: "n", Override: true})
}

func TestCoverageThreshold(t *testing.T) {
	f := newFixture(t)
	f.dept("Ops", "staff", "hr", "head", "orphan")
	f.rule("Ops", 50)

	// One of four, then two of four: 50% is "not more than" 50%.
	f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-14")
	f.mustSubmit("hr", "annual", "2026-10-12", "2026-10-14")

	// Three of four is over.
	_, err := f.submit("head", "annual", "2026-10-13", "2026-10-13")
	if !fieldErr(err, "coverage") {
		t.Fatalf("third person: %v", err)
	}
	msg := err.(FieldErrors)["coverage"]
	for _, want := range []string{"Ops", "3 of 4", "75%", "Tue 13 Oct", "limit is 50%", "special permission"} {
		if !strings.Contains(msg, want) {
			t.Errorf("coverage message lacks %q: %s", want, msg)
		}
	}
	// A day the others are not away is fine, and so is the preview, which
	// carries the warning for the form.
	f.mustSubmit("head", "annual", "2026-10-15", "2026-10-15")
	qt, err := f.svc.Preview(f.ctx, f.people["head"], Input{TypeID: f.types["annual"].ID, Start: "2026-10-13", End: "2026-10-13"})
	if !fieldErr(err, "coverage") || qt.Coverage == nil || qt.Coverage.Away != 3 || qt.Coverage.Size != 4 {
		t.Fatalf("preview: %+v %v", qt.Coverage, err)
	}
}

func TestCoverageTwoPeopleAtFiftyPercent(t *testing.T) {
	f := newFixture(t)
	f.dept("Pair", "staff", "hr")
	f.rule("Pair", 50)
	f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-12") // one of two: allowed
	if _, err := f.submit("hr", "annual", "2026-10-12", "2026-10-12"); !fieldErr(err, "coverage") {
		t.Fatalf("both of two: %v", err)
	}
}

func TestCoverageDefaultAndOwnRule(t *testing.T) {
	f := newFixture(t)
	f.dept("Kids", "staff", "hr", "head", "orphan")
	f.dept("Solo", "pastor")

	// No rule anywhere: no limit.
	f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-12")
	f.mustSubmit("hr", "annual", "2026-10-12", "2026-10-12")

	// The default applies to a department with no rule of its own: 25% of 4 is one person.
	f.rule("", 25)
	if _, err := f.submit("head", "annual", "2026-10-12", "2026-10-12"); !fieldErr(err, "coverage") {
		t.Fatalf("default rule ignored: %v", err)
	}
	// ...but a department's own rule replaces it, even a looser one.
	f.rule("Kids", 100)
	f.mustSubmit("head", "annual", "2026-10-12", "2026-10-12")
	// The first person away is always allowed: someone alone in their
	// department would otherwise be 100% away and could never take leave
	// without escalating it.
	f.rule("Kids", 0)
	f.mustSubmit("pastor", "annual", "2026-10-12", "2026-10-12")
	// A person with no department is never limited.
	f.dept("", "pastor")
	f.mustSubmit("pastor", "annual", "2026-10-13", "2026-10-13")

	rules, _ := f.svc.CoverageRules(f.ctx)
	if len(rules) != 1 || rules[""] != 25 {
		t.Fatalf("rules = %v, want only the default", rules)
	}
	if err := f.svc.SetCoverageRule(f.ctx, "Kids", 101); !fieldErr(err, "percent") {
		t.Fatalf("101%%: %v", err)
	}
	if err := f.svc.SetCoverageRule(f.ctx, "Kids", -1); !fieldErr(err, "percent") {
		t.Fatalf("-1%%: %v", err)
	}
}

func TestDepartmentsListsEnabledOnly(t *testing.T) {
	f := newFixture(t)
	f.dept("Kids", "staff", "hr")
	f.dept("Ops", "head")
	ds, err := f.svc.Departments(f.ctx)
	if err != nil || len(ds) != 2 || ds[0] != (Department{"Kids", 2}) || ds[1] != (Department{"Ops", 1}) {
		t.Fatalf("departments = %+v, %v", ds, err)
	}
}

func TestCoverageCountsPendingNotDrafts(t *testing.T) {
	f := newFixture(t)
	f.dept("Pair", "staff", "hr")
	f.rule("Pair", 50)

	// A draft is not away yet.
	f.insertAway("hr", "annual", "2026-10-12", "2026-10-12", "draft")
	f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-12")

	// Pending, approved and awaiting HR all are.
	for i, status := range []string{"pending", "approved", "awaiting_hr"} {
		day := time.Date(2026, 10, 19+i, 0, 0, 0, 0, time.UTC).Format(time.DateOnly)
		f.insertAway("hr", "annual", day, day, status)
		if _, err := f.submit("staff", "annual", day, day); !fieldErr(err, "coverage") {
			t.Errorf("%s leave did not count as away on %s: %v", status, day, err)
		}
	}
	// Declined and withdrawn do not.
	for i, status := range []string{"declined", "withdrawn", "cancelled"} {
		day := time.Date(2026, 10, 26+i, 0, 0, 0, 0, time.UTC).Format(time.DateOnly)
		f.insertAway("hr", "annual", day, day, status)
		f.mustSubmit("staff", "annual", day, day)
	}
}

func TestCoverageCountTypesNeverBlockedButCount(t *testing.T) {
	f := newFixture(t)
	f.dept("Pair", "staff", "hr")
	f.rule("Pair", 50)

	f.mustSubmit("staff", "sick", "2026-10-12", "2026-10-13")
	// Sick leave for the second person fills the whole department, and is not refused...
	f.mustSubmit("hr", "sick", "2026-10-12", "2026-10-13")
	// ...but it did count: annual leave on those days is.
	if _, err := f.submit("hr", "annual", "2026-10-14", "2026-10-14"); err != nil {
		t.Fatalf("a day nobody else is away: %v", err)
	}
	f.insertAway("staff", "sick", "2026-10-16", "2026-10-16", "pending")
	if _, err := f.submit("hr", "annual", "2026-10-16", "2026-10-16"); !fieldErr(err, "coverage") {
		t.Fatalf("sick leave did not count as away: %v", err)
	}
}

func TestCoverageSkipsDaysOffAndHolidays(t *testing.T) {
	f := newFixture(t)
	f.dept("Pair", "staff", "hr")
	f.rule("Pair", 50)

	// hr is away on 16 Dec, a public holiday: the day costs nothing and is not checked.
	if err := f.svc.EnsureHolidays(f.ctx, 2026); err != nil {
		t.Fatal(err)
	}
	f.insertAway("hr", "annual", "2026-12-16", "2026-12-16", "pending")
	f.mustSubmit("staff", "annual", "2026-12-16", "2026-12-17")

	// A weekend off is taken on days the working week skips, so there every day counts.
	f.insertAway("hr", "weekend", "2026-10-17", "2026-10-18", "pending")
	staff := f.people["staff"]
	c, err := f.svc.coverage(f.ctx, f.db, staff, f.types["weekend"], civil(2026, 10, 17), civil(2026, 10, 18), nil, 0)
	if err != nil || c == nil || c.Day != "2026-10-17" {
		t.Fatalf("weekend coverage = %+v, %v", c, err)
	}
	// The same days under a working-day type check nothing.
	c, err = f.svc.coverage(f.ctx, f.db, staff, f.types["annual"], civil(2026, 10, 17), civil(2026, 10, 18), nil, 0)
	if err != nil || c != nil {
		t.Fatalf("workday coverage over a weekend = %+v, %v", c, err)
	}
}

func TestCoverageShutdownExempt(t *testing.T) {
	f := newFixture(t)
	f.dept("Pair", "staff", "hr")
	f.rule("Pair", 50)
	f.december(false)

	for _, who := range []string{"staff", "hr"} {
		d := f.drafts(who)[0]
		if _, err := f.svc.SubmitDraft(f.ctx, f.people[who], d.ID, ""); err != nil {
			t.Fatalf("%s's shutdown request was refused: %v", who, err)
		}
	}
	// Both are away on those days now, and that counts for anyone else's cover.
	f.dept("Pair", "head")
	f.rule("Pair", 60)
	c, err := f.svc.coverage(f.ctx, f.db, f.people["head"], f.types["annual"], civil(2026, 12, 8), civil(2026, 12, 8), nil, 0)
	if err != nil || c == nil || c.Away != 3 {
		t.Fatalf("shutdown leave did not count as away: %+v %v", c, err)
	}
}

// staff, then their head, then the final approver, each approving in turn.
func TestEscalationChain(t *testing.T) {
	f := newFixture(t)
	f.dept("Pair", "staff", "hr")
	f.rule("Pair", 50)
	f.mustSubmit("hr", "annual", "2026-10-12", "2026-10-14")
	if _, err := f.submit("staff", "annual", "2026-10-12", "2026-10-14"); !fieldErr(err, "coverage") {
		t.Fatalf("not over the limit: %v", err)
	}
	f.events = nil

	r, err := f.override("staff", "annual", "2026-10-12", "2026-10-14")
	if err != nil {
		t.Fatal(err)
	}
	if !r.Escalate || r.Status != StatusPending || r.ApproverID != f.people["head"].ID {
		t.Fatalf("escalated request = %+v", r)
	}
	if len(f.events) != 1 || f.events[0].Kind != EventSubmitted || f.events[0].Coverage == nil {
		t.Fatalf("submit events = %+v", f.events)
	}
	// Reserved as any pending request is.
	if got := f.available("staff", "annual"); got != 12 {
		t.Fatalf("available while pending = %d", got)
	}

	// The head approves: it moves to the pastor, still pending, and they are told.
	f.events = nil
	r, err = f.svc.Decide(f.ctx, f.people["head"], r.ID, true, "go on")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusPending || r.ApproverID != f.people["pastor"].ID || r.DecidedByID != 0 {
		t.Fatalf("after the first approval = %+v", r)
	}
	if len(f.events) != 1 || f.events[0].Kind != EventEscalated || f.events[0].Request.ApproverID != f.people["pastor"].ID ||
		f.events[0].Coverage == nil || len(f.events[0].Steps) != 1 {
		t.Fatalf("escalation events = %+v", f.events)
	}
	// The head has no say any more; the pastor does.
	if _, err := f.svc.Decide(f.ctx, f.people["head"], r.ID, true, ""); err != ErrNotAllowed {
		t.Fatalf("head deciding twice: %v", err)
	}
	next, ok, err := f.svc.NextApprover(f.ctx, f.people["pastor"], r)
	if err != nil || ok {
		t.Fatalf("the final approver's approval should be last: %v %v %v", next, ok, err)
	}

	f.events = nil
	r, err = f.svc.Decide(f.ctx, f.people["pastor"], r.ID, true, "")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusApproved || r.DecidedByID != f.people["pastor"].ID {
		t.Fatalf("after the final approval = %+v", r)
	}
	if len(f.events) != 1 || f.events[0].Kind != EventDecided {
		t.Fatalf("final events = %+v", f.events)
	}
	steps, err := f.svc.Steps(f.ctx, r.ID)
	if err != nil || len(steps) != 2 || steps[0].ApproverName != "head" || steps[0].Decision != "approved" ||
		steps[0].Note != "go on" || steps[1].ApproverName != "pastor" {
		t.Fatalf("steps = %+v, %v", steps, err)
	}
	if got := f.available("staff", "annual"); got != 12 {
		t.Fatalf("available once approved = %d", got)
	}
}

func TestEscalationDeclineMidChain(t *testing.T) {
	f := newFixture(t)
	f.dept("Pair", "staff", "hr")
	f.rule("Pair", 50)
	f.mustSubmit("hr", "annual", "2026-10-12", "2026-10-14")
	r, _ := f.override("staff", "annual", "2026-10-12", "2026-10-14")
	r, _ = f.svc.Decide(f.ctx, f.people["head"], r.ID, true, "")

	if _, err := f.svc.Decide(f.ctx, f.people["pastor"], r.ID, false, ""); !fieldErr(err, "note") {
		t.Fatalf("decline without a note: %v", err)
	}
	r, err := f.svc.Decide(f.ctx, f.people["pastor"], r.ID, false, "we are short that week")
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != StatusDeclined || r.DecisionNote != "we are short that week" {
		t.Fatalf("declined request = %+v", r)
	}
	steps, _ := f.svc.Steps(f.ctx, r.ID)
	if len(steps) != 2 || steps[1].Decision != "declined" {
		t.Fatalf("steps = %+v", steps)
	}
	if got := f.available("staff", "annual"); got != 15 {
		t.Fatalf("declined leave still reserved: %d", got)
	}
}

func TestNonEscalatedRequestIsSingleStep(t *testing.T) {
	f := newFixture(t)
	r := f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-14")
	if r.Escalate {
		t.Fatal("an ordinary request is marked escalate")
	}
	r, err := f.svc.Decide(f.ctx, f.people["head"], r.ID, true, "")
	if err != nil || r.Status != StatusApproved {
		t.Fatalf("one approval: %+v %v", r, err)
	}
	if steps, _ := f.svc.Steps(f.ctx, r.ID); len(steps) != 0 {
		t.Fatalf("an ordinary decision logged steps: %+v", steps)
	}
}

// Asking for permission nobody needed costs nothing: it is an ordinary request.
func TestOverrideWhenNotOverTheLimitIsPlain(t *testing.T) {
	f := newFixture(t)
	f.dept("Pair", "staff", "hr")
	f.rule("Pair", 50)
	r, err := f.override("staff", "annual", "2026-10-12", "2026-10-14")
	if err != nil || r.Escalate {
		t.Fatalf("override with room to spare: %+v %v", r, err)
	}
	if r, _ = f.svc.Decide(f.ctx, f.people["head"], r.ID, true, ""); r.Status != StatusApproved {
		t.Fatalf("not single-step: %+v", r)
	}
	// And it cannot be used to skip another rule.
	if _, err := f.override("staff", "annual", "2026-10-10", "2026-10-11"); !fieldErr(err, "end") {
		t.Fatalf("override of a weekend-only request: %v", err)
	}
}

// The final approver's own leave goes to the admins, and one admin finishes it.
func TestEscalationInAdminQueueFinishesInOneDecision(t *testing.T) {
	f := newFixture(t)
	f.dept("Lead", "pastor", "orphan")
	f.rule("Lead", 50)
	f.mustSubmit("orphan", "annual", "2026-10-12", "2026-10-12")
	r, err := f.override("pastor", "annual", "2026-10-12", "2026-10-12")
	if err != nil || !r.Escalate || r.ApproverID != 0 {
		t.Fatalf("pastor's escalated request: %+v %v", r, err)
	}
	r, err = f.svc.Decide(f.ctx, f.people["hr"], r.ID, true, "")
	if err != nil || r.Status != StatusApproved {
		t.Fatalf("admin approval: %+v %v", r, err)
	}
}

// The chain stops rather than hand a request back to the person who made it.
func TestEscalationStopsAtTheRequester(t *testing.T) {
	f := newFixture(t)
	f.dept("Pair", "staff", "orphan")
	f.rule("Pair", 50)
	// staff -> hr -> head -> (override) staff.
	if err := f.svc.SetOverride(f.ctx, f.people["staff"].ID, f.people["hr"].ID); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.SetOverride(f.ctx, f.people["head"].ID, f.people["staff"].ID); err != nil {
		t.Fatal(err)
	}
	f.mustSubmit("orphan", "annual", "2026-10-12", "2026-10-12")
	r, err := f.override("staff", "annual", "2026-10-12", "2026-10-12")
	if err != nil || r.ApproverID != f.people["hr"].ID {
		t.Fatalf("%+v %v", r, err)
	}
	if r, err = f.svc.Decide(f.ctx, f.people["hr"], r.ID, true, ""); err != nil || r.ApproverID != f.people["head"].ID {
		t.Fatalf("first level: %+v %v", r, err)
	}
	if r, err = f.svc.Decide(f.ctx, f.people["head"], r.ID, true, ""); err != nil || r.Status != StatusApproved {
		t.Fatalf("head's approval should have been the last: %+v %v", r, err)
	}
}

func TestEscalatedRequestCoverageIsRecomputed(t *testing.T) {
	f := newFixture(t)
	f.dept("Pair", "staff", "hr")
	f.rule("Pair", 50)
	hr := f.mustSubmit("hr", "annual", "2026-10-12", "2026-10-14")
	r, _ := f.override("staff", "annual", "2026-10-12", "2026-10-14")
	c, err := f.svc.CoverageFor(f.ctx, r)
	if err != nil || c == nil || c.Away != 2 || c.Size != 2 || c.Percent() != 100 {
		t.Fatalf("coverage while hr is away: %+v %v", c, err)
	}
	if want := "100% of Pair (2 of 2) would be away on Mon 12 Oct. The limit is 50%."; c.Sentence() != want {
		t.Fatalf("sentence = %q", c.Sentence())
	}
	// Once hr's request is declined the warning goes away.
	f.svc.Decide(f.ctx, f.people["head"], hr.ID, false, "no")
	if c, _ := f.svc.CoverageFor(f.ctx, r); c != nil {
		t.Fatalf("stale warning: %+v", c)
	}
}
