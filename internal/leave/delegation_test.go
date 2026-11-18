package leave

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/17xande-dev/leave/internal/directory"
)

// The fixture's "today" is Monday 2026-10-05. head approves staff and hr;
// orphan and pastor have no manager above them (pastor is the final approver).

func (f *fixture) setToday(day string) {
	f.t.Helper()
	d, err := ParseDate(day)
	if err != nil {
		f.t.Fatal(err)
	}
	f.svc.Now = func() time.Time { return d.Add(8 * time.Hour) }
}

// delegate sets a delegation up as its delegator would.
func (f *fixture) delegate(delegator, delegate, start, end string) (Delegation, error) {
	return f.svc.Delegate(f.ctx, f.people[delegator], f.people[delegator].ID, f.people[delegate].ID, start, end, "away")
}

func (f *fixture) mustDelegate(delegator, delegate, start, end string) Delegation {
	f.t.Helper()
	d, err := f.delegate(delegator, delegate, start, end)
	if err != nil {
		f.t.Fatalf("delegate %s -> %s %s..%s: %v", delegator, delegate, start, end, err)
	}
	return d
}

func (f *fixture) canDecide(who string, r Request) bool {
	f.t.Helper()
	_, ok, err := f.svc.CanDecideNow(f.ctx, f.db, f.people[who], r)
	if err != nil {
		f.t.Fatal(err)
	}
	return ok
}

func (f *fixture) queueHas(who string, r Request) bool {
	f.t.Helper()
	q, err := f.svc.Queue(f.ctx, f.people[who])
	if err != nil {
		f.t.Fatal(err)
	}
	n, err := f.svc.PendingCount(f.ctx, f.people[who])
	if err != nil {
		f.t.Fatal(err)
	}
	in := false
	for _, x := range q {
		in = in || x.ID == r.ID
	}
	if n != len(q) {
		f.t.Fatalf("%s: PendingCount %d but Queue has %d", who, n, len(q))
	}
	return in
}

func TestDelegateCreate(t *testing.T) {
	const (
		notAllowed = "not allowed"
		ok         = ""
	)
	for _, tc := range []struct {
		name, actor, delegator, delegate, start, end string
		disabledActor, disabledDelegate              bool
		want                                         string // ok, notAllowed, or a field name
		message                                      string
	}{
		{name: "delegator for themselves", actor: "head", delegator: "head", delegate: "orphan", start: "2026-10-06", end: "2026-10-10", want: ok},
		{name: "starting today", actor: "head", delegator: "head", delegate: "orphan", start: "2026-10-05", end: "2026-10-05", want: ok},
		{name: "admin for someone", actor: "hr", delegator: "head", delegate: "orphan", start: "2026-10-06", end: "2026-10-10", want: ok},
		{name: "non-admin for someone else", actor: "staff", delegator: "head", delegate: "orphan", start: "2026-10-06", end: "2026-10-10", want: notAllowed},
		{name: "disabled admin", actor: "hr", delegator: "head", delegate: "orphan", start: "2026-10-06", end: "2026-10-10", disabledActor: true, want: notAllowed},
		{name: "disabled delegator", actor: "head", delegator: "head", delegate: "orphan", start: "2026-10-06", end: "2026-10-10", disabledActor: true, want: notAllowed},
		{name: "disabled delegate", actor: "head", delegator: "head", delegate: "orphan", start: "2026-10-06", end: "2026-10-10", disabledDelegate: true, want: "delegate", message: MsgDelegateUnusable},
		{name: "unknown delegate", actor: "head", delegator: "head", delegate: "nobody", start: "2026-10-06", end: "2026-10-10", want: "delegate", message: MsgDelegateUnusable},
		{name: "the delegator", actor: "head", delegator: "head", delegate: "head", start: "2026-10-06", end: "2026-10-10", want: "delegate", message: MsgDelegateUnusable},
		{name: "start yesterday", actor: "head", delegator: "head", delegate: "orphan", start: "2026-10-04", end: "2026-10-10", want: "start", message: "Start today or later."},
		{name: "start not a date", actor: "head", delegator: "head", delegate: "orphan", start: "soon", end: "2026-10-10", want: "start"},
		{name: "end not a date", actor: "head", delegator: "head", delegate: "orphan", start: "2026-10-06", end: "", want: "end"},
		{name: "end before start", actor: "head", delegator: "head", delegate: "orphan", start: "2026-10-08", end: "2026-10-07", want: "end"},
		// First and last day count: 5 Oct + 29 days is 3 Nov, the 30th day.
		{name: "30 days", actor: "head", delegator: "head", delegate: "orphan", start: "2026-10-05", end: "2026-11-03", want: ok},
		{name: "31 days", actor: "head", delegator: "head", delegate: "orphan", start: "2026-10-05", end: "2026-11-04", want: "end", message: "A stand-in can cover at most 30 days."},
		{name: "30 days later in the year", actor: "head", delegator: "head", delegate: "orphan", start: "2026-12-20", end: "2027-01-18", want: ok},
		{name: "31 days later in the year", actor: "head", delegator: "head", delegate: "orphan", start: "2026-12-20", end: "2027-01-19", want: "end"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			delegate := f.people[tc.delegate].ID
			if tc.delegate == "nobody" {
				delegate = 99999
			}
			if tc.disabledDelegate {
				f.exec(`UPDATE users SET disabled = 1 WHERE id = ?`, delegate)
			}
			actor := f.people[tc.actor]
			actor.Disabled = tc.disabledActor
			f.events = nil
			d, err := f.svc.Delegate(f.ctx, actor, f.people[tc.delegator].ID, delegate, tc.start, tc.end, " going to the coast ")
			var jobs int
			if err := f.db.QueryRow(`SELECT count(*) FROM approval_delegations`).Scan(&jobs); err != nil {
				t.Fatal(err)
			}
			switch tc.want {
			case ok:
				if err != nil {
					t.Fatal(err)
				}
				if d.ID == 0 || d.DelegatorID != f.people[tc.delegator].ID || d.DelegateID != delegate || d.CreatedByID != actor.ID ||
					d.Start != tc.start || d.End != tc.end || d.Note != "going to the coast" || d.DelegatorName != tc.delegator ||
					d.DelegateName != tc.delegate || d.CreatedBy != tc.actor || d.RevokedAt != "" || d.CreatedAt == "" {
					t.Fatalf("delegation = %+v", d)
				}
				if jobs != 1 || len(f.events) != 1 || f.events[0].Kind != EventDelegated || f.events[0].Delegation == nil ||
					f.events[0].Delegation.ID != d.ID || f.events[0].Request.ID != 0 || f.events[0].Actor.ID != actor.ID {
					t.Fatalf("rows=%d events=%+v", jobs, f.events)
				}
			case notAllowed:
				if !errors.Is(err, ErrNotAllowed) {
					t.Fatalf("error = %v", err)
				}
			default:
				var fe FieldErrors
				if !errors.As(err, &fe) || fe[tc.want] == "" || (tc.message != "" && fe[tc.want] != tc.message) {
					t.Fatalf("error = %v, want %s %q", err, tc.want, tc.message)
				}
			}
			if tc.want != ok && (jobs != 0 || len(f.events) != 0) {
				t.Fatalf("a refusal left rows=%d events=%v", jobs, f.events)
			}
		})
	}
}

