// Package notify turns leave events into outbox jobs: the emails each change
// sends, and the shared-calendar create or delete an approval implies.
//
// It runs inside the transaction making the change (see leave.EventHook), so
// messages are rendered then and there. That freezes what they say to the
// moment of the change -- the email about a submission still describes the
// submission even if the request is withdrawn before the worker sends it.
package notify

import (
	"bytes"
	"context"
	"database/sql"
	"embed"
	"fmt"
	htmltemplate "html/template"
	"log/slog"
	"strings"
	"text/template"
	"time"

	"github.com/17xande-dev/mailer"

	"github.com/17xande-dev/leave/internal/leave"
	"github.com/17xande-dev/leave/internal/outbox"
)

//go:embed templates
var templateFS embed.FS

var (
	// Each .txt file defines "subject" and "body", so each is parsed into a
	// set of its own: in one shared set the last file parsed would win.
	textTmpls  = parseText("submitted", "decided", "withdrawn", "drafts")
	htmlLayout = htmltemplate.Must(htmltemplate.ParseFS(templateFS, "templates/layout.html"))
)

func parseText(names ...string) map[string]*template.Template {
	out := map[string]*template.Template{}
	for _, n := range names {
		out[n] = template.Must(template.New(n).Funcs(template.FuncMap{"lower": strings.ToLower}).
			Option("missingkey=error").ParseFS(templateFS, "templates/"+n+".txt"))
	}
	return out
}

// CalendarJob is the payload of a calendar create or delete.
type CalendarJob struct {
	RequestID int64 `json:"request_id"`
}

type Notifier struct {
	baseURL string
}

func New(baseURL string) *Notifier {
	return &Notifier{baseURL: strings.TrimRight(baseURL, "/")}
}

type recipient struct{ Name, Email string }

