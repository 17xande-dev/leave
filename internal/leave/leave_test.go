package leave

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/17xande-dev/leave/internal/dbtest"
	"github.com/17xande-dev/leave/internal/directory"
	"github.com/17xande-dev/leave/internal/users"
)

type fixture struct {
	t      *testing.T
	ctx    context.Context
	db     *sql.DB
	svc    *Service
	us     *users.Store
	events []Event
	people map[string]users.User
	types  map[string]Type
}

// The org chart the tests run against:
//
//	pastor (the final approver, set in newFixture)
//	├── head (department head)
//	│   ├── staff
//	│   └── hr (admin)
//	└── orphan has no manager at all
func newFixture(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	d := dbtest.New(t)
	us := users.NewStore(d)
	jhb, _ := time.LoadLocation("Africa/Johannesburg")
	svc := New(d, us, Config{Location: jhb})
	// Pinned to Monday 5 October 2026, 10:00 in Johannesburg.
	svc.Now = func() time.Time { return time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC) }
	f := &fixture{t: t, ctx: ctx, db: d, svc: svc, us: us, people: map[string]users.User{}, types: map[string]Type{}}
	svc.OnEvent(func(_ context.Context, _ *sql.Tx, e Event) error {
		f.events = append(f.events, e)
		return nil
	})

	dn := func(n string) string {
		if n == "" {
			return ""
		}
		return "CN=" + n + ",OU=Staff"
	}
	for _, p := range []struct {
		name, manager string
		admin         bool
	}{
		{"pastor", "", false}, {"head", "pastor", false}, {"staff", "head", false},
		{"hr", "head", true}, {"orphan", "", false},
	} {
		u, err := us.Upsert(ctx, directory.User{ID: dn(p.name), DN: dn(p.name), Username: p.name, Name: p.name,
			Email: p.name + "@example.test", ManagerDN: dn(p.manager), Admin: p.admin})
		if err != nil {
			t.Fatal(err)
		}
		f.people[p.name] = u
	}
	f.setSettings(func(st *Settings) { st.FinalApproverID = f.people["pastor"].ID })
	if err := svc.SeedTypes(ctx); err != nil {
		t.Fatal(err)
	}
	ts, _ := svc.Types(ctx, false)
	for _, ty := range ts {
		f.types[ty.Code] = ty
	}
	return f
}

// setSettings changes the settings the way an admin would, through
// SaveSettings, so the tests exercise the same validation and the same row the
// pages do.
func (f *fixture) setSettings(change func(*Settings)) {
	f.t.Helper()
	st, err := f.svc.Settings(f.ctx)
	if err != nil {
		f.t.Fatal(err)
	}
	change(&st)
	if err := f.svc.SaveSettings(f.ctx, f.people["hr"], st); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) submit(who, code, start, end string) (Request, error) {
	return f.svc.Submit(f.ctx, f.people[who], Input{TypeID: f.types[code].ID, Start: start, End: end, Note: "n"})
}

func (f *fixture) mustSubmit(who, code, start, end string) Request {
	f.t.Helper()
	r, err := f.submit(who, code, start, end)
	if err != nil {
		f.t.Fatalf("submit %s %s %s..%s: %v", who, code, start, end, err)
	}
	return r
}

func (f *fixture) available(who, code string) int {
	f.t.Helper()
	bs, err := f.svc.Balances(f.ctx, f.people[who].ID)
	if err != nil {
		f.t.Fatal(err)
	}
	for _, b := range bs {
		if b.Type.Code == code {
			return *b.Available()
		}
	}
	f.t.Fatalf("no %s balance", code)
	return 0
}

func fieldErr(err error, field string) bool {
	var fe FieldErrors
	return errors.As(err, &fe) && fe[field] != ""
}

