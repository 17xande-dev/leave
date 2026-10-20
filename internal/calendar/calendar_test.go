package calendar

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/17xande-dev/mailer/msauth"
)

type staticToken string

func (s staticToken) Token(context.Context) (string, error) { return string(s), nil }

// The request shape is the part a misread of Graph's docs gets wrong: an
// exclusive end date, all-day, no attendees, and the idempotency key.
func TestCreateSendsAllDayEvent(t *testing.T) {
	var got map[string]any
	var path, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, auth = r.URL.Path, r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &got)
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":"AAMk123"}`))
	}))
	defer srv.Close()

	g := &Graph{Tokens: staticToken("tok"), Mailbox: "leave@example.org", BaseURL: srv.URL}
	id, err := g.Create(context.Background(), Event{Key: "leave-request-7", Subject: "Sam — Annual leave",
		Start: "2026-10-12", End: "2026-10-16"})
	if err != nil || id != "AAMk123" {
		t.Fatalf("create = %q, %v", id, err)
	}
	if path != "/v1.0/users/leave@example.org/events" || auth != "Bearer tok" {
		t.Fatalf("path %q auth %q", path, auth)
	}
	end := got["end"].(map[string]any)["dateTime"]
	if end != "2026-10-17T00:00:00" || got["isAllDay"] != true || got["transactionId"] != "leave-request-7" {
		t.Fatalf("body = %v", got)
	}
	if _, ok := got["attendees"]; ok {
		t.Fatal("event has attendees: they would be sent invitations")
	}
}

func TestCreateInNamedCalendar(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(`{"id":"x"}`))
	}))
	defer srv.Close()
	g := &Graph{Tokens: staticToken("t"), Mailbox: "m@x", CalendarID: "CAL1", BaseURL: srv.URL}
	g.Create(context.Background(), Event{Start: "2026-10-12", End: "2026-10-12"})
	if path != "/v1.0/users/m@x/calendars/CAL1/events" {
		t.Fatalf("path %q", path)
	}
}

// Someone may delete the event by hand in Outlook; the cancellation must still
// complete rather than retry for a day.
func TestDeleteTreatsMissingAsDone(t *testing.T) {
	status := http.StatusNotFound
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/v1.0/users/m@x/events/AAMk123" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(status)
	}))
	defer srv.Close()
	g := &Graph{Tokens: staticToken("t"), Mailbox: "m@x", BaseURL: srv.URL}
	if err := g.Delete(context.Background(), "AAMk123"); err != nil {
		t.Fatalf("404: %v", err)
	}
	status = http.StatusForbidden
	if err := g.Delete(context.Background(), "AAMk123"); err == nil {
		t.Fatal("403 treated as success")
	}
}

// TestLiveGraph creates and deletes one event on the real shared calendar.
// Opt in with GRAPH_LIVE=1 after `set -a; source .env; set +a`.
func TestLiveGraph(t *testing.T) {
	if os.Getenv("GRAPH_LIVE") != "1" {
		t.Skip("set GRAPH_LIVE=1 to create and delete a real calendar event")
	}
	tokens, err := msauth.New(os.Getenv("GRAPH_TENANT_ID"), os.Getenv("GRAPH_CLIENT_ID"),
		os.Getenv("GRAPH_CLIENT_SECRET"), msauth.WithScope(msauth.ScopeGraph))
	if err != nil {
		t.Fatal(err)
	}
	g := &Graph{Tokens: tokens, Mailbox: os.Getenv("LEAVE_CALENDAR_MAILBOX"), CalendarID: os.Getenv("LEAVE_CALENDAR_ID")}
	id, err := g.Create(context.Background(), Event{Subject: "Leave app test — safe to delete",
		Start: "2030-01-07", End: "2030-01-08", Body: "Created by TestLiveGraph."})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("created %s", id)
	if err := g.Delete(context.Background(), id); err != nil {
		t.Fatal(err)
	}
}
