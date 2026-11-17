package leave

import (
	"errors"
	"reflect"
	"testing"

	"github.com/17xande-dev/leave/internal/users"
)

func (f *fixture) correct(admin string, id int64, code, start, end, note, reason string) (Request, error) {
	return f.svc.Correct(f.ctx, f.people[admin], id, Input{TypeID: f.types[code].ID, Start: start, End: end, Note: note}, reason, "")
}

func (f *fixture) mustCorrect(admin string, id int64, code, start, end, note, reason string) Request {
	f.t.Helper()
	r, err := f.correct(admin, id, code, start, end, note, reason)
	if err != nil {
		f.t.Fatalf("correct %d to %s %s..%s: %v", id, code, start, end, err)
	}
	return r
}

func (f *fixture) corrections(id int64) []Correction {
	f.t.Helper()
	cs, err := f.svc.Corrections(f.ctx, id)
	if err != nil {
		f.t.Fatal(err)
	}
	return cs
}

func (f *fixture) setAdmin(who string) {
	f.t.Helper()
	f.exec(`UPDATE users SET is_admin = 1 WHERE username = ?`, who)
	u, err := f.us.ByID(f.ctx, f.people[who].ID)
	if err != nil {
		f.t.Fatal(err)
	}
	f.people[who] = u
}

func TestCorrectPendingAnnualToCompassionate(t *testing.T) {
	f := newFixture(t)
	r := f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-14")
	seen := r.Fingerprint()
	f.events = nil
	out, err := f.svc.Correct(f.ctx, f.people["hr"], r.ID,
		Input{TypeID: f.types["compassionate"].ID, Start: "2026-10-13", End: "2026-10-14", Note: "funeral"}, "  It was a bereavement ", seen)
	if err != nil {
		t.Fatal(err)
	}
	if out.TypeName != f.types["compassionate"].Name || out.Days != WholeDays(2) || out.Start != "2026-10-13" || out.Status != StatusPending ||
		out.Note != "funeral" {
		t.Fatalf("corrected: %+v", out)
	}
	if out.ApproverID != r.ApproverID || out.Escalate != r.Escalate || out.CreatedAt != r.CreatedAt || out.ID != r.ID {
		t.Fatalf("approver/escalate/created_at/id changed: before %+v after %+v", r, out)
	}
	if out.Fingerprint() == seen {
		t.Fatal("fingerprint unchanged by a correction")
	}
	// An approver reading the old page must not approve what has changed.
	if _, err := f.svc.DecideSeen(f.ctx, f.people["head"], r.ID, true, "", seen); !errors.Is(err, ErrChanged) {
		t.Fatalf("stale decision: %v", err)
	}
	if len(f.events) != 1 || f.events[0].Kind != EventCorrected || f.events[0].Before == nil || f.events[0].Before.TypeName != r.TypeName ||
		f.events[0].Note != "It was a bereavement" || f.events[0].Actor.ID != f.people["hr"].ID {
		t.Fatalf("events: %+v", f.events)
	}
	cs := f.corrections(r.ID)
	if len(cs) != 1 {
		t.Fatalf("corrections: %+v", cs)
	}
	c := cs[0]
	if c.AdminName != "hr" || c.Reason != "It was a bereavement" || c.At != stampNow(f.svc.Now()) ||
		c.Before.TypeName != r.TypeName || c.Before.Days != r.Days || c.Before.Start != "2026-10-12" ||
		c.After.TypeName != out.TypeName || c.After.Days != WholeDays(2) || c.After.Note != "funeral" ||
		!reflect.DeepEqual(c.After.Ranges, []Range{{"2026-10-13", "2026-10-14"}}) {
		t.Fatalf("correction row: %+v", c)
	}
	// The approver can now decide the request as it reads.
	if _, err := f.svc.DecideSeen(f.ctx, f.people["head"], r.ID, true, "", out.Fingerprint()); err != nil {
		t.Fatal(err)
	}
	// Several corrections are all kept, oldest first.
	if _, err := f.correct("hr", r.ID, "annual", "2026-10-13", "2026-10-14", "x", "r"); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("approved leave is not correctable until 11d: %v", err)
	}
}

func TestCorrectKeepsEveryCorrection(t *testing.T) {
	f := newFixture(t)
	r := f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-14")
	f.mustCorrect("hr", r.ID, "compassionate", "2026-10-12", "2026-10-14", "n", "first")
	f.mustCorrect("hr", r.ID, "unpaid", "2026-10-12", "2026-10-13", "n", "second")
	cs := f.corrections(r.ID)
	if len(cs) != 2 || cs[0].Reason != "first" || cs[1].Reason != "second" ||
		cs[1].Before.TypeName != f.types["compassionate"].Name || cs[1].After.TypeName != f.types["unpaid"].Name {
		t.Fatalf("corrections: %+v", cs)
	}
}