func TestApproverChain(t *testing.T) {
	f := newFixture(t)
	for who, want := range map[string]string{
		"staff":  "head",   // a department member goes to their head
		"head":   "pastor", // a head goes to the senior person above them
		"orphan": "pastor", // no manager: the final approver
		"pastor": "",       // the final approver: the admins' queue
	} {
		got, err := f.svc.ResolveApprover(f.ctx, f.people[who])
		if err != nil {
			t.Fatal(err)
		}
		if want == "" && got != 0 || want != "" && got != f.people[want].ID {
			t.Errorf("%s's approver = %d, want %s", who, got, want)
		}
	}

	// An override wins over the directory...
	if err := f.svc.SetOverride(f.ctx, f.people["staff"].ID, f.people["pastor"].ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := f.svc.ResolveApprover(f.ctx, f.people["staff"]); got != f.people["pastor"].ID {
		t.Errorf("override ignored: %d", got)
	}
	// ...and a self-override is refused.
	if err := f.svc.SetOverride(f.ctx, f.people["staff"].ID, f.people["staff"].ID); !fieldErr(err, "approver") {
		t.Errorf("self-override: %v", err)
	}

	// A disabled manager is skipped rather than left holding requests nobody
	// can act on.
	all := []directory.User{}
	for _, n := range []string{"pastor", "staff", "hr", "orphan"} {
		u := f.people[n]
		all = append(all, directory.User{ID: u.DN, DN: u.DN, Username: u.Username, Name: u.Name, ManagerDN: u.ManagerDN, Admin: u.Admin})
	}
	f.us.Sync(f.ctx, all) // head leaves
	hr, _ := f.us.ByUsername(f.ctx, "hr")
	if got, _ := f.svc.ResolveApprover(f.ctx, hr); got != f.people["pastor"].ID {
		t.Errorf("with head disabled, hr's approver = %d, want pastor", got)
	}
}

func TestSubmitValidation(t *testing.T) {
	f := newFixture(t)
	cases := []struct {
		name, code, start, end, field string
	}{
		{"past", "annual", "2026-10-02", "2026-10-02", "start"},
		{"reversed", "annual", "2026-10-09", "2026-10-07", "end"},
		{"weekend only", "annual", "2026-10-10", "2026-10-11", "end"},
		{"holiday only", "annual", "2026-12-16", "2026-12-16", "end"},
		{"over balance", "annual", "2026-10-06", "2026-11-06", "end"},
		{"typo year", "annual", "2026-10-06", "2062-10-06", "end"},
		{"sick too far back", "sick", "2026-09-01", "2026-09-01", "start"},
		{"not a date", "annual", "tomorrow", "2026-10-06", "start"},
	}
	for _, c := range cases {
		if _, err := f.submit("staff", c.code, c.start, c.end); !fieldErr(err, c.field) {
			t.Errorf("%s: err = %v, want a %s error", c.name, err, c.field)
		}
	}
	// Sick leave may be applied for after the fact, within the window.
	if _, err := f.submit("staff", "sick", "2026-09-28", "2026-09-29"); err != nil {
		t.Errorf("backdated sick leave: %v", err)
	}
	// Family responsibility needs a note.
	_, err := f.svc.Submit(f.ctx, f.people["staff"], Input{TypeID: f.types["family"].ID, Start: "2026-10-06", End: "2026-10-06"})
	if !fieldErr(err, "note") {
		t.Errorf("family leave without note: %v", err)
	}
	if len(f.events) != 1 {
		t.Errorf("%d events for one successful submit", len(f.events))
	}
}

func TestPendingReservesBalanceAndBlocksOverlap(t *testing.T) {
	f := newFixture(t)
	r := f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-16") // 5 days
	if r.Days != 5 || r.ApproverID != f.people["head"].ID || r.Status != StatusPending {
		t.Fatalf("submitted = %+v", r)
	}
	// Pending days are reserved, so the same days cannot be spent twice.
	if got := f.available("staff", "annual"); got != 10 {
		t.Fatalf("available after pending 5 = %d, want 10", got)
	}
	if _, err := f.submit("staff", "annual", "2026-10-16", "2026-10-19"); !fieldErr(err, "start") {
		t.Fatalf("overlapping request: %v", err)
	}
	// Withdrawing releases them.
	if err := f.svc.Withdraw(f.ctx, f.people["staff"], r.ID); err != nil {
		t.Fatal(err)
	}
	if got := f.available("staff", "annual"); got != 15 {
		t.Fatalf("available after withdraw = %d, want 15", got)
	}
	if err := f.svc.Withdraw(f.ctx, f.people["staff"], r.ID); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("second withdraw: %v", err)
	}
}

