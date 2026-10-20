// Package calendar puts approved leave on the shared Microsoft 365 calendar.
package calendar

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Event is one person's leave as it appears on the shared calendar.
type Event struct {
	// Key is sent as Graph's transactionId, which Graph uses to avoid creating
	// a duplicate when a POST is retried -- here, a create that reached Graph
	// but whose event id was never recorded because the process died.
	Key     string
	Subject string
	Body    string
	// Start and End are the first and last day of leave, inclusive, YYYY-MM-DD.
	Start, End string
}

type Client interface {
	Create(ctx context.Context, e Event) (id string, err error)
	Delete(ctx context.Context, id string) error
}

// TokenSource supplies a Graph access token (msauth.ClientCredentials).
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// Graph is a Client for one mailbox's calendar via Microsoft Graph. It needs
// the Calendars.ReadWrite application permission, which an Exchange
// application access policy should restrict to that one mailbox.
type Graph struct {
	Tokens     TokenSource
	Mailbox    string
	CalendarID string // empty: the mailbox's default calendar
	// TimeZone is the Windows zone name Graph expects for all-day events.
	TimeZone string
	// BaseURL and HTTP are for tests; zero values use the real Graph.
	BaseURL string
	HTTP    *http.Client
}

func (g *Graph) base() string {
	if g.BaseURL != "" {
		return strings.TrimRight(g.BaseURL, "/")
	}
	return "https://graph.microsoft.com"
}

func (g *Graph) client() *http.Client {
	if g.HTTP != nil {
		return g.HTTP
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (g *Graph) eventsURL() string {
	u := g.base() + "/v1.0/users/" + url.PathEscape(g.Mailbox)
	if g.CalendarID != "" {
		return u + "/calendars/" + url.PathEscape(g.CalendarID) + "/events"
	}
	return u + "/events"
}

type dateTimeTZ struct {
	DateTime string `json:"dateTime"`
	TimeZone string `json:"timeZone"`
}

type graphEvent struct {
	Subject       string         `json:"subject"`
	Body          map[string]any `json:"body"`
	Start         dateTimeTZ     `json:"start"`
	End           dateTimeTZ     `json:"end"`
	IsAllDay      bool           `json:"isAllDay"`
	ShowAs        string         `json:"showAs"`
	IsReminderOn  bool           `json:"isReminderOn"`
	TransactionID string         `json:"transactionId,omitempty"`
}

func (g *Graph) Create(ctx context.Context, e Event) (string, error) {
	start, err1 := time.Parse(time.DateOnly, e.Start)
	end, err2 := time.Parse(time.DateOnly, e.End)
	if err1 != nil || err2 != nil {
		return "", fmt.Errorf("calendar: bad dates %q..%q", e.Start, e.End)
	}
	tz := g.TimeZone
	if tz == "" {
		tz = "South Africa Standard Time"
	}
	// An all-day event runs from midnight on the first day to midnight after
	// the last: Graph's end is exclusive.
	body, _ := json.Marshal(graphEvent{
		Subject:  e.Subject,
		Body:     map[string]any{"contentType": "text", "content": e.Body},
		Start:    dateTimeTZ{start.Format("2006-01-02T00:00:00"), tz},
		End:      dateTimeTZ{end.AddDate(0, 0, 1).Format("2006-01-02T00:00:00"), tz},
		IsAllDay: true,
		// Free, not out-of-office: this is the team's shared calendar, and
		// marking its owner busy for everyone's leave would block the mailbox.
		ShowAs: "free", IsReminderOn: false,
		// No attendees, deliberately: attendees would be sent invitations.
		TransactionID: e.Key,
	})
	var out struct {
		ID string `json:"id"`
	}
	if err := g.do(ctx, http.MethodPost, g.eventsURL(), body, http.StatusCreated, &out); err != nil {
		return "", err
	}
	if out.ID == "" {
		return "", fmt.Errorf("calendar: Graph returned no event id")
	}
	return out.ID, nil
}

// Delete removes an event. One that is already gone counts as deleted:
// someone may have removed it by hand in Outlook.
func (g *Graph) Delete(ctx context.Context, id string) error {
	u := g.base() + "/v1.0/users/" + url.PathEscape(g.Mailbox) + "/events/" + url.PathEscape(id)
	err := g.do(ctx, http.MethodDelete, u, nil, http.StatusNoContent, nil)
	if se, ok := err.(*statusError); ok && se.code == http.StatusNotFound {
		return nil
	}
	return err
}

type statusError struct {
	code int
	body string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("calendar: Graph status %d: %s", e.code, e.body)
}

func (g *Graph) do(ctx context.Context, method, u string, body []byte, want int, out any) error {
	token, err := g.Tokens.Token(ctx)
	if err != nil {
		return fmt.Errorf("calendar: access token: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := g.client().Do(req)
	if err != nil {
		return fmt.Errorf("calendar: %s: %w", method, err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(res.Body, 64<<10))
	if res.StatusCode != want {
		return &statusError{res.StatusCode, strings.TrimSpace(string(b))}
	}
	if out != nil {
		if err := json.Unmarshal(b, out); err != nil {
			return fmt.Errorf("calendar: decode response: %w", err)
		}
	}
	return nil
}

// Log is a Client for development: it logs what it would do and keeps the
// events in memory.
type Log struct {
	mu     sync.Mutex
	n      int
	Events map[string]Event
}

func (l *Log) Create(_ context.Context, e Event) (string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.Events == nil {
		l.Events = map[string]Event{}
	}
	l.n++
	id := fmt.Sprintf("dev-%d", l.n)
	l.Events[id] = e
	slog.Info("calendar (dev): create", "id", id, "subject", e.Subject, "start", e.Start, "end", e.End)
	return id, nil
}

func (l *Log) Delete(_ context.Context, id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.Events, id)
	slog.Info("calendar (dev): delete", "id", id)
	return nil
}
