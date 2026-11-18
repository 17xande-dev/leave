package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/17xande-dev/leave/internal/calendar"
	"github.com/17xande-dev/leave/internal/dbtest"
	"github.com/17xande-dev/leave/internal/directory"
	"github.com/17xande-dev/leave/internal/leave"
	"github.com/17xande-dev/leave/internal/notify"
	"github.com/17xande-dev/leave/internal/outbox"
	"github.com/17xande-dev/leave/internal/users"
)

// graphLikeCalendar behaves like Graph where it matters here: a create with a
// transactionId already used returns the first event's id (even if that event
// was deleted since, so it is not live again), and deleting a gone event is
// success.
type graphLikeCalendar struct {
	byKey      map[string]string
	live       map[string]calendar.Event
	keys       []string // every key Create was called with, in order
	deleted    []string // every id Delete was called with, in order
	n          int
	failCreate int // fail the Nth Create call (1-based), once
	calls      int
}

func newGraphLike() *graphLikeCalendar {
	return &graphLikeCalendar{byKey: map[string]string{}, live: map[string]calendar.Event{}}
}

func (c *graphLikeCalendar) Create(_ context.Context, e calendar.Event) (string, error) {
	c.calls++
	if c.calls == c.failCreate {
		return "", errors.New("create interrupted")
	}
	c.keys = append(c.keys, e.Key)
	if id, ok := c.byKey[e.Key]; ok {
		return id, nil
	}
	c.n++
	id := fmt.Sprintf("ev%d", c.n)
	c.byKey[e.Key] = id
	c.live[id] = e
	return id, nil
}

func (c *graphLikeCalendar) Delete(_ context.Context, id string) error {
	c.deleted = append(c.deleted, id)
	delete(c.live, id)
	return nil
}

// liveRanges is the dates of the events on the calendar, sorted.
func (c *graphLikeCalendar) liveRanges() []string {
	var out []string
	for _, e := range c.live {
		out = append(out, e.Start+".."+e.End+" "+e.Subject)
	}
	sort.Strings(out)
	return out
}

type correctRig struct {
	t       *testing.T
	ctx     context.Context
	db      *sql.DB
	svc     *leave.Service
	cal     *graphLikeCalendar
	create  outbox.Handler
	remove  outbox.Handler
	boss    users.User
	admin   users.User
	staff   users.User
	typeIDs map[string]int64
	seen    int64 // outbox rows already handed out
}

type job struct {
	kind    string
	payload []byte
}

func newCorrectRig(t *testing.T) *correctRig {
	t.Helper()
	ctx := context.Background()
	d := dbtest.New(t)
	us := users.NewStore(d)
	svc := leave.New(d, us, leave.Config{Location: time.UTC})
	svc.Now = func() time.Time { return time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC) }
	if err := svc.SeedTypes(ctx); err != nil {
		t.Fatal(err)
	}
	boss, _ := us.Upsert(ctx, directory.User{ID: "CN=boss", DN: "CN=boss", Username: "boss", Name: "Big Boss", Email: "boss@x.test"})
	root, _ := us.Upsert(ctx, directory.User{ID: "CN=root", DN: "CN=root", Username: "root", Name: "Root Admin", Email: "root@x.test", Admin: true})
	st, _ := svc.Settings(ctx)
	st.FinalApproverID = boss.ID
	if err := svc.SaveSettings(ctx, root, st); err != nil {
		t.Fatal(err)
	}
	staff, _ := us.Upsert(ctx, directory.User{ID: "CN=s", DN: "CN=s", Username: "s", Name: "Sam Staff", Email: "s@x.test", ManagerDN: "CN=boss"})
	types, _ := svc.Types(ctx, true)
	ids := map[string]int64{}
	for _, ty := range types {
		ids[ty.Code] = ty.ID
	}
	svc.OnEvent(notify.New("https://leave.test").Hook)
	cal := newGraphLike()
	create, remove := calendarHandlers(cal, svc)
	return &correctRig{t: t, ctx: ctx, db: d, svc: svc, cal: cal, create: create, remove: remove,
		boss: boss, admin: root, staff: staff, typeIDs: ids}
}