// The transition matrix: who may decide what, in which state.
func TestDecidePermissions(t *testing.T) {
	f := newFixture(t)
	r := f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-13")

	for _, who := range []string{"staff", "hr", "pastor"} {
		if _, err := f.svc.Decide(f.ctx, f.people[who], r.ID, true, ""); !errors.Is(err, ErrNotAllowed) {
			t.Errorf("%s approving staff's leave: %v, want not allowed", who, err)
		}
	}
	if _, err := f.svc.Decide(f.ctx, f.people["head"], r.ID, false, ""); !fieldErr(err, "note") {
		t.Errorf("decline without a reason: %v", err)
	}
	got, err := f.svc.Decide(f.ctx, f.people["head"], r.ID, true, "")
	if err != nil || got.Status != StatusApproved || got.DecidedByID != f.people["head"].ID {
		t.Fatalf("head approves: %+v %v", got, err)
	}
	// A second click (another tab) is refused, not applied twice.
	if _, err := f.svc.Decide(f.ctx, f.people["head"], r.ID, false, "no"); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("deciding twice: %v", err)
	}
	if got := f.available("staff", "annual"); got != 13 {
		t.Errorf("available after approval = %d, want 13", got)
	}

	// The final approver's own leave goes to the admins -- any admin but them.
	p := f.mustSubmit("pastor", "annual", "2026-10-12", "2026-10-12")
	if p.ApproverID != 0 {
		t.Fatalf("pastor's approver = %d, want the admins", p.ApproverID)
	}
	if _, err := f.svc.Decide(f.ctx, f.people["head"], p.ID, true, ""); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("non-admin deciding the admins' queue: %v", err)
	}
	if _, err := f.svc.Decide(f.ctx, f.people["hr"], p.ID, true, ""); err != nil {
		t.Errorf("admin deciding the admins' queue: %v", err)
	}
	// An admin cannot approve their own leave even from that queue.
	f.svc.SetOverride(f.ctx, f.people["hr"].ID, f.people["pastor"].ID)
	f.db.Exec(`UPDATE users SET is_admin = 1 WHERE username = 'pastor'`)
	pastor, _ := f.us.ByUsername(f.ctx, "pastor")
	p2, _ := f.svc.Submit(f.ctx, pastor, Input{TypeID: f.types["annual"].ID, Start: "2026-10-20", End: "2026-10-20"})
	if _, err := f.svc.Decide(f.ctx, pastor, p2.ID, true, ""); !errors.Is(err, ErrNotAllowed) {
		t.Errorf("admin approving own leave: %v", err)
	}
}

func TestCancellationGoesThroughApproval(t *testing.T) {
	f := newFixture(t)
	r := f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-16")
	if _, err := f.svc.RequestCancellation(f.ctx, f.people["staff"], r.ID, ""); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("cancelling pending leave (withdraw it instead): %v", err)
	}
	f.svc.Decide(f.ctx, f.people["head"], r.ID, true, "")
	r, _ = f.svc.Get(f.ctx, r.ID)

	if _, err := f.svc.RequestCancellation(f.ctx, f.people["hr"], r.ID, ""); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("cancelling someone else's leave: %v", err)
	}
	c, err := f.svc.RequestCancellation(f.ctx, f.people["staff"], r.ID, "plans changed")
	if err != nil || c.Kind != KindCancellation || c.CancelsID != r.ID || c.ApproverID != f.people["head"].ID {
		t.Fatalf("cancellation = %+v, %v", c, err)
	}
	if _, err := f.svc.RequestCancellation(f.ctx, f.people["staff"], r.ID, "again"); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("second pending cancellation: %v", err)
	}
	// Until it is approved the leave stays booked.
	if got := f.available("staff", "annual"); got != 10 {
		t.Fatalf("available while cancellation pending = %d, want 10", got)
	}

	// Declined: the leave stands, and a new cancellation may be requested.
	f.svc.Decide(f.ctx, f.people["head"], c.ID, false, "we need you that week")
	if orig, _ := f.svc.Get(f.ctx, r.ID); orig.Status != StatusApproved {
		t.Fatalf("after declined cancellation, leave is %s", orig.Status)
	}
	c2, err := f.svc.RequestCancellation(f.ctx, f.people["staff"], r.ID, "please")
	if err != nil {
		t.Fatal(err)
	}
	f.events = nil
	if _, err := f.svc.Decide(f.ctx, f.people["head"], c2.ID, true, ""); err != nil {
		t.Fatal(err)
	}
	if orig, _ := f.svc.Get(f.ctx, r.ID); orig.Status != StatusCancelled {
		t.Fatalf("after approved cancellation, leave is %s", orig.Status)
	}
	if got := f.available("staff", "annual"); got != 15 {
		t.Fatalf("available after cancellation = %d, want 15", got)
	}
	if len(f.events) != 1 || f.events[0].Cancelled == nil || f.events[0].Cancelled.ID != r.ID {
		t.Fatalf("approval event does not carry the cancelled leave: %+v", f.events)
	}
}

// Leave already under way cannot be cancelled through the app (an open
// decision; an admin adjusts the balance instead).
func TestCancellationOnlyForFutureLeave(t *testing.T) {
	f := newFixture(t)
	r := f.mustSubmit("staff", "sick", "2026-10-01", "2026-10-02")
	f.svc.Decide(f.ctx, f.people["head"], r.ID, true, "")
	if _, err := f.svc.RequestCancellation(f.ctx, f.people["staff"], r.ID, ""); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("cancelling past leave: %v", err)
	}
}