func TestDelegateOverlap(t *testing.T) {
	f := newFixture(t)
	first := f.mustDelegate("head", "orphan", "2026-10-06", "2026-10-10")

	for _, tc := range []struct{ name, delegate, start, end string }{
		{"same days", "orphan", "2026-10-06", "2026-10-10"},
		{"inside", "orphan", "2026-10-07", "2026-10-08"},
		{"overlapping the end", "orphan", "2026-10-10", "2026-10-12"},
		{"overlapping the start", "orphan", "2026-10-05", "2026-10-06"},
		{"around it", "orphan", "2026-10-05", "2026-10-20"},
		// One stand-in at a time, whoever it is.
		{"another delegate", "staff", "2026-10-08", "2026-10-09"},
	} {
		_, err := f.delegate("head", tc.delegate, tc.start, tc.end)
		var fe FieldErrors
		want := "head already has a stand-in from Tue 6 Oct 2026 to Sat 10 Oct 2026. Revoke it first."
		if !errors.As(err, &fe) || fe["delegate"] != want {
			t.Errorf("%s: error = %v", tc.name, err)
		}
	}
	// The day before and the day after are free.
	f.mustDelegate("head", "orphan", "2026-10-11", "2026-10-12")
	f.mustDelegate("head", "staff", "2026-10-05", "2026-10-05")
	// Someone else's delegation to the same person does not count.
	f.mustDelegate("pastor", "orphan", "2026-10-06", "2026-10-10")

	// A revoked one frees its days.
	if _, err := f.svc.RevokeDelegation(f.ctx, f.people["head"], first.ID); err != nil {
		t.Fatal(err)
	}
	f.mustDelegate("head", "orphan", "2026-10-07", "2026-10-09")
}

