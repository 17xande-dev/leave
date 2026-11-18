package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/17xande-dev/mailer"

	"github.com/17xande-dev/leave/internal/calendar"
	"github.com/17xande-dev/leave/internal/dbtest"
	"github.com/17xande-dev/leave/internal/directory"
	"github.com/17xande-dev/leave/internal/leave"
	"github.com/17xande-dev/leave/internal/notify"
	"github.com/17xande-dev/leave/internal/outbox"
	"github.com/17xande-dev/leave/internal/users"
)

func rangeCalendarFixture(t *testing.T) (context.Context, *sql.DB, *leave.Service, int64, []byte) {
	t.Helper()
	ctx := context.Background()
	d := dbtest.New(t)
	us := users.NewStore(d)
	svc := leave.New(d, us, leave.Config{Location: time.UTC})
	if err := svc.SeedTypes(ctx); err != nil {
		t.Fatal(err)
	}
	u, err := us.Upsert(ctx, directory.User{ID: "calendar-person", DN: "calendar-person", Username: "calendar-person", Name: "Sam"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := seedRequest(d, `INSERT INTO requests(user_id,type_id,kind,start_date,end_date,days_x100,cycle_start,status,created_at)
		VALUES(?,(SELECT id FROM leave_types WHERE code='annual'),'leave','2026-11-02','2026-11-06',300,'2026-01-01','approved','')`, u.ID)
	if err != nil {
		t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return ctx, d, svc, id, []byte(fmt.Sprintf(`{"request_id":%d}`, id))
}

// Fail a chosen call while retaining earlier successes, as Graph does when a
// worker loses connectivity part-way through one request's calendar jobs.
type rangeCalendar struct {
	created                []calendar.Event
	deleted                []string
	failCreate, failDelete int
}

func (c *rangeCalendar) Create(_ context.Context, e calendar.Event) (string, error) {
	c.created = append(c.created, e)
	if len(c.created) == c.failCreate {
		return "", errors.New("create interrupted")
	}
	return e.Key, nil
}

func (c *rangeCalendar) Delete(_ context.Context, id string) error {
	c.deleted = append(c.deleted, id)
	if len(c.deleted) == c.failDelete {
		return errors.New("delete interrupted")
	}
	return nil
}

func TestCalendarRangesResumePartialJobs(t *testing.T) {
	ctx, d, svc, id, payload := rangeCalendarFixture(t)
	// Multi-range input is not user-reachable until Phase 5; seed its storage
	// directly to exercise the calendar boundary independently of quoting.
	if _, err := d.Exec(`DELETE FROM request_ranges WHERE request_id=?`, id); err != nil {
		t.Fatal(err)
	}
	for _, day := range []string{"2026-11-02", "2026-11-04", "2026-11-06"} {
		if _, err := d.Exec(`INSERT INTO request_ranges(request_id,start_date,end_date) VALUES(?,?,?)`, id, day, day); err != nil {
			t.Fatal(err)
		}
	}
	cal := &rangeCalendar{failCreate: 2, failDelete: 2}
	create, remove := calendarHandlers(cal, svc)
	if err := create(ctx, payload); err == nil {
		t.Fatal("expected interrupted create")
	}
	ranges, err := svc.CalendarRanges(ctx, id)
	if err != nil || len(ranges) != 3 || ranges[0].EventID == "" || ranges[1].EventID != "" || ranges[2].EventID != "" {
		t.Fatalf("partial create state=%+v, %v", ranges, err)
	}
	if err := create(ctx, payload); err != nil {
		t.Fatal(err)
	}
	if err := create(ctx, payload); err != nil {
		t.Fatal(err)
	}
	first := fmt.Sprintf("leave-request-%d", id)
	want := []calendar.Event{
		{Key: first, Start: "2026-11-02", End: "2026-11-02"},
		{Key: first + "-2026-11-04", Start: "2026-11-04", End: "2026-11-04"},
		{Key: first + "-2026-11-04", Start: "2026-11-04", End: "2026-11-04"},
		{Key: first + "-2026-11-06", Start: "2026-11-06", End: "2026-11-06"},
	}
	got := append([]calendar.Event(nil), cal.created...)
	for i := range got {
		got[i].Subject, got[i].Body = "", ""
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("create calls=%+v want %+v", got, want)
	}
	if err := remove(ctx, payload); err == nil {
		t.Fatal("expected interrupted delete")
	}
	ranges, err = svc.CalendarRanges(ctx, id)
	if err != nil || ranges[0].EventID != "" || ranges[1].EventID == "" || ranges[2].EventID == "" {
		t.Fatalf("partial delete state=%+v, %v", ranges, err)
	}
	if err := remove(ctx, payload); err != nil {
		t.Fatal(err)
	}
	if err := remove(ctx, payload); err != nil {
		t.Fatal(err)
	}
	if want := []string{first, first + "-2026-11-04", first + "-2026-11-04", first + "-2026-11-06"}; !reflect.DeepEqual(cal.deleted, want) {
		t.Fatalf("delete calls=%v want %v", cal.deleted, want)
	}
	ranges, err = svc.CalendarRanges(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range ranges {
		if r.EventID != "" {
			t.Fatalf("event survived delete: %+v", ranges)
		}
	}
}

type calendarTestToken struct{}

func (calendarTestToken) Token(context.Context) (string, error) { return "test", nil }

func TestCalendarDeleteMissingGraphEvent(t *testing.T) {
	ctx, _, svc, id, payload := rangeCalendarFixture(t)
	if err := svc.SetRangeEventID(ctx, id, "2026-11-02", "gone-event"); err != nil {
		t.Fatal(err)
	}
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()
	_, remove := calendarHandlers(&calendar.Graph{Tokens: calendarTestToken{}, BaseURL: server.URL, Mailbox: "shared"}, svc)
	for range 2 {
		if err := remove(ctx, payload); err != nil {
			t.Fatal(err)
		}
	}
	if want := []string{"DELETE /v1.0/users/shared/events/gone-event"}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("Graph requests=%v want %v", paths, want)
	}
	ranges, err := svc.CalendarRanges(ctx, id)
	if err != nil || len(ranges) != 1 || ranges[0].EventID != "" {
		t.Fatalf("missing event wasn't cleared: %+v, %v", ranges, err)
	}
}

// The whole side-effect path, joined up the way main wires it: a change
// commits outbox rows, the worker runs them, and the calendar and mailbox end
// up in the right state -- including when a cancellation overtakes a create
// that has not run yet.
func TestSideEffectsEndToEnd(t *testing.T) {
	ctx := context.Background()
	d := dbtest.New(t)
	us := users.NewStore(d)
	svc := leave.New(d, us, leave.Config{Location: time.UTC})
	svc.Now = func() time.Time { return time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC) }
	svc.SeedTypes(ctx)
	boss, _ := us.Upsert(ctx, directory.User{ID: "CN=boss", DN: "CN=boss", Username: "boss", Name: "Big Boss", Email: "boss@x.test"})
	// Settings are saved by an admin, so one exists; they have no manager, so
	// they take no part in the chain under test.
	root, _ := us.Upsert(ctx, directory.User{ID: "CN=root", DN: "CN=root", Username: "root", Name: "Root", Admin: true})
	st, _ := svc.Settings(ctx)
	st.FinalApproverID = boss.ID
	st.HalfDaysEnabled = true
	if err := svc.SaveSettings(ctx, root, st); err != nil {
		t.Fatal(err)
	}
	staff, _ := us.Upsert(ctx, directory.User{ID: "CN=s", DN: "CN=s", Username: "s", Name: "Sam Staff", Email: "s@x.test", ManagerDN: "CN=boss"})
	types, _ := svc.Types(ctx, true)

	mail := mailer.NewFake()
	cal := &calendar.Log{}
	create, remove := calendarHandlers(cal, svc)
	w := outbox.NewWorker(outbox.NewStore(d), map[string]outbox.Handler{
		outbox.KindEmail: emailHandler(mail), outbox.KindCalendarCreate: create, outbox.KindCalendarDelete: remove,
	})
	svc.OnEvent(notify.New("https://leave.test").Hook)

	r, err := svc.Submit(ctx, staff, leave.Input{TypeID: types[0].ID, Start: "2026-10-12", End: "2026-10-16"})
	if err != nil {
		t.Fatal(err)
	}
	svc.Decide(ctx, boss, r.ID, true, "")
	w.RunDue(ctx)

	if len(mail.To("boss@x.test")) != 1 || len(mail.To("s@x.test")) != 1 {
		t.Fatalf("mail: boss %d, staff %d", len(mail.To("boss@x.test")), len(mail.To("s@x.test")))
	}
	r, _ = svc.Get(ctx, r.ID)
	ranges, err := svc.CalendarRanges(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	ev, ok := cal.Events[ranges[0].EventID]
	if !ok || ev.Subject != "Sam Staff — Annual leave" || ev.Start != "2026-10-12" || ev.End != "2026-10-16" {
		t.Fatalf("calendar = %+v, request ranges %+v", cal.Events, ranges)
	}
	if want := "Annual leave, Mon 12 Oct – Fri 16 Oct 2026.\nApproved by Big Boss."; ev.Body != want {
		t.Fatalf("single-range event body changed: %q, want %q", ev.Body, want)
	}
	// Running the create again (a retry) must not add a second event.
	w.RunDue(ctx)
	if len(cal.Events) != 1 {
		t.Fatalf("%d events after a second pass", len(cal.Events))
	}

	c, _ := svc.RequestCancellation(ctx, staff, r.ID, "")
	svc.Decide(ctx, boss, c.ID, true, "")
	w.RunDue(ctx)
	if len(cal.Events) != 0 {
		t.Fatalf("event survived the cancellation: %+v", cal.Events)
	}

	// A real multi-range approval creates separate events, each identifying its
	// place in the whole request; cancellation must remove both of them.
	multi, err := svc.Submit(ctx, staff, leave.Input{TypeID: types[0].ID,
		Ranges: []leave.Range{{Start: "2026-10-19", End: "2026-10-19"}, {Start: "2026-10-21", End: "2026-10-21"}}, HalfStart: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Decide(ctx, boss, multi.ID, true, ""); err != nil {
		t.Fatal(err)
	}
	w.RunDue(ctx)
	if len(cal.Events) != 2 {
		t.Fatalf("multi-range approval created %d events, want 2", len(cal.Events))
	}
	multiRanges, err := svc.CalendarRanges(ctx, multi.ID)
	if err != nil || len(multiRanges) != 2 {
		t.Fatalf("multi-range events = %+v, %v", multiRanges, err)
	}
	for i, rr := range multiRanges {
		ev, ok := cal.Events[rr.EventID]
		want := fmt.Sprintf("Annual leave, %s.\nApproved by Big Boss.\nPart %d of 2.", notify.Dates(rr.Start, rr.End), i+1)
		if !ok || ev.Start != rr.Start || ev.End != rr.End || ev.Body != want {
			t.Fatalf("multi-range event = %+v, want %q", ev, want)
		}
	}
	cMulti, err := svc.RequestCancellation(ctx, staff, multi.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cMulti.Ranges, multi.Ranges) {
		t.Fatalf("cancellation ranges = %+v, want %+v", cMulti.Ranges, multi.Ranges)
	}
	cancelRanges, err := svc.CalendarRanges(ctx, cMulti.ID)
	if err != nil || len(cancelRanges) != 2 {
		t.Fatalf("cancellation event rows = %+v, %v", cancelRanges, err)
	}
	for _, rr := range cancelRanges {
		if rr.EventID != "" {
			t.Fatalf("cancellation copied event ids: %+v", cancelRanges)
		}
	}
	if _, err := svc.Decide(ctx, boss, cMulti.ID, true, ""); err != nil {
		t.Fatal(err)
	}
	w.RunDue(ctx)
	if len(cal.Events) != 0 {
		t.Fatalf("events survived multi-range cancellation: %+v", cal.Events)
	}

	// Half-day corrections do not need to patch calendar event bodies.
	rh, err := svc.Submit(ctx, staff, leave.Input{TypeID: types[0].ID, Start: "2026-11-09", End: "2026-11-10", HalfEnd: true})
	if err != nil {
		t.Fatal(err)
	}
	svc.Decide(ctx, boss, rh.ID, true, "")
	w.RunDue(ctx)
	rh, _ = svc.Get(ctx, rh.ID)
	ranges, err = svc.CalendarRanges(ctx, rh.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ev, ok := cal.Events[ranges[0].EventID]; !ok || strings.Contains(ev.Body, "half day") {
		t.Fatalf("half-day event = %+v", ev)
	}
	ch, _ := svc.RequestCancellation(ctx, staff, rh.ID, "")
	svc.Decide(ctx, boss, ch.ID, true, "")
	w.RunDue(ctx)

	// Approve and cancel before the worker gets to run: the create finds
	// cancelled leave and does nothing, the delete finds no event.
	r2, _ := svc.Submit(ctx, staff, leave.Input{TypeID: types[0].ID, Start: "2026-11-02", End: "2026-11-02"})
	svc.Decide(ctx, boss, r2.ID, true, "")
	c2, _ := svc.RequestCancellation(ctx, staff, r2.ID, "")
	svc.Decide(ctx, boss, c2.ID, true, "")
	w.RunDue(ctx)
	if len(cal.Events) != 0 {
		t.Fatalf("an event was created for leave cancelled before the worker ran: %+v", cal.Events)
	}
}

// A stand-in's approval is written on the shared calendar as such: "Approved
// by B for A.", so whoever reads the event knows the approver was away.
func TestCalendarBodyNamesTheApproverStoodInFor(t *testing.T) {
	ctx := context.Background()
	d := dbtest.New(t)
	us := users.NewStore(d)
	svc := leave.New(d, us, leave.Config{Location: time.UTC})
	svc.Now = func() time.Time { return time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC) }
	svc.SeedTypes(ctx)
	boss, _ := us.Upsert(ctx, directory.User{ID: "CN=boss", DN: "CN=boss", Username: "boss", Name: "Big Boss", Email: "boss@x.test"})
	deputy, _ := us.Upsert(ctx, directory.User{ID: "CN=dep", DN: "CN=dep", Username: "dep", Name: "Dee Deputy", Email: "dep@x.test"})
	staff, _ := us.Upsert(ctx, directory.User{ID: "CN=s", DN: "CN=s", Username: "s", Name: "Sam Staff", Email: "s@x.test", ManagerDN: "CN=boss"})
	types, _ := svc.Types(ctx, true)

	cal := &calendar.Log{}
	create, remove := calendarHandlers(cal, svc)
	w := outbox.NewWorker(outbox.NewStore(d), map[string]outbox.Handler{
		outbox.KindEmail: emailHandler(mailer.NewFake()), outbox.KindCalendarCreate: create, outbox.KindCalendarDelete: remove,
	})
	svc.OnEvent(notify.New("https://leave.test").Hook)

	if _, err := svc.Delegate(ctx, boss, boss.ID, deputy.ID, "2026-10-05", "2026-10-09", ""); err != nil {
		t.Fatal(err)
	}
	r, err := svc.Submit(ctx, staff, leave.Input{TypeID: types[0].ID, Start: "2026-10-12", End: "2026-10-16"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Decide(ctx, deputy, r.ID, true, ""); err != nil {
		t.Fatal(err)
	}
	w.RunDue(ctx)
	ranges, err := svc.CalendarRanges(ctx, r.ID)
	if err != nil || len(ranges) != 1 {
		t.Fatalf("ranges = %+v %v", ranges, err)
	}
	if want := "Annual leave, Mon 12 Oct – Fri 16 Oct 2026.\nApproved by Dee Deputy for Big Boss."; cal.Events[ranges[0].EventID].Body != want {
		t.Fatalf("event body = %q, want %q", cal.Events[ranges[0].EventID].Body, want)
	}
}