// An event hook failure rolls the change back: no approval nobody hears about.
func TestHookFailureRollsBack(t *testing.T) {
	f := newFixture(t)
	f.svc.OnEvent(func(context.Context, *sql.Tx, Event) error { return errors.New("outbox full") })
	if _, err := f.submit("staff", "annual", "2026-10-12", "2026-10-12"); err == nil {
		t.Fatal("submit succeeded despite the hook failing")
	}
	var n int
	f.db.QueryRow(`SELECT count(*) FROM requests`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d requests saved by a rolled-back submit", n)
	}
}

func TestAdjustmentsAndSickCycle(t *testing.T) {
	f := newFixture(t)
	err := f.svc.AddAdjustment(f.ctx, f.people["hr"].ID, Adjustment{UserID: f.people["staff"].ID,
		TypeID: f.types["annual"].ID, CycleStart: "2026-01-01", Days: 5, Reason: "carry-over from 2025"})
	if err != nil {
		t.Fatal(err)
	}
	if got := f.available("staff", "annual"); got != 20 {
		t.Fatalf("available with carry-over = %d, want 20", got)
	}
	// The sick cycle started 2025-01-01, so an adjustment dated 2026 is refused.
	err = f.svc.AddAdjustment(f.ctx, f.people["hr"].ID, Adjustment{UserID: f.people["staff"].ID,
		TypeID: f.types["sick"].ID, CycleStart: "2026-01-01", Days: 1, Reason: "x"})
	if !fieldErr(err, "cycle") {
		t.Fatalf("adjustment to a non-cycle start: %v", err)
	}
	// Sick leave taken in 2025 still counts in October 2026: same 36-month cycle.
	f.db.Exec(`INSERT INTO requests (user_id, type_id, kind, start_date, end_date, days, cycle_start, status,
		approver_id, created_at) VALUES (?, ?, 'leave', '2025-06-02', '2025-06-06', 5, '2025-01-01', 'approved', ?, '')`,
		f.people["staff"].ID, f.types["sick"].ID, f.people["head"].ID)
	if got := f.available("staff", "sick"); got != 25 {
		t.Fatalf("sick available = %d, want 25", got)
	}
}

func TestSeedTypesLeavesEditsAlone(t *testing.T) {
	f := newFixture(t)
	a := f.types["annual"]
	a.Allowance = intp(18)
	if _, err := f.svc.SaveType(f.ctx, a); err != nil {
		t.Fatal(err)
	}
	if err := f.svc.SeedTypes(f.ctx); err != nil {
		t.Fatal(err)
	}
	got, _ := f.svc.Type(f.ctx, a.ID)
	if *got.Allowance != 18 {
		t.Fatalf("seed overwrote an edit: allowance %d", *got.Allowance)
	}
	if len(f.types) != len(bceaDefaults) {
		t.Fatalf("%d types seeded, want %d", len(f.types), len(bceaDefaults))
	}
}

// A deleted computed holiday stays deleted: generating the year again (on the
// next request that touches it) must not overrule the admin.
func TestHolidayDeletionSticks(t *testing.T) {
	f := newFixture(t)
	f.mustSubmit("staff", "annual", "2026-12-14", "2026-12-18") // generates 2026
	if err := f.svc.DeleteHoliday(f.ctx, "2026-12-16"); err != nil {
		t.Fatal(err)
	}
	q, err := f.svc.Preview(f.ctx, f.people["head"], Input{TypeID: f.types["annual"].ID, Start: "2026-12-14", End: "2026-12-18"})
	if err != nil {
		t.Fatal(err)
	}
	if q.Days != 5 {
		t.Fatalf("days with 16 Dec deleted = %d, want 5", q.Days)
	}
}

func TestQueueAndCounts(t *testing.T) {
	f := newFixture(t)
	for i := range 3 {
		day := strconv.Itoa(12 + i)
		f.mustSubmit("staff", "annual", "2026-10-"+day, "2026-10-"+day)
	}
	f.mustSubmit("pastor", "annual", "2026-10-12", "2026-10-12")
	for who, want := range map[string]int{"head": 3, "hr": 1, "pastor": 0, "staff": 0} {
		n, _ := f.svc.PendingCount(f.ctx, f.people[who])
		q, _ := f.svc.Queue(f.ctx, f.people[who])
		if n != want || len(q) != want {
			t.Errorf("%s: count %d, queue %d, want %d", who, n, len(q), want)
		}
	}
	team, _ := f.svc.Team(f.ctx, f.people["head"])
	if len(team) != 3 {
		t.Errorf("head's team view has %d entries, want 3", len(team))
	}
}