// jobs returns the calendar jobs queued since the last call, oldest first.
// Tests run them by hand, in the order a scenario needs, rather than letting
// a worker race the next step.
func (r *correctRig) jobs() []job {
	r.t.Helper()
	rows, err := r.db.QueryContext(r.ctx, `SELECT id, kind, payload FROM outbox WHERE id > ? AND kind <> 'email' ORDER BY id`, r.seen)
	if err != nil {
		r.t.Fatal(err)
	}
	defer rows.Close()
	var out []job
	for rows.Next() {
		var id int64
		var j job
		var p string
		if err := rows.Scan(&id, &j.kind, &p); err != nil {
			r.t.Fatal(err)
		}
		j.payload = []byte(p)
		r.seen = id
		out = append(out, j)
	}
	if err := rows.Err(); err != nil {
		r.t.Fatal(err)
	}
	// Emails are skipped above but still advance nothing: the cursor only
	// follows calendar rows, which is all this reads.
	return out
}

func (r *correctRig) run(j job) error {
	if j.kind == outbox.KindCalendarCreate {
		return r.create(r.ctx, j.payload)
	}
	return r.remove(r.ctx, j.payload)
}

func (r *correctRig) mustRun(js ...job) {
	r.t.Helper()
	for _, j := range js {
		if err := r.run(j); err != nil {
			r.t.Fatal(err)
		}
	}
}

// approved books and approves annual leave over the ranges and runs its
// calendar job, so every range has an event.
func (r *correctRig) approved(ranges ...leave.Range) leave.Request {
	r.t.Helper()
	req, err := r.svc.Submit(r.ctx, r.staff, leave.Input{TypeID: r.typeIDs["annual"], Ranges: ranges})
	if err != nil {
		r.t.Fatal(err)
	}
	if _, err := r.svc.Decide(r.ctx, r.boss, req.ID, true, ""); err != nil {
		r.t.Fatal(err)
	}
	js := r.jobs()
	if len(js) != 1 {
		r.t.Fatalf("approval queued %d calendar jobs", len(js))
	}
	r.mustRun(js...)
	return req
}

func (r *correctRig) correct(id int64, code string, ranges ...leave.Range) {
	r.t.Helper()
	if _, err := r.svc.Correct(r.ctx, r.admin, id, leave.Input{TypeID: r.typeIDs[code], Ranges: ranges, Note: "n"}, "fix", ""); err != nil {
		r.t.Fatal(err)
	}
}

func (r *correctRig) eventIDs(id int64) []string {
	r.t.Helper()
	rs, err := r.svc.CalendarRanges(r.ctx, id)
	if err != nil {
		r.t.Fatal(err)
	}
	var out []string
	for _, x := range rs {
		out = append(out, x.EventID)
	}
	return out
}

func rg(s, e string) leave.Range { return leave.Range{Start: s, End: e} }

// The job deletes every listed id, then creates one event per range under the
// -c1 key, so Graph's transactionId dedupe cannot return the deleted event.
func TestCorrectApprovedRecreatesCalendarEvents(t *testing.T) {
	r := newCorrectRig(t)
	req := r.approved(rg("2026-10-12", "2026-10-12"), rg("2026-10-14", "2026-10-14"))
	old := r.eventIDs(req.ID)
	if len(old) != 2 || old[0] == "" || old[1] == "" || len(r.cal.live) != 2 {
		t.Fatalf("setup: ids %v, live %v", old, r.cal.live)
	}
	r.correct(req.ID, "compassionate", rg("2026-10-12", "2026-10-13"))
	js := r.jobs()
	if len(js) != 1 || js[0].kind != outbox.KindCalendarCreate {
		t.Fatalf("jobs: %+v", js)
	}
	want := fmt.Sprintf(`{"request_id":%d,"delete_event_ids":[%q,%q]}`, req.ID, old[0], old[1])
	if string(js[0].payload) != want {
		t.Fatalf("payload %s, want %s", js[0].payload, want)
	}
	r.cal.deleted, r.cal.keys = nil, nil
	r.mustRun(js...)
	if !slices.Equal(r.cal.deleted, old) {
		t.Fatalf("deleted %v, want %v", r.cal.deleted, old)
	}
	if want := []string{fmt.Sprintf("leave-request-%d-c1", req.ID)}; !slices.Equal(r.cal.keys, want) {
		t.Fatalf("create keys %v, want %v", r.cal.keys, want)
	}
	if got, want := r.cal.liveRanges(), []string{"2026-10-12..2026-10-13 Sam Staff — Compassionate leave"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("calendar %v, want %v", got, want)
	}
	// A retry of the same job changes nothing.
	r.mustRun(js...)
	if len(r.cal.live) != 1 || len(r.cal.keys) != 1 {
		t.Fatalf("retry changed the calendar: %v keys %v", r.cal.live, r.cal.keys)
	}
}