func TestRevokeDelegation(t *testing.T) {
	f := newFixture(t)
	d := f.mustDelegate("head", "orphan", "2026-10-06", "2026-10-10")

	f.events = nil
	if _, err := f.svc.RevokeDelegation(f.ctx, f.people["staff"], d.ID); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("stranger: %v", err)
	}
	if _, err := f.svc.RevokeDelegation(f.ctx, f.people["orphan"], d.ID); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("the delegate: %v", err)
	}
	disabled := f.people["hr"]
	disabled.Disabled = true
	if _, err := f.svc.RevokeDelegation(f.ctx, disabled, d.ID); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("disabled admin: %v", err)
	}
	if _, err := f.svc.RevokeDelegation(f.ctx, f.people["head"], 99999); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("unknown id: %v", err)
	}
	if len(f.events) != 0 {
		t.Fatalf("refusals emitted %v", f.events)
	}
	got, err := f.svc.RevokeDelegation(f.ctx, f.people["hr"], d.ID) // an admin may
	if err != nil {
		t.Fatal(err)
	}
	if got.RevokedAt == "" || got.RevokedByID != f.people["hr"].ID || got.RevokedBy != "hr" {
		t.Fatalf("revoked = %+v", got)
	}
	if len(f.events) != 1 || f.events[0].Kind != EventDelegationRevoked || f.events[0].Delegation.ID != d.ID || f.events[0].Request.ID != 0 {
		t.Fatalf("events = %+v", f.events)
	}
	if _, err := f.svc.RevokeDelegation(f.ctx, f.people["head"], d.ID); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("twice: %v", err)
	}

	// One that is already over cannot be revoked; its last day is still current.
	over := f.mustDelegate("pastor", "orphan", "2026-10-05", "2026-10-07")
	f.setToday("2026-10-07")
	if _, err := f.svc.RevokeDelegation(f.ctx, f.people["pastor"], over.ID); err != nil {
		t.Fatalf("on its last day: %v", err)
	}
	again := f.mustDelegate("pastor", "staff", "2026-10-08", "2026-10-08")
	f.setToday("2026-10-09")
	if _, err := f.svc.RevokeDelegation(f.ctx, f.people["pastor"], again.ID); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("after it ended: %v", err)
	}
}

func TestDelegationsListing(t *testing.T) {
	f := newFixture(t)
	a := f.mustDelegate("head", "orphan", "2026-10-06", "2026-10-10")
	b := f.mustDelegate("pastor", "orphan", "2026-10-20", "2026-10-22")
	c := f.mustDelegate("hr", "staff", "2026-10-05", "2026-10-05")
	if _, err := f.svc.RevokeDelegation(f.ctx, f.people["hr"], c.ID); err != nil {
		t.Fatal(err)
	}
	ids := func(ds []Delegation, err error) []int64 {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		var out []int64
		for _, d := range ds {
			out = append(out, d.ID)
		}
		return out
	}
	eq := func(got []int64, want ...int64) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("got %v, want %v", got, want)
			}
		}
	}
	eq(ids(f.svc.Delegations(f.ctx, DelegationFilter{})), a.ID, b.ID)
	eq(ids(f.svc.Delegations(f.ctx, DelegationFilter{DelegatorID: f.people["head"].ID})), a.ID)
	eq(ids(f.svc.Delegations(f.ctx, DelegationFilter{DelegateID: f.people["orphan"].ID})), a.ID, b.ID)
	eq(ids(f.svc.Delegations(f.ctx, DelegationFilter{DelegateID: f.people["staff"].ID})))
	eq(ids(f.svc.Delegations(f.ctx, DelegationFilter{Past: true})), c.ID)
	f.setToday("2026-10-12")
	eq(ids(f.svc.Delegations(f.ctx, DelegationFilter{})), b.ID)
	eq(ids(f.svc.Delegations(f.ctx, DelegationFilter{Past: true})), a.ID, c.ID)
	eq(ids(f.svc.Delegations(f.ctx, DelegationFilter{Past: true, DelegatorID: f.people["head"].ID})), a.ID)

	if !a.Active("2026-10-06") || !a.Active("2026-10-10") || a.Active("2026-10-05") || a.Active("2026-10-11") {
		t.Fatal("Active ignores the range")
	}
	c, _ = f.svc.getDelegation(f.ctx, f.db, c.ID)
	if c.Active("2026-10-05") {
		t.Fatal("a revoked delegation is active")
	}
}