func TestCorrectRefusals(t *testing.T) {
	type tc struct {
		name  string
		setup func(f *fixture) (Request, Input, string, string) // request, input, reason, seen
		admin func(f *fixture) users.User                       // nil: hr
		want  func(err error) bool
	}
	in := func(f *fixture, code, start, end string) Input {
		return Input{TypeID: f.types[code].ID, Start: start, End: end, Note: "n"}
	}
	field := func(key string) func(error) bool { return func(err error) bool { return fieldErr(err, key) } }
	notAllowed := func(err error) bool { return errors.Is(err, ErrNotAllowed) }
	status := func(s string) func(f *fixture) (Request, Input, string, string) {
		return func(f *fixture) (Request, Input, string, string) {
			r := f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-14")
			f.exec(`UPDATE requests SET status = ? WHERE id = ?`, s, r.ID)
			return r, in(f, "unpaid", "2026-10-12", "2026-10-14"), "why", ""
		}
	}
	cases := []tc{
		{"own request", func(f *fixture) (Request, Input, string, string) {
			r := f.mustSubmit("hr", "annual", "2026-10-12", "2026-10-14")
			return r, in(f, "unpaid", "2026-10-12", "2026-10-14"), "why", ""
		}, nil, field("correct")},
		{"empty reason", func(f *fixture) (Request, Input, string, string) {
			r := f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-14")
			return r, in(f, "unpaid", "2026-10-12", "2026-10-14"), "   ", ""
		}, nil, field("reason")},
		{"unchanged", func(f *fixture) (Request, Input, string, string) {
			r := f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-14")
			return r, in(f, "annual", "2026-10-12", "2026-10-14"), "why", ""
		}, nil, field("correct")},
		{"wrong seen", func(f *fixture) (Request, Input, string, string) {
			r := f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-14")
			return r, in(f, "unpaid", "2026-10-12", "2026-10-14"), "why", "0123456789abcdef01234567"
		}, nil, func(err error) bool { return errors.Is(err, ErrChanged) }},
		{"a step already logged", func(f *fixture) (Request, Input, string, string) {
			r := f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-14")
			f.exec(`INSERT INTO request_steps (request_id, approver_id, decision, note, decided_at) VALUES (?, ?, 'approved', '', 'x')`, r.ID, f.people["head"].ID)
			return r, in(f, "unpaid", "2026-10-12", "2026-10-14"), "why", ""
		}, nil, field("correct")},
		{"pending cancellation", func(f *fixture) (Request, Input, string, string) {
			r := f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-14")
			f.exec(`INSERT INTO requests (user_id, type_id, kind, cancels_request_id, start_date, end_date, days_x100, cycle_start, status, created_at)
				VALUES (?, ?, 'cancellation', ?, '2026-10-12', '2026-10-14', 300, '2026-01-01', 'pending', 'x')`, r.UserID, r.TypeID, r.ID)
			return r, in(f, "unpaid", "2026-10-12", "2026-10-14"), "why", ""
		}, nil, field("correct")},
		{"maternity for someone recorded male", func(f *fixture) (Request, Input, string, string) {
			f.setSex("staff", "m")
			r := f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-14")
			return r, in(f, "maternity", "2026-10-12", "2026-10-14"), "why", ""
		}, nil, field("type")},
		{"a type that is retired and not the request's own", func(f *fixture) (Request, Input, string, string) {
			r := f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-14")
			f.exec(`UPDATE leave_types SET active = 0 WHERE code = 'sick'`)
			return r, in(f, "sick", "2026-10-12", "2026-10-14"), "why", ""
		}, nil, field("type")},
		{"non-admin", func(f *fixture) (Request, Input, string, string) {
			r := f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-14")
			return r, in(f, "unpaid", "2026-10-12", "2026-10-14"), "why", ""
		}, func(f *fixture) users.User { return f.people["head"] }, notAllowed},
		{"disabled admin", func(f *fixture) (Request, Input, string, string) {
			r := f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-14")
			return r, in(f, "unpaid", "2026-10-12", "2026-10-14"), "why", ""
		}, func(f *fixture) users.User { u := f.people["hr"]; u.Disabled = true; return u }, notAllowed},
		{"approved", status(StatusApproved), nil, notAllowed},
		{"declined", status(StatusDeclined), nil, notAllowed},
		{"withdrawn", status(StatusWithdrawn), nil, notAllowed},
		{"cancelled", status(StatusCancelled), nil, notAllowed},
		{"shutdown request", func(f *fixture) (Request, Input, string, string) {
			f.december(false)
			d := f.drafts("staff")
			if len(d) == 0 {
				f.t.Fatal("no shutdown draft")
			}
			return d[0], in(f, "unpaid", "2026-12-07", "2026-12-08"), "why", ""
		}, nil, notAllowed},
		{"recorded leave", func(f *fixture) (Request, Input, string, string) {
			r := f.mustRecord("staff", "annual", "2026-09-14", "2026-09-15")
			return r, in(f, "unpaid", "2026-09-14", "2026-09-15"), "why", ""
		}, nil, notAllowed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFixture(t)
			r, input, reason, seen := c.setup(f)
			admin := f.people["hr"]
			if c.admin != nil {
				admin = c.admin(f)
			}
			before := f.mustGetRow(r.ID)
			f.events = nil
			_, err := f.svc.Correct(f.ctx, admin, r.ID, input, reason, seen)
			if err == nil || !c.want(err) {
				t.Fatalf("got %v", err)
			}
			if after := f.mustGetRow(r.ID); !reflect.DeepEqual(before, after) {
				t.Fatalf("a refused correction changed the request:\n%+v\n%+v", before, after)
			}
			if len(f.events) != 0 || len(f.corrections(r.ID)) != 0 {
				t.Fatalf("a refused correction left events %+v / history", f.events)
			}
		})
	}
}