// A Graph failure on the second create is retried without re-creating the
// first, and without a duplicate.
func TestCorrectApprovedRetryCreatesOnlyMissingRange(t *testing.T) {
	r := newCorrectRig(t)
	req := r.approved(rg("2026-10-12", "2026-10-14"))
	r.correct(req.ID, "annual", rg("2026-10-19", "2026-10-19"), rg("2026-10-21", "2026-10-21"))
	js := r.jobs()
	r.cal.keys = nil
	r.cal.failCreate = r.cal.calls + 2
	if err := r.run(js[0]); err == nil {
		t.Fatal("expected the second create to fail")
	}
	ids := r.eventIDs(req.ID)
	if ids[0] == "" || ids[1] != "" {
		t.Fatalf("after the failure: %v", ids)
	}
	r.mustRun(js[0])
	c1 := fmt.Sprintf("leave-request-%d-c1", req.ID)
	if want := []string{c1, c1 + "-2026-10-21"}; !slices.Equal(r.cal.keys, want) {
		t.Fatalf("create keys %v, want %v (the first range must not be created twice)", r.cal.keys, want)
	}
	if len(r.cal.live) != 2 {
		t.Fatalf("calendar: %v", r.cal.liveRanges())
	}
}

// A Graph 404 on a stale event is success: the event is already gone.
func TestCorrectApprovedDeleteNotFoundIsSuccess(t *testing.T) {
	r := newCorrectRig(t)
	var calls []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls = append(calls, req.Method+" "+req.URL.Path)
		if req.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"id":"fresh"}`)
	}))
	defer server.Close()
	create, _ := calendarHandlers(&calendar.Graph{Tokens: calendarTestToken{}, BaseURL: server.URL, Mailbox: "shared"}, r.svc)

	req, err := r.svc.Submit(r.ctx, r.staff, leave.Input{TypeID: r.typeIDs["annual"], Start: "2026-10-12", End: "2026-10-14"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Decide(r.ctx, r.boss, req.ID, true, ""); err != nil {
		t.Fatal(err)
	}
	r.jobs() // the approval's own job is not run: the event id is set by hand
	if err := r.svc.SetRangeEventID(r.ctx, req.ID, "2026-10-12", "gone"); err != nil {
		t.Fatal(err)
	}
	r.correct(req.ID, "compassionate", rg("2026-10-12", "2026-10-13"))
	js := r.jobs()
	if err := create(r.ctx, js[0].payload); err != nil {
		t.Fatalf("a 404 on delete failed the job: %v", err)
	}
	if want := []string{"DELETE /v1.0/users/shared/events/gone", "POST /v1.0/users/shared/events"}; !slices.Equal(calls, want) {
		t.Fatalf("Graph calls %v, want %v", calls, want)
	}
	if ids := r.eventIDs(req.ID); !slices.Equal(ids, []string{"fresh"}) {
		t.Fatalf("ids %v", ids)
	}
}

// A correction followed by a cancellation before the worker runs: the create
// still deletes the stale events (it deletes before its status check) and
// creates nothing; the cancellation's delete finds no ids.
func TestCorrectApprovedThenCancelledBeforeJobRuns(t *testing.T) {
	r := newCorrectRig(t)
	req := r.approved(rg("2026-10-12", "2026-10-14"))
	old := r.eventIDs(req.ID)
	r.correct(req.ID, "compassionate", rg("2026-10-12", "2026-10-13"))
	c, err := r.svc.RequestCancellation(r.ctx, r.staff, req.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.svc.Decide(r.ctx, r.boss, c.ID, true, ""); err != nil {
		t.Fatal(err)
	}
	js := r.jobs()
	if len(js) != 2 || js[0].kind != outbox.KindCalendarCreate || js[1].kind != outbox.KindCalendarDelete {
		t.Fatalf("jobs: %+v", js)
	}
	r.cal.deleted, r.cal.keys = nil, nil
	r.mustRun(js...)
	if !slices.Equal(r.cal.deleted, old) {
		t.Fatalf("stale events %v were not deleted (deleted %v)", old, r.cal.deleted)
	}
	if len(r.cal.keys) != 0 || len(r.cal.live) != 0 {
		t.Fatalf("created for cancelled leave: keys %v live %v", r.cal.keys, r.cal.liveRanges())
	}
}

// Two corrections before any job runs: the second captured no ids (the first
// blanked them), and the first job creates the newest ranges under -c2, so the
// second job has nothing left to do.
func TestCorrectApprovedTwiceBeforeAnyJobRuns(t *testing.T) {
	r := newCorrectRig(t)
	req := r.approved(rg("2026-10-12", "2026-10-14"))
	old := r.eventIDs(req.ID)
	r.correct(req.ID, "compassionate", rg("2026-10-12", "2026-10-13"))
	r.correct(req.ID, "annual", rg("2026-10-19", "2026-10-20"))
	js := r.jobs()
	if len(js) != 2 {
		t.Fatalf("jobs: %+v", js)
	}
	if !strings.Contains(string(js[0].payload), old[0]) || strings.Contains(string(js[1].payload), "delete_event_ids") {
		t.Fatalf("payloads: %s / %s", js[0].payload, js[1].payload)
	}
	r.cal.keys = nil
	r.mustRun(js...)
	if want := []string{fmt.Sprintf("leave-request-%d-c2", req.ID)}; !slices.Equal(r.cal.keys, want) {
		t.Fatalf("create keys %v, want %v", r.cal.keys, want)
	}
	if got, want := r.cal.liveRanges(), []string{"2026-10-19..2026-10-20 Sam Staff — Annual leave"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("calendar %v, want %v", got, want)
	}
}

// Two corrections with the first job run in between: the second captures the
// events the first made and deletes them, and the keys are -c1 then -c2, so
// nothing is orphaned and Graph cannot hand back a deleted event.
func TestCorrectApprovedTwiceWithJobBetween(t *testing.T) {
	r := newCorrectRig(t)
	req := r.approved(rg("2026-10-12", "2026-10-14"))
	r.correct(req.ID, "compassionate", rg("2026-10-12", "2026-10-13"))
	r.mustRun(r.jobs()...)
	if len(r.cal.live) != 1 {
		t.Fatalf("after the first job: %v", r.cal.liveRanges())
	}
	r.correct(req.ID, "annual", rg("2026-10-19", "2026-10-20"))
	r.mustRun(r.jobs()...)
	c := fmt.Sprintf("leave-request-%d", req.ID)
	if want := []string{c, c + "-c1", c + "-c2"}; !slices.Equal(r.cal.keys, want) {
		t.Fatalf("create keys %v, want %v", r.cal.keys, want)
	}
	if got, want := r.cal.liveRanges(), []string{"2026-10-19..2026-10-20 Sam Staff — Annual leave"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("calendar %v, want %v (an orphan, or a missing event)", got, want)
	}
	ids := r.eventIDs(req.ID)
	if len(ids) != 1 || ids[0] == "" || r.cal.live[ids[0]].Start != "2026-10-19" {
		t.Fatalf("range ids %v", ids)
	}
}