// The delegate may decide what waits for the delegator only on the days it
// covers, and the request itself never moves.
func TestDelegateDecidesOnlyWhileActive(t *testing.T) {
	f := newFixture(t)
	// Waiting before the delegation exists.
	r := f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-12")
	d := f.mustDelegate("head", "orphan", "2026-10-07", "2026-10-09")

	// Before the start date: nothing.
	if f.canDecide("orphan", r) || f.queueHas("orphan", r) {
		t.Fatal("the stand-in acts before the start date")
	}
	if _, err := f.svc.Decide(f.ctx, f.people["orphan"], r.ID, true, ""); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("before start: %v", err)
	}
	if ok, _ := f.svc.CanViewNow(f.ctx, f.people["orphan"], r); ok {
		t.Fatal("the stand-in can view before the start date")
	}

	// On the start date both can; the request already waiting shows up.
	f.setToday("2026-10-07")
	if !f.canDecide("orphan", r) || !f.canDecide("head", r) || !f.queueHas("orphan", r) || !f.queueHas("head", r) {
		t.Fatal("on the start date both should be able to decide")
	}
	if ok, err := f.svc.CanViewNow(f.ctx, f.people["orphan"], r); err != nil || !ok {
		t.Fatalf("stand-in cannot view: %v %v", ok, err)
	}
	if got, _ := f.svc.Get(f.ctx, r.ID); got.ApproverID != f.people["head"].ID {
		t.Fatal("the request moved to the stand-in")
	}
	// Nobody else gains anything.
	for _, who := range []string{"staff", "pastor", "hr"} {
		if f.canDecide(who, r) || f.queueHas(who, r) {
			t.Errorf("%s can decide", who)
		}
	}
	// The last day still counts, the day after does not.
	f.setToday("2026-10-09")
	if !f.canDecide("orphan", r) {
		t.Fatal("last day")
	}
	f.setToday("2026-10-10")
	if f.canDecide("orphan", r) || f.queueHas("orphan", r) || !f.canDecide("head", r) {
		t.Fatal("the stand-in acts after the end date, or the approver lost the request")
	}
	if _, err := f.svc.Decide(f.ctx, f.people["orphan"], r.ID, true, ""); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("after end: %v", err)
	}
	if ok, _ := f.svc.CanViewNow(f.ctx, f.people["orphan"], r); ok {
		t.Fatal("the stand-in can still view after the end date")
	}

	// Revoked: refused again.
	f.setToday("2026-10-08")
	if !f.canDecide("orphan", r) {
		t.Fatal("active again")
	}
	if _, err := f.svc.RevokeDelegation(f.ctx, f.people["head"], d.ID); err != nil {
		t.Fatal(err)
	}
	if f.canDecide("orphan", r) || f.queueHas("orphan", r) {
		t.Fatal("the stand-in acts after revocation")
	}
	if _, err := f.svc.Decide(f.ctx, f.people["orphan"], r.ID, true, ""); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("after revoke: %v", err)
	}
	// The approver still can, and a disabled stand-in never could.
	if !f.canDecide("head", r) {
		t.Fatal("head lost the request")
	}
}

// The page may be read while the delegation is active; the decision is judged
// inside the transaction, on what is true then.
func TestDelegateRevokedBetweenReadAndWrite(t *testing.T) {
	f := newFixture(t)
	r := f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-12")
	d := f.mustDelegate("head", "orphan", "2026-10-05", "2026-10-09")
	if !f.canDecide("orphan", r) {
		t.Fatal("not active")
	}
	f.svc.beforeDecideTx = func() {
		if _, err := f.svc.RevokeDelegation(f.ctx, f.people["head"], d.ID); err != nil {
			t.Error(err)
		}
	}
	if _, err := f.svc.Decide(f.ctx, f.people["orphan"], r.ID, true, ""); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("decided on a revoked delegation: %v", err)
	}
	if got, _ := f.svc.Get(f.ctx, r.ID); got.Status != StatusPending {
		t.Fatalf("status = %s", got.Status)
	}
}

func TestDelegateDisabledCannotDecide(t *testing.T) {
	f := newFixture(t)
	r := f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-12")
	f.mustDelegate("head", "orphan", "2026-10-05", "2026-10-09")
	u := f.people["orphan"]
	u.Disabled = true
	if g, err := f.svc.ActingFor(f.ctx, f.db, u); err != nil || len(g) != 0 {
		t.Fatalf("grants for a disabled user: %v %v", g, err)
	}
	if _, ok, _ := f.svc.CanDecideNow(f.ctx, f.db, u, r); ok {
		t.Fatal("disabled stand-in can decide")
	}
	if q, _ := f.svc.Queue(f.ctx, u); len(q) != 0 {
		t.Fatalf("disabled stand-in's queue: %v", q)
	}
	if n, _ := f.svc.PendingCount(f.ctx, u); n != 0 {
		t.Fatalf("disabled stand-in's count: %d", n)
	}
}

