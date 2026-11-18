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
	"errors"
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
	textTmpls  = parseText("submitted", "decided", "withdrawn", "drafts", "escalated", "negative", "granted", "recorded", "revoked", "edited", "halves", "corrected", "delegated", "delegation_revoked")
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
	// DeleteEventIDs are events a create job removes before creating: those
	// of the ranges an admin's correction of approved leave replaced. omitempty
	// keeps every other payload exactly as it was (an old worker ignores it).
	DeleteEventIDs []string `json:"delete_event_ids,omitempty"`
}

type Notifier struct {
	baseURL string
}

func New(baseURL string) *Notifier {
	return &Notifier{baseURL: strings.TrimRight(baseURL, "/")}
}

// recipient is someone to email. StandInFor and StandInUntil are set when they
// are hearing as a stand-in: the approver they act for and the last day.
type recipient struct{ Name, Email, StandInFor, StandInUntil string }

// Hook is the leave.EventHook.
func (n *Notifier) Hook(ctx context.Context, tx *sql.Tx, e leave.Event) error {
	r := e.Request
	switch e.Kind {
	case leave.EventDelegated, leave.EventDelegationRevoked:
		// About a delegation, not a request: e.Request is zero.
		return n.delegationMail(ctx, tx, e)
	case leave.EventSubmitted, leave.EventReassigned:
		to, err := approversAndStandIn(ctx, tx, e)
		if err != nil {
			return err
		}
		if err := n.send(ctx, tx, "submitted", to, r, e.Kind == leave.EventReassigned, "Review the request", "", special(e)); err != nil {
			return err
		}
	case leave.EventEscalated:
		to, err := approversAndStandIn(ctx, tx, e)
		if err != nil {
			return err
		}
		if err := n.send(ctx, tx, "escalated", to, r, false, "Review the request", "", special(e)); err != nil {
			return err
		}
	case leave.EventNegativeRequested:
		// No approver: r.ApproverID is 0, so this is every admin but the requester.
		to, err := approvers(ctx, tx, r)
		if err != nil {
			return err
		}
		if err := n.send(ctx, tx, "negative", to, r, false, "Review the request", "/admin/permissions",
			func(d *msgData) { d.Shortfall = daysOf(e.Shortfall, r.TypeUnit) }); err != nil {
			return err
		}
	case leave.EventNegativeGranted:
		if err := n.send(ctx, tx, "granted", []recipient{{Name: r.UserName, Email: r.UserEmail}}, r, false, "Submit your leave", "",
			func(d *msgData) { d.Note = e.Note }); err != nil {
			return err
		}
	case leave.EventRecorded:
		// Approved from the start: no approver to tell, but the person hears
		// of it, and it goes on the shared calendar like approved leave.
		if err := n.send(ctx, tx, "recorded", []recipient{{Name: r.UserName, Email: r.UserEmail}}, r, false, "View the leave", "",
			func(d *msgData) { d.Shortfall = daysOf(e.Shortfall, r.TypeUnit) }); err != nil {
			return err
		}
		if err := outbox.Enqueue(ctx, tx, outbox.KindCalendarCreate, CalendarJob{RequestID: r.ID}); err != nil {
			return err
		}
	case leave.EventImported:
		// A back-filled log: no email (hundreds of messages about leave
		// payroll already has), but every entry goes on the shared calendar.
		if err := outbox.Enqueue(ctx, tx, outbox.KindCalendarCreate, CalendarJob{RequestID: r.ID}); err != nil {
			return err
		}
	case leave.EventUnimported:
		// Correcting the bulk import: the calendar event goes, nobody is told.
		if err := outbox.Enqueue(ctx, tx, outbox.KindCalendarDelete, CalendarJob{RequestID: r.ID}); err != nil {
			return err
		}
	case leave.EventRevoked:
		if err := n.send(ctx, tx, "revoked", []recipient{{Name: r.UserName, Email: r.UserEmail}}, r, false, "View the leave", "",
			func(d *msgData) { d.Actor = e.Actor.Name }); err != nil {
			return err
		}
		if err := outbox.Enqueue(ctx, tx, outbox.KindCalendarDelete, CalendarJob{RequestID: r.ID}); err != nil {
			return err
		}
	case leave.EventEdited:
		to, err := approversAndStandIn(ctx, tx, e)
		if err != nil {
			return err
		}
		if err := n.send(ctx, tx, "edited", to, r, false, "Review the request", "", special(e), changes(e)); err != nil {
			return err
		}
	case leave.EventHalvesChanged:
		to := []recipient{{Name: r.UserName, Email: r.UserEmail}}
		if r.Status == leave.StatusPending {
			approvers, err := approversAndStandIn(ctx, tx, e)
			if err != nil {
				return err
			}
			to = append(to, approvers...)
		}
		if err := n.send(ctx, tx, "halves", to, r, false, "View the leave", "", func(d *msgData) {
			d.Actor = e.Actor.Name
			if e.Before != nil {
				d.Was = daysOf(e.Before.Days, e.Before.TypeUnit)
			}
		}); err != nil {
			return err
		}
	case leave.EventCorrected:
		// The person always hears; the approver (or every admin but the
		// person, when it sits in the admins' queue) only while it is pending.
		to := []recipient{{Name: r.UserName, Email: r.UserEmail}}
		if r.Status == leave.StatusPending {
			who, err := approversAndStandIn(ctx, tx, e)
			if err != nil {
				return err
			}
			to = append(to, who...)
		}
		linkText := "View the leave"
		if r.Status == leave.StatusPending {
			linkText = "View the request"
		}
		if err := n.send(ctx, tx, "corrected", to, r, false, linkText, "", special(e), changes(e), func(d *msgData) {
			d.Actor = e.Actor.Name
			d.Note = e.Note
		}); err != nil {
			return err
		}
		// Approved leave is on the shared calendar: one job deletes the old
		// events and creates the corrected ones (the type is in the subject,
		// so even a type-only change recreates them).
		if r.Status == leave.StatusApproved {
			if err := outbox.Enqueue(ctx, tx, outbox.KindCalendarCreate, CalendarJob{RequestID: r.ID, DeleteEventIDs: e.StaleEventIDs}); err != nil {
				return err
			}
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
		if err := n.send(ctx, tx, "drafts", []recipient{{Name: r.UserName, Email: r.UserEmail}}, r, false, "Submit your leave", "/"); err != nil {
			return err
		}
	case leave.EventDecided:
		if err := n.send(ctx, tx, "decided", []recipient{{Name: r.UserName, Email: r.UserEmail}}, r, false, "View the request", ""); err != nil {
			return err
		}
		if r.Status == leave.StatusApproved {
			if r.IsCancellation() && e.Cancelled != nil {
				if err := outbox.Enqueue(ctx, tx, outbox.KindCalendarDelete, CalendarJob{RequestID: e.Cancelled.ID}); err != nil {
					return err
				}
			} else if !r.IsCancellation() {
				if err := outbox.Enqueue(ctx, tx, outbox.KindCalendarCreate, CalendarJob{RequestID: r.ID}); err != nil {
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

// approversAndStandIn is approvers plus, when e carries an active stand-in for
// the approver, the stand-in (if they can sign in and are not the requester),
// marked so their copy says who they are approving for.
func approversAndStandIn(ctx context.Context, tx *sql.Tx, e leave.Event) ([]recipient, error) {
	out, err := approvers(ctx, tx, e.Request)
	if err != nil || e.StandIn == nil || e.Request.ApproverID == 0 {
		return out, err
	}
	// Second line of defence, as in leave.emit: whoever created the delegation
	// cannot use it on their own leave, so its details are not sent on.
	if e.StandIn.CreatedByID == e.Request.UserID || e.StandIn.DelegateID == e.Request.UserID {
		return out, nil
	}
	var p recipient
	err = tx.QueryRowContext(ctx, `SELECT name, email FROM users WHERE id = ? AND disabled = 0 AND id <> ?`,
		e.StandIn.DelegateID, e.Request.UserID).Scan(&p.Name, &p.Email)
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	p.StandInFor, p.StandInUntil = e.StandIn.DelegatorName, Dates(e.StandIn.End, e.StandIn.End)
	return append(out, p), nil
}

// delegationData is what the delegation messages are rendered from.
type delegationData struct {
	To string
	// Delegator, Delegate and Actor are names; Actor set the delegation up (or
	// ended it).
	Delegator, Delegate, Actor string
	Dates                      string
	Note                       string
	// ToDelegate: the message is to the stand-in; otherwise to the approver
	// they stand in for. ByDelegator: the approver did it themselves.
	ToDelegate, ByDelegator bool
}

// delegationMail tells the stand-in, and the approver too when someone else
// (an admin) set the delegation up or ended it.
func (n *Notifier) delegationMail(ctx context.Context, tx *sql.Tx, e leave.Event) error {
	d := e.Delegation
	if d == nil {
		return fmt.Errorf("notify: %s event without a delegation", e.Kind)
	}
	tmpl := "delegated"
	if e.Kind == leave.EventDelegationRevoked {
		tmpl = "delegation_revoked"
	}
	type person struct {
		id         int64
		toDelegate bool
	}
	people := []person{{d.DelegateID, true}}
	if e.Actor.ID != d.DelegatorID {
		people = append(people, person{d.DelegatorID, false})
	}
	for _, pe := range people {
		var name, email string
		err := tx.QueryRowContext(ctx, `SELECT name, email FROM users WHERE id = ? AND disabled = 0`, pe.id).Scan(&name, &email)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		if email == "" {
			slog.Warn("notify: recipient has no email address in the directory", "name", name, "delegation", d.ID)
			continue
		}
		data := delegationData{To: firstName(name), Delegator: d.DelegatorName, Delegate: d.DelegateName, Actor: e.Actor.Name,
			Dates: Dates(d.Start, d.End), Note: d.Note, ToDelegate: pe.toDelegate, ByDelegator: e.Actor.ID == d.DelegatorID}
		m, err := n.render(tmpl, data, n.baseURL+"/approvals", "Open your approvals")
		if err != nil {
			return err
		}
		m.To = []string{email}
		if err := outbox.Enqueue(ctx, tx, outbox.KindEmail, m); err != nil {
			return err
		}
	}
	return nil
}

type msgData struct {
	// StandInFor and StandInUntil: the recipient is approving for this person
	// until this date (see recipient).
	StandInFor, StandInUntil string
	Owner                    bool
	To                       string
	Request                  leave.Request
	Dates                    string
	Days                     string
	Reassigned               bool
	// Coverage is each warning sentence and ApprovedBy who has approved so
	// far, for a request needing special permission.
	Coverage   []string
	ApprovedBy string
	// Shortfall is the days below zero asked for ("3 working days").
	Shortfall string
	Note      string
	// Actor is who made the change, where it is not on the request.
	Actor string
	// Was and Now describe an edited request before and after; NoteChanged
	// says the note differs too.
	Was, Now    string
	NoteChanged bool
	// DroppedSpecial: the request needed special permission and no longer does.
	DroppedSpecial bool
}

// changes fills in what an edit changed, so the approver sees the old and the
// new side by side. The note is quoted only when it changed.
func changes(e leave.Event) func(*msgData) {
	return func(d *msgData) {
		if e.Before == nil {
			return
		}
		b := *e.Before
		d.Was = fmt.Sprintf("%s, %s (%s)", b.TypeName, RangesText(b), days(b))
		d.Now = fmt.Sprintf("%s, %s (%s)", e.Request.TypeName, d.Dates, d.Days)
		if b.Note != e.Request.Note {
			d.NoteChanged = true
		}
		d.DroppedSpecial = b.Escalate && !e.Request.Escalate
	}
}

// special fills in what a special-permission message says: why, and who has
// already approved.
func special(e leave.Event) func(*msgData) {
	return func(d *msgData) {
		for _, c := range e.Coverage {
			d.Coverage = append(d.Coverage, c.Sentence())
		}
		var names []string
		for _, st := range e.Steps {
			if st.Decision == "approved" {
				names = append(names, st.ApproverName)
			}
		}
		d.ApprovedBy = strings.Join(names, ", ")
	}
}

// send renders one message per recipient (each is greeted by name) and
// enqueues it. Someone with no email address in the directory is skipped with
// a warning: failing the approval because of a missing mail attribute would
// punish the wrong person.
// path is where the button goes; empty means the request's own page.
// extra adjusts the data each message is rendered from.
func (n *Notifier) send(ctx context.Context, tx *sql.Tx, tmpl string, to []recipient, r leave.Request, reassigned bool, linkText, path string, extra ...func(*msgData)) error {
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
		d := msgData{To: firstName(p.Name), Owner: p.Email == r.UserEmail, Request: r, Dates: RangesText(r),
			Days: days(r), Reassigned: reassigned, StandInFor: p.StandInFor, StandInUntil: p.StandInUntil}
		for _, f := range extra {
			f(&d)
		}
		m, err := n.render(tmpl, d, n.baseURL+path, linkText)
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

func (n *Notifier) render(tmpl string, d any, link, linkText string) (mailer.Message, error) {
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

// RangesText renders the requested stretches, omitting a repeated year when
// they all fall in the same year. A single stretch keeps Dates' exact format.
func RangesText(r leave.Request) string {
	if len(r.Ranges) <= 1 {
		return Dates(r.Start, r.End)
	}
	first, err1 := time.Parse(time.DateOnly, r.Ranges[0].Start)
	last, err2 := time.Parse(time.DateOnly, r.Ranges[len(r.Ranges)-1].End)
	sameYear := err1 == nil && err2 == nil && first.Year() == last.Year()
	parts := make([]string, 0, len(r.Ranges))
	for _, rr := range r.Ranges {
		s, err1 := time.Parse(time.DateOnly, rr.Start)
		e, err2 := time.Parse(time.DateOnly, rr.End)
		if !sameYear || err1 != nil || err2 != nil {
			parts = append(parts, Dates(rr.Start, rr.End))
			continue
		}
		text := e.Format("Mon 2 Jan")
		if !s.Equal(e) {
			startFormat := "Mon 2 Jan"
			if s.Month() == e.Month() {
				startFormat = "Mon 2"
			}
			text = s.Format(startFormat) + " – " + text
		}
		parts = append(parts, text)
	}
	if sameYear {
		parts[len(parts)-1] += last.Format(" 2006")
	}
	return strings.Join(parts, " and ")
}

// days is the request's figure with its unit, and which end is a half day when
// one is ("1.5 working days, last day a half day"): every email, edit diff and
// notification reads the figure through here, so none can leave it out.
func days(r leave.Request) string {
	s := daysOf(r.Days, r.TypeUnit)
	if h := r.HalfNote(); h != "" {
		s += ", " + h
	}
	return s
}

func daysOf(n leave.Days, unit string) string { return n.Label(unit) }