func (f *fixture) mustGetRow(id int64) Request {
	f.t.Helper()
	r, err := f.svc.Get(f.ctx, id)
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

func TestCorrectWaitingForHRThenAllowedAndSubmitted(t *testing.T) {
	f := newFixture(t)
	f.exhaustAnnual()
	r, err := f.negative("staff", "annual", "2026-11-02", "2026-11-04", "short of annual")
	if err != nil || r.Status != StatusAwaitingHR {
		t.Fatalf("setup: %+v %v", r, err)
	}
	out := f.mustCorrect("hr", r.ID, "unpaid", "2026-11-02", "2026-11-04", "short of annual", "Unpaid is what they need")
	if out.Status != StatusAwaitingHR || out.TypeName != f.types["unpaid"].Name || out.ApproverID != 0 {
		t.Fatalf("corrected: %+v", out)
	}
	granted, err := f.svc.GrantNegative(f.ctx, f.people["hr"], r.ID, "")
	if err != nil || granted.Status != StatusDraft {
		t.Fatalf("grant: %+v %v", granted, err)
	}
	// An HR-allowed draft is correctable too, and stays submittable.
	drafted := f.mustCorrect("hr", r.ID, "unpaid", "2026-11-03", "2026-11-05", "short of annual", "dates were a day out")
	if drafted.Status != StatusDraft || !drafted.AllowNegative {
		t.Fatalf("draft corrected: %+v", drafted)
	}
	sub, err := f.svc.SubmitDraft(f.ctx, f.people["staff"], r.ID, "")
	if err != nil || sub.Status != StatusPending || sub.TypeName != f.types["unpaid"].Name || sub.Start != "2026-11-03" {
		t.Fatalf("submit: %+v %v", sub, err)
	}
}

func TestCorrectOverlapStillRefused(t *testing.T) {
	f := newFixture(t)
	f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-14")
	b := f.mustSubmit("staff", "annual", "2026-10-19", "2026-10-21")
	if _, err := f.correct("hr", b.ID, "unpaid", "2026-10-14", "2026-10-15", "n", "why"); !fieldErr(err, "start") {
		t.Fatalf("overlap: %v", err)
	}
	// Its own old dates are not an overlap.
	if _, err := f.correct("hr", b.ID, "unpaid", "2026-10-20", "2026-10-22", "n", "why"); err != nil {
		t.Fatal(err)
	}
}

func TestCorrectRelaxationsAndBalance(t *testing.T) {
	f := newFixture(t)
	r := f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-14")
	// Far in the past, beyond any backdating limit, and beyond the 31-day span.
	out := f.mustCorrect("hr", r.ID, "annual", "2026-01-05", "2026-02-27", "n", "wrong year")
	if out.Start != "2026-01-05" || out.Days != WholeDays(40) || !out.AllowNegative || out.Status != StatusPending {
		t.Fatalf("relaxed: %+v", out)
	}
	if out.CycleStart != "2026-01-01" {
		t.Fatalf("cycle: %s", out.CycleStart)
	}
	back := f.mustCorrect("hr", r.ID, "annual", "2026-10-12", "2026-10-14", "n", "back")
	if back.AllowNegative || back.Days != WholeDays(3) {
		t.Fatalf("within balance: %+v", back)
	}
	// Future dates work too: the leave is not "recorded".
	if _, err := f.correct("hr", r.ID, "annual", "2026-12-01", "2026-12-02", "n", "later"); err != nil {
		t.Fatal(err)
	}
}

func TestCorrectKeepsRetiredOwnType(t *testing.T) {
	f := newFixture(t)
	r := f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-14")
	f.exec(`UPDATE leave_types SET active = 0 WHERE code = 'annual'`)
	out := f.mustCorrect("hr", r.ID, "annual", "2026-10-13", "2026-10-14", "n", "a day too many")
	if out.TypeName != f.types["annual"].Name || out.Days != WholeDays(2) {
		t.Fatalf("%+v", out)
	}
}

func TestCorrectMultiRangeInAndOut(t *testing.T) {
	f := newFixture(t)
	two := []Range{{"2026-10-12", "2026-10-13"}, {"2026-10-19", "2026-10-20"}}
	r, err := f.svc.Submit(f.ctx, f.people["staff"], Input{TypeID: f.types["annual"].ID, Ranges: two, Note: "n"})
	if err != nil || len(r.Ranges) != 2 {
		t.Fatalf("setup: %+v %v", r, err)
	}
	three := []Range{{"2026-10-26", "2026-10-26"}, {"2026-11-02", "2026-11-03"}, {"2026-11-09", "2026-11-09"}}
	out, err := f.svc.Correct(f.ctx, f.people["hr"], r.ID, Input{TypeID: f.types["annual"].ID, Ranges: three, Note: "n"}, "three stretches", r.Fingerprint())
	if err != nil || !reflect.DeepEqual(out.Ranges, three) || out.Days != WholeDays(4) || out.Start != "2026-10-26" || out.End != "2026-11-09" {
		t.Fatalf("to three: %+v %v", out, err)
	}
	single, err := f.svc.Correct(f.ctx, f.people["hr"], r.ID, Input{TypeID: f.types["annual"].ID, Start: "2026-10-12", End: "2026-10-14", Note: "n"}, "one stretch", "")
	if err != nil || len(single.Ranges) != 1 || single.Ranges[0] != (Range{"2026-10-12", "2026-10-14"}) {
		t.Fatalf("to one: %+v %v", single, err)
	}
	cs := f.corrections(r.ID)
	if len(cs) != 2 || !reflect.DeepEqual(cs[0].Before.Ranges, two) || !reflect.DeepEqual(cs[0].After.Ranges, three) {
		t.Fatalf("history: %+v", cs)
	}
	// Ranges that touch are merged before they are compared: nothing changed.
	if _, err := f.svc.Correct(f.ctx, f.people["hr"], r.ID, Input{TypeID: f.types["annual"].ID, Ranges: []Range{{"2026-10-12", "2026-10-13"}, {"2026-10-14", "2026-10-14"}}, Note: "n"}, "same", ""); !fieldErr(err, "correct") {
		t.Fatalf("merged ranges are unchanged: %v", err)
	}
}

func TestCorrectSecondAdminCorrectsAnAdminsOwnRequest(t *testing.T) {
	f := newFixture(t)
	f.setAdmin("head")
	r := f.mustSubmit("hr", "annual", "2026-10-12", "2026-10-14")
	if _, err := f.correct("hr", r.ID, "unpaid", "2026-10-12", "2026-10-14", "n", "why"); !fieldErr(err, "correct") {
		t.Fatalf("own: %v", err)
	}
	out, err := f.correct("head", r.ID, "unpaid", "2026-10-12", "2026-10-14", "n", "why")
	if err != nil || out.TypeName != f.types["unpaid"].Name {
		t.Fatalf("other admin: %+v %v", out, err)
	}
}

func TestPreviewCorrect(t *testing.T) {
	f := newFixture(t)
	r := f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-14")
	qt, old, err := f.svc.PreviewCorrect(f.ctx, f.people["hr"], r.ID, Input{TypeID: f.types["compassionate"].ID, Start: "2026-10-12", End: "2026-10-13", Note: "x"})
	if err != nil || qt.Days != WholeDays(2) || old.Days != WholeDays(3) {
		t.Fatalf("%+v %+v %v", qt, old, err)
	}
	if _, _, err := f.svc.PreviewCorrect(f.ctx, f.people["staff"], r.ID, Input{}); !errors.Is(err, ErrNotAllowed) {
		t.Fatalf("staff: %v", err)
	}
}

// Correcting never touches a shutdown request, so deleting a shutdown meets no
// correction row (request_corrections references requests).
func TestDeleteShutdownWithCorrectionsElsewhere(t *testing.T) {
	f := newFixture(t)
	r := f.mustSubmit("staff", "annual", "2026-10-12", "2026-10-14")
	f.mustCorrect("hr", r.ID, "unpaid", "2026-10-12", "2026-10-14", "n", "why")
	id := f.december(false)
	if err := f.svc.DeleteShutdown(f.ctx, f.people["hr"], id); err != nil {
		t.Fatalf("delete shutdown: %v", err)
	}
	if len(f.corrections(r.ID)) != 1 {
		t.Fatal("the correction history disappeared")
	}
}