func TestActingForAndActiveStandIn(t *testing.T) {
	f := newFixture(t)
	f.mustDelegate("head", "orphan", "2026-10-06", "2026-10-10")
	f.mustDelegate("pastor", "orphan", "2026-10-05", "2026-10-07")
	grants := func(who string) []Grant {
		t.Helper()
		g, err := f.svc.ActingFor(f.ctx, f.db, f.people[who])
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	if g := grants("orphan"); len(g) != 1 || g[0].DelegatorName != "pastor" || g[0].Until != "2026-10-07" || g[0].CreatedByID != f.people["pastor"].ID {
		t.Fatalf("grants today = %+v", g)
	}
	f.setToday("2026-10-06")
	if g := grants("orphan"); len(g) != 2 {
		t.Fatalf("grants = %+v", g)
	}
	if g := grants("head"); len(g) != 0 {
		t.Fatalf("the delegator acts for nobody: %+v", g)
	}
	sd, err := f.svc.ActiveStandIn(f.ctx, f.db, f.people["head"].ID)
	if err != nil || sd == nil || sd.DelegateID != f.people["orphan"].ID || sd.DelegateName != "orphan" {
		t.Fatalf("stand-in = %+v %v", sd, err)
	}
	if sd, err := f.svc.ActiveStandIn(f.ctx, f.db, f.people["staff"].ID); err != nil || sd != nil {
		t.Fatalf("stand-in for staff = %+v %v", sd, err)
	}
	f.setToday("2026-10-11")
	if sd, _ := f.svc.ActiveStandIn(f.ctx, f.db, f.people["head"].ID); sd != nil {
		t.Fatal("a stand-in after the end")
	}
}

func TestDelegationBoundaries(t *testing.T) {
	// A stand-in cannot decide the delegator's own... their own leave waiting
	// with the delegator stays with the delegator.
	t.Run("own request", func(t *testing.T) {
		f := newFixture(t)
		mine := f.mustSubmit("hr", "annual", "2026-10-12", "2026-10-12") // with head
		other := f.mustSubmit("staff", "annual", "2026-10-13", "2026-10-13")
		f.mustDelegate("head", "hr", "2026-10-05", "2026-10-09")
		if f.canDecide("hr", mine) || f.queueHas("hr", mine) {
			t.Fatal("the stand-in decides their own request")
		}
		if _, err := f.svc.Decide(f.ctx, f.people["hr"], mine.ID, true, ""); !errors.Is(err, ErrNotAllowed) {
			t.Fatalf("own request: %v", err)
		}
		if !f.canDecide("hr", other) || !f.queueHas("hr", other) {
			t.Fatal("the stand-in cannot decide the others")
		}
	})

	// An admin who sets up a delegation for the approver of their own leave
	// does not thereby let a friend approve it.
	t.Run("creator's own leave", func(t *testing.T) {
		f := newFixture(t)
		mine := f.mustSubmit("hr", "annual", "2026-10-12", "2026-10-12")
		other := f.mustSubmit("staff", "annual", "2026-10-13", "2026-10-13")
		if _, err := f.svc.Delegate(f.ctx, f.people["hr"], f.people["head"].ID, f.people["orphan"].ID, "2026-10-05", "2026-10-09", ""); err != nil {
			t.Fatal(err)
		}
		if f.canDecide("orphan", mine) || f.queueHas("orphan", mine) {
			t.Fatal("a delegation lets its delegate decide its creator's own leave")
		}
		if _, err := f.svc.Decide(f.ctx, f.people["orphan"], mine.ID, true, ""); !errors.Is(err, ErrNotAllowed) {
			t.Fatalf("creator's request: %v", err)
		}
		if !f.canDecide("orphan", other) || !f.queueHas("orphan", other) {
			t.Fatal("the delegation does not cover the others")
		}
		// head still decides it, as ever.
		if !f.canDecide("head", mine) {
			t.Fatal("head lost it")
		}
	})

	// A -> B and B -> C: C acts for B, never for A.
	t.Run("no chains", func(t *testing.T) {
		f := newFixture(t)
		ofHead := f.mustSubmit("hr", "annual", "2026-10-12", "2026-10-12") // with head (A)
		ofOrphan := f.mustSubmit("staff", "annual", "2026-10-13", "2026-10-13")
		f.exec(`UPDATE requests SET approver_id = ? WHERE id = ?`, f.people["orphan"].ID, ofOrphan.ID) // with orphan (B)
		ofOrphan, _ = f.svc.Get(f.ctx, ofOrphan.ID)
		f.mustDelegate("head", "orphan", "2026-10-05", "2026-10-09")
		f.mustDelegate("orphan", "pastor", "2026-10-05", "2026-10-09")
		if !f.canDecide("orphan", ofHead) {
			t.Fatal("B cannot act for A")
		}
		if !f.canDecide("pastor", ofOrphan) || !f.queueHas("pastor", ofOrphan) {
			t.Fatal("C cannot act for B")
		}
		if f.canDecide("pastor", ofHead) || f.queueHas("pastor", ofHead) {
			t.Fatal("C acts for A through B: a chain")
		}
		if _, err := f.svc.Decide(f.ctx, f.people["pastor"], ofHead.ID, true, ""); !errors.Is(err, ErrNotAllowed) {
			t.Fatalf("chain: %v", err)
		}
	})

	// The admins' queue (no approver) is never delegated, and a stand-in gets
	// no administrator powers.
	t.Run("admin queue and powers", func(t *testing.T) {
		f := newFixture(t)
		inQueue := f.mustSubmit("pastor", "annual", "2026-10-12", "2026-10-12")
		if inQueue.ApproverID != 0 {
			t.Fatalf("approver = %d", inQueue.ApproverID)
		}
		ordinary := f.mustSubmit("staff", "annual", "2026-10-13", "2026-10-13")
		f.mustDelegate("hr", "orphan", "2026-10-05", "2026-10-09")
		f.mustDelegate("head", "orphan", "2026-10-05", "2026-10-09")
		if f.canDecide("orphan", inQueue) || f.queueHas("orphan", inQueue) {
			t.Fatal("the admins' queue was delegated")
		}
		o := f.people["orphan"]
		if _, err := f.svc.GrantNegative(f.ctx, o, ordinary.ID, ""); !errors.Is(err, ErrNotAllowed) {
			t.Fatalf("GrantNegative: %v", err)
		}
		if _, err := f.svc.RefuseNegative(f.ctx, o, ordinary.ID, "no"); !errors.Is(err, ErrNotAllowed) {
			t.Fatalf("RefuseNegative: %v", err)
		}
		if _, err := f.svc.Reassign(f.ctx, o, ordinary.ID, f.people["hr"].ID); !errors.Is(err, ErrNotAllowed) {
			t.Fatalf("Reassign: %v", err)
		}
		if _, err := f.svc.Decide(f.ctx, o, inQueue.ID, true, ""); !errors.Is(err, ErrNotAllowed) {
			t.Fatalf("admin queue: %v", err)
		}
	})
}

func TestDelegateAuditTrail(t *testing.T) {
	f := newFixture(t)
	direct := f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-12")
	viaStandIn := f.mustSubmit("hr", "annual", "2026-10-13", "2026-10-13")
	f.mustDelegate("head", "orphan", "2026-10-05", "2026-10-09")

	got, err := f.svc.Decide(f.ctx, f.people["head"], direct.ID, true, "")
	if err != nil || got.DecidedByID != f.people["head"].ID || got.DecidedForID != 0 || got.DecidedFor != "" || got.DecidedByFor() != "head" {
		t.Fatalf("direct approval = %+v %v", got, err)
	}
	got, err = f.svc.Decide(f.ctx, f.people["orphan"], viaStandIn.ID, true, "")
	if err != nil || got.Status != StatusApproved || got.DecidedByID != f.people["orphan"].ID || got.DecidedBy != "orphan" ||
		got.DecidedForID != f.people["head"].ID || got.DecidedFor != "head" || got.DecidedByFor() != "orphan for head" {
		t.Fatalf("stand-in approval = %+v %v", got, err)
	}
	if got.ApproverID != f.people["head"].ID {
		t.Fatalf("approver changed to %d", got.ApproverID)
	}
	var forID any
	f.mustScan(&forID, `SELECT decided_for_id FROM requests WHERE id = ?`, direct.ID)
	if forID != nil {
		t.Fatalf("direct approval stored decided_for_id %v", forID)
	}

	// A decline is also "for".
	declined := f.mustSubmit("staff", "annual", "2026-10-14", "2026-10-14")
	got, err = f.svc.Decide(f.ctx, f.people["orphan"], declined.ID, false, "too busy")
	if err != nil || got.Status != StatusDeclined || got.DecidedFor != "head" {
		t.Fatalf("stand-in decline = %+v %v", got, err)
	}
	// The event the notifications read carries who it was for.
	last := f.events[len(f.events)-1]
	if last.Kind != EventDecided || last.Request.DecidedByFor() != "orphan for head" {
		t.Fatalf("event = %+v", last)
	}
}

func (f *fixture) mustScan(dest any, q string, args ...any) {
	f.t.Helper()
	if err := f.db.QueryRow(q, args...).Scan(dest); err != nil {
		f.t.Fatal(err)
	}
}

// An escalated request is approved by level; a stand-in's approval is the
// delegator's, so it goes where the delegator's would, and a stand-in for the
// final approver finishes it.
func TestDelegateEscalatedRequest(t *testing.T) {
	setup := func(t *testing.T) (*fixture, Request) {
		f := newFixture(t)
		f.dept("Pair", "staff", "hr")
		f.rule("Pair", 50)
		f.mustSubmit("hr", "annual", "2026-10-12", "2026-10-14")
		r, err := f.override("staff", "annual", "2026-10-12", "2026-10-14")
		if err != nil || !r.Escalate || r.ApproverID != f.people["head"].ID {
			t.Fatalf("escalated request = %+v %v", r, err)
		}
		return f, r
	}

	t.Run("mid-level then final", func(t *testing.T) {
		f, r := setup(t)
		// hr is no use as a test stand-in if their own approver is the same as
		// the request's: hr reports to head, so computing the next level from
		// hr would leave the request with head. It must go to head's approver.
		f.mustDelegate("head", "hr", "2026-10-05", "2026-10-09")
		f.mustDelegate("pastor", "hr", "2026-10-05", "2026-10-09")
		// The final approver has someone above them here (orphan, made an
		// admin so they can be an override), so only the acting approver's
		// final-approver status ends the chain: judged on hr instead it
		// would carry on to orphan.
		f.exec(`UPDATE users SET is_admin = 1 WHERE id = ?`, f.people["orphan"].ID)
		if err := f.svc.SetOverride(f.ctx, f.people["pastor"].ID, f.people["orphan"].ID); err != nil {
			t.Fatal(err)
		}

		f.events = nil
		r, err := f.svc.Decide(f.ctx, f.people["hr"], r.ID, true, "ok for head")
		if err != nil {
			t.Fatal(err)
		}
		if r.Status != StatusPending || r.ApproverID != f.people["pastor"].ID || r.DecidedByID != 0 || r.DecidedForID != 0 {
			t.Fatalf("after the stand-in's first approval = %+v", r)
		}
		if len(f.events) != 1 || f.events[0].Kind != EventEscalated || f.events[0].Request.ApproverID != f.people["pastor"].ID {
			t.Fatalf("events = %+v", f.events)
		}
		steps, _ := f.svc.Steps(f.ctx, r.ID)
		if len(steps) != 1 || steps[0].ApproverID != f.people["hr"].ID || steps[0].OnBehalfOfID != f.people["head"].ID || steps[0].OnBehalfOf != "head" {
			t.Fatalf("steps = %+v", steps)
		}
		// head, though away, may not approve twice.
		if _, err := f.svc.Decide(f.ctx, f.people["head"], r.ID, true, ""); !errors.Is(err, ErrNotAllowed) {
			t.Fatalf("head after it moved: %v", err)
		}

		// hr, now standing in for the final approver, makes it final.
		next, ok, err := f.svc.NextApprover(f.ctx, f.people["hr"], r)
		if err != nil || ok {
			t.Fatalf("a stand-in for the final approver should be last: %v %v %v", next, ok, err)
		}
		r, err = f.svc.Decide(f.ctx, f.people["hr"], r.ID, true, "ok for pastor")
		if err != nil {
			t.Fatal(err)
		}
		if r.Status != StatusApproved || r.DecidedByID != f.people["hr"].ID || r.DecidedForID != f.people["pastor"].ID || r.DecidedByFor() != "hr for pastor" {
			t.Fatalf("final approval = %+v", r)
		}
		steps, _ = f.svc.Steps(f.ctx, r.ID)
		if len(steps) != 2 || steps[1].OnBehalfOf != "pastor" || steps[1].ApproverName != "hr" {
			t.Fatalf("steps = %+v", steps)
		}
	})

	// NextApprover answers for the acting approver too, for the page's
	// "Override and send to ...".
	t.Run("next approver for a stand-in", func(t *testing.T) {
		f, r := setup(t)
		f.mustDelegate("head", "hr", "2026-10-05", "2026-10-09")
		next, ok, err := f.svc.NextApprover(f.ctx, f.people["hr"], r)
		if err != nil || !ok || next.ID != f.people["pastor"].ID {
			t.Fatalf("next = %v %v %v", next, ok, err)
		}
	})

	// The stand-in for the first level is also the next level's person: they
	// have just approved, so it does not come back to them.
	t.Run("stand-in is next", func(t *testing.T) {
		f, r := setup(t)
		f.mustDelegate("head", "pastor", "2026-10-05", "2026-10-09")
		r, err := f.svc.Decide(f.ctx, f.people["pastor"], r.ID, true, "")
		if err != nil || r.Status != StatusApproved || r.DecidedFor != "head" {
			t.Fatalf("approval = %+v %v", r, err)
		}
	})

	// staff -> head -> boss -> pastor, with boss standing in for head: boss's
	// one approval counts for head's level and for their own, so the request
	// goes on to pastor rather than being approved with pastor never seeing it.
	t.Run("stand-in is next but not final", func(t *testing.T) {
		f, r := setup(t)
		boss, err := f.us.Upsert(f.ctx, directory.User{ID: "CN=boss,OU=Staff", DN: "CN=boss,OU=Staff", Username: "boss", Name: "boss",
			Email: "boss@example.test", ManagerDN: "CN=pastor,OU=Staff"})
		if err != nil {
			t.Fatal(err)
		}
		f.people["boss"] = boss
		if _, err := f.us.Upsert(f.ctx, directory.User{ID: "CN=head,OU=Staff", DN: "CN=head,OU=Staff", Username: "head", Name: "head",
			Email: "head@example.test", ManagerDN: "CN=boss,OU=Staff"}); err != nil {
			t.Fatal(err)
		}
		f.mustDelegate("head", "boss", "2026-10-05", "2026-10-09")

		next, ok, err := f.svc.NextApprover(f.ctx, boss, r)
		if err != nil || !ok || next.ID != f.people["pastor"].ID {
			t.Fatalf("next for the stand-in = %v %v %v, want pastor", next.Username, ok, err)
		}
		r, err = f.svc.Decide(f.ctx, boss, r.ID, true, "for head")
		if err != nil {
			t.Fatal(err)
		}
		if r.Status != StatusPending || r.ApproverID != f.people["pastor"].ID {
			t.Fatalf("after boss's approval = %+v", r)
		}
		steps, _ := f.svc.Steps(f.ctx, r.ID)
		if len(steps) != 1 || steps[0].ApproverID != boss.ID || steps[0].OnBehalfOf != "head" {
			t.Fatalf("steps = %+v", steps)
		}
		r, err = f.svc.Decide(f.ctx, f.people["pastor"], r.ID, true, "")
		if err != nil || r.Status != StatusApproved || r.DecidedByID != f.people["pastor"].ID || r.DecidedForID != 0 {
			t.Fatalf("pastor's approval = %+v %v", r, err)
		}
		steps, _ = f.svc.Steps(f.ctx, r.ID)
		if len(steps) != 2 || steps[1].ApproverID != f.people["pastor"].ID || steps[1].OnBehalfOfID != 0 {
			t.Fatalf("steps = %+v", steps)
		}
	})

	t.Run("declining ends it", func(t *testing.T) {
		f, r := setup(t)
		f.mustDelegate("head", "hr", "2026-10-05", "2026-10-09")
		r, err := f.svc.Decide(f.ctx, f.people["hr"], r.ID, false, "no")
		if err != nil || r.Status != StatusDeclined || r.DecidedFor != "head" {
			t.Fatalf("decline = %+v %v", r, err)
		}
		steps, _ := f.svc.Steps(f.ctx, r.ID)
		if len(steps) != 1 || steps[0].Decision != "declined" || steps[0].OnBehalfOf != "head" {
			t.Fatalf("steps = %+v", steps)
		}
	})
}

// The stand-in rides on an event only when the requester did not create the
// delegation: its creator cannot use it on their own leave, and the event is
// what the messages are built from.
func TestEventCarriesStandInUnlessRequesterCreatedIt(t *testing.T) {
	for _, tc := range []struct {
		name, creator, delegate string
		want                    bool
	}{
		{"made by the approver", "head", "pastor", true},
		{"made by the requester", "hr", "pastor", false},
		// The stand-in would be approving their own leave: not offered it.
		{"delegate is the requester", "head", "hr", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			if _, err := f.svc.Delegate(f.ctx, f.people[tc.creator], f.people["head"].ID, f.people[tc.delegate].ID, "2026-10-05", "2026-10-09", ""); err != nil {
				t.Fatal(err)
			}
			f.events = nil
			f.mustSubmit("hr", "annual", "2026-10-12", "2026-10-14")
			if len(f.events) != 1 || (f.events[0].StandIn != nil) != tc.want {
				t.Fatalf("events = %+v, want a stand-in: %v", f.events, tc.want)
			}
		})
	}
}

