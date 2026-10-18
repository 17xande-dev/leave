package main

import (
	"context"
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
	ev, ok := cal.Events[r.CalendarEventID]
	if !ok || ev.Subject != "Sam Staff — Annual leave" || ev.Start != "2026-10-12" || ev.End != "2026-10-16" {
		t.Fatalf("calendar = %+v, request event id %q", cal.Events, r.CalendarEventID)
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