// Hook is the leave.EventHook.
func (n *Notifier) Hook(ctx context.Context, tx *sql.Tx, e leave.Event) error {
	r := e.Request
	switch e.Kind {
	case leave.EventSubmitted, leave.EventReassigned:
		to, err := approvers(ctx, tx, r)
		if err != nil {
			return err
		}
		if err := n.send(ctx, tx, "submitted", to, r, e.Kind == leave.EventReassigned, "Review the request", ""); err != nil {
			return err
		}
	case leave.EventWithdrawn:
		to, err := approvers(ctx, tx, r)
		if err != nil {
			return err
		}
		if err := n.send(ctx, tx, "withdrawn", to, r, false, "View the request", ""); err != nil {
			return err
		}
	case leave.EventDraftsCreated:
		// To the home page, which lists every draft, rather than the first.
		if err := n.send(ctx, tx, "drafts", []recipient{{r.UserName, r.UserEmail}}, r, false, "Submit your leave", "/"); err != nil {
			return err
		}
	case leave.EventDecided:
		if err := n.send(ctx, tx, "decided", []recipient{{r.UserName, r.UserEmail}}, r, false, "View the request", ""); err != nil {
			return err
		}
		if r.Status == leave.StatusApproved {
			if r.IsCancellation() && e.Cancelled != nil {
				if err := outbox.Enqueue(ctx, tx, outbox.KindCalendarDelete, CalendarJob{e.Cancelled.ID}); err != nil {
					return err
				}
			} else if !r.IsCancellation() {
				if err := outbox.Enqueue(ctx, tx, outbox.KindCalendarCreate, CalendarJob{r.ID}); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// approvers is who acts on r: its approver, or every admin but the requester
// when it sits in the admins' queue.
func approvers(ctx context.Context, tx *sql.Tx, r leave.Request) ([]recipient, error) {
	q := `SELECT name, email FROM users WHERE id = ? AND disabled = 0`
	args := []any{r.ApproverID}
	if r.ApproverID == 0 {
		q = `SELECT name, email FROM users WHERE is_admin = 1 AND disabled = 0 AND id <> ?`
		args = []any{r.UserID}
	}
	rows, err := tx.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []recipient
	for rows.Next() {
		var p recipient
		if err := rows.Scan(&p.Name, &p.Email); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

type msgData struct {
	To         string
	Request    leave.Request
	Dates      string
	Days       string
	Reassigned bool
}

// send renders one message per recipient (each is greeted by name) and
// enqueues it. Someone with no email address in the directory is skipped with
// a warning: failing the approval because of a missing mail attribute would
// punish the wrong person.
// path is where the button goes; empty means the request's own page.
func (n *Notifier) send(ctx context.Context, tx *sql.Tx, tmpl string, to []recipient, r leave.Request, reassigned bool, linkText, path string) error {
	if path == "" {
		path = fmt.Sprintf("/leave/%d", r.ID)
	}
	if len(to) == 0 {
		slog.Warn("notify: nobody to tell", "template", tmpl, "request", r.ID)
	}
	for _, p := range to {
		if p.Email == "" {
			slog.Warn("notify: recipient has no email address in the directory", "name", p.Name, "request", r.ID)
			continue
		}
		m, err := n.render(tmpl, msgData{To: firstName(p.Name), Request: r, Dates: Dates(r.Start, r.End),
			Days: days(r), Reassigned: reassigned}, n.baseURL+path, linkText)
		if err != nil {
			return err
		}
		m.To = []string{p.Email}
		if err := outbox.Enqueue(ctx, tx, outbox.KindEmail, m); err != nil {
			return err
		}
	}
	return nil
}

func (n *Notifier) render(tmpl string, d msgData, link, linkText string) (mailer.Message, error) {
	own, ok := textTmpls[tmpl]
	if !ok {
		return mailer.Message{}, fmt.Errorf("notify: no template %s", tmpl)
	}
	var subj, body bytes.Buffer
	if err := own.ExecuteTemplate(&subj, "subject", d); err != nil {
		return mailer.Message{}, err
	}
	if err := own.ExecuteTemplate(&body, "body", d); err != nil {
		return mailer.Message{}, err
	}
	text := strings.TrimSpace(body.String())
	var html bytes.Buffer
	if err := htmlLayout.Execute(&html, map[string]any{
		"Paragraphs": strings.Split(text, "\n\n"), "Link": link, "LinkText": linkText,
	}); err != nil {
		return mailer.Message{}, err
	}
	return mailer.Message{
		Subject: strings.Join(strings.Fields(subj.String()), " "),
		Text:    text + "\n\n" + linkText + ": " + link + "\n",
		HTML:    html.String(),
	}, nil
}

func firstName(name string) string {
	if f := strings.Fields(name); len(f) > 0 {
		return f[0]
	}
	return "there"
}

// Dates renders a request's dates for a person: "Mon 12 Oct" or
// "Mon 12 – Fri 16 Oct 2026".
func Dates(start, end string) string {
	s, err1 := time.Parse(time.DateOnly, start)
	e, err2 := time.Parse(time.DateOnly, end)
	if err1 != nil || err2 != nil {
		return start + " – " + end
	}
	if s.Equal(e) {
		return s.Format("Mon 2 Jan 2006")
	}
	if s.Year() == e.Year() {
		return s.Format("Mon 2 Jan") + " – " + e.Format("Mon 2 Jan 2006")
	}
	return s.Format("Mon 2 Jan 2006") + " – " + e.Format("Mon 2 Jan 2006")
}

func days(r leave.Request) string {
	switch r.TypeUnit {
	case leave.UnitWeekend:
		if r.Days == 1 {
			return "1 weekend"
		}
		return fmt.Sprintf("%d weekends", r.Days)
	case leave.UnitCalendar:
		if r.Days == 1 {
			return "1 day"
		}
		return fmt.Sprintf("%d days", r.Days)
	}
	if r.Days == 1 {
		return "1 working day"
	}
	return fmt.Sprintf("%d working days", r.Days)
}