// The guard against a request coming back to someone it has been through
// counts a step done on their behalf as theirs.
func TestEscalationLoopGuardCountsOnBehalfOf(t *testing.T) {
	for _, onBehalf := range []bool{false, true} {
		name := "no earlier step"
		if onBehalf {
			name = "earlier step on behalf of head"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			f.dept("Pair", "staff", "hr")
			f.rule("Pair", 50)
			if err := f.svc.SetOverride(f.ctx, f.people["staff"].ID, f.people["hr"].ID); err != nil {
				t.Fatal(err)
			}
			f.mustSubmit("hr", "annual", "2026-10-12", "2026-10-14")
			r, err := f.override("staff", "annual", "2026-10-12", "2026-10-14")
			if err != nil || !r.Escalate || r.ApproverID != f.people["hr"].ID {
				t.Fatalf("request = %+v %v", r, err)
			}
			if onBehalf {
				// orphan approved earlier, standing in for head.
				f.exec(`INSERT INTO request_steps (request_id, approver_id, decision, note, decided_at, on_behalf_of_id)
					VALUES (?, ?, 'approved', '', '2026-10-05T08:00:00Z', ?)`, r.ID, f.people["orphan"].ID, f.people["head"].ID)
			}
			// hr's approver is head: with no earlier step it moves there.
			got, err := f.svc.Decide(f.ctx, f.people["hr"], r.ID, true, "")
			if err != nil {
				t.Fatal(err)
			}
			// Head's level is already covered (orphan stood in), so it must not
			// go back to head -- but it climbs on to pastor, the final
			// approver, who has not seen it, rather than finishing at hr.
			if onBehalf {
				if got.Status != StatusPending || got.ApproverID != f.people["pastor"].ID {
					t.Fatalf("it went back to head, or skipped pastor: %+v", got)
				}
				got, err = f.svc.Decide(f.ctx, f.people["pastor"], r.ID, true, "")
				if err != nil || got.Status != StatusApproved {
					t.Fatalf("pastor's approval = %+v %v", got, err)
				}
			}
			if !onBehalf && (got.Status != StatusPending || got.ApproverID != f.people["head"].ID) {
				t.Fatalf("it did not move to head: %+v", got)
			}
		})
	}
}

func TestBulkDecideForStandIn(t *testing.T) {
	f := newFixture(t)
	r := f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-12")
	f.mustDelegate("head", "orphan", "2026-10-05", "2026-10-09")
	got, err := f.svc.DecideOrdinary(f.ctx, f.people["orphan"], r.ID, r.Fingerprint())
	if err != nil || got.Status != StatusApproved || got.DecidedFor != "head" {
		t.Fatalf("bulk approval = %+v %v", got, err)
	}
	if !strings.Contains(got.DecidedByFor(), "for") {
		t.Fatal("not marked")
	}
}
