package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/17xande-dev/mailer"
	"github.com/17xande-dev/mailer/msauth"

	"github.com/17xande-dev/leave/internal/calendar"
	"github.com/17xande-dev/leave/internal/config"
	"github.com/17xande-dev/leave/internal/leave"
	"github.com/17xande-dev/leave/internal/notify"
	"github.com/17xande-dev/leave/internal/outbox"
)

// newMailer picks the mail transport: Graph in production, a local SMTP sink
// (mailpit) in development, or Discard -- which logs each message -- when
// neither is configured. Mail being unconfigured does not stop the app: every
// email is an outbox row first, so nothing is lost, only unsent.
func newMailer(cfg config.Config, tokens mailer.TokenSource) (mailer.Sender, error) {
	switch {
	case cfg.Graph.Configured():
		slog.Info("email: Graph", "from", cfg.Graph.From)
		return mailer.NewGraphSender(mailer.GraphConfig{TokenSource: tokens, From: cfg.Graph.From})
	case cfg.DevSMTP.Host != "":
		slog.Warn("email: development SMTP sink, no TLS or auth", "host", cfg.DevSMTP.Host, "port", cfg.DevSMTP.Port)
		return mailer.NewSMTPSender(mailer.SMTPConfig{Host: cfg.DevSMTP.Host, Port: cfg.DevSMTP.Port,
			From: cfg.DevSMTP.From, TLS: mailer.TLSNone})
	default:
		slog.Warn("email: NOT CONFIGURED; messages are logged and dropped")
		return mailer.Discard{Log: slog.Default()}, nil
	}
}

// graphTokens is the one client-credentials token source that mail and the
// calendar share, so both ride on one cached token.
func graphTokens(cfg config.Config) (mailer.TokenSource, error) {
	if !cfg.Graph.Configured() {
		return nil, nil
	}
	return msauth.New(cfg.Graph.TenantID, cfg.Graph.ClientID, cfg.Graph.ClientSecret,
		msauth.WithScope(msauth.ScopeGraph))
}

func emailHandler(sender mailer.Sender) outbox.Handler {
	return func(ctx context.Context, payload []byte) error {
		var m mailer.Message
		if err := json.Unmarshal(payload, &m); err != nil {
			return fmt.Errorf("decode email job: %w", err)
		}
		return sender.Send(ctx, m)
	}
}

// calendarHandlers run the shared-calendar jobs. Both re-read the request when
// they run rather than trusting the payload: jobs retry, and by the time a
// create runs its leave may already be cancelled.
func calendarHandlers(cal calendar.Client, svc *leave.Service) (create, remove outbox.Handler) {
	load := func(ctx context.Context, payload []byte) (leave.Request, error) {
		var j notify.CalendarJob
		if err := json.Unmarshal(payload, &j); err != nil {
			return leave.Request{}, fmt.Errorf("decode calendar job: %w", err)
		}
		return svc.Get(ctx, j.RequestID)
	}
	create = func(ctx context.Context, payload []byte) error {
		r, err := load(ctx, payload)
		if err != nil {
			return err
		}
		// Cancelled before the create ran (Graph was down), or already done.
		if r.Status != leave.StatusApproved || r.CalendarEventID != "" {
			return nil
		}
		approved := "Approved automatically."
		if r.DecidedBy != "" {
			approved = "Approved by " + r.DecidedBy + "."
			if r.IsRecorded() {
				approved = "Recorded by " + r.DecidedBy + "."
			}
		}
		body := fmt.Sprintf("%s, %s.\n%s", r.TypeName, notify.Dates(r.Start, r.End), approved)
		if r.Department != "" {
			body += "\nDepartment: " + r.Department
		}
		id, err := cal.Create(ctx, calendar.Event{
			Key:     fmt.Sprintf("leave-request-%d", r.ID),
			Subject: r.UserName + " — " + r.TypeName,
			Body:    body, Start: r.Start, End: r.End,
		})
		if err != nil {
			return err
		}
		return svc.SetCalendarEventID(ctx, r.ID, id)
	}
	remove = func(ctx context.Context, payload []byte) error {
		r, err := load(ctx, payload)
		if err != nil {
			return err
		}
		if r.CalendarEventID == "" {
			return nil // never made it onto the calendar
		}
		if err := cal.Delete(ctx, r.CalendarEventID); err != nil {
			return err
		}
		return svc.SetCalendarEventID(ctx, r.ID, "")
	}
	return create, remove
}

func newCalendar(cfg config.Config, tokens mailer.TokenSource) calendar.Client {
	if !cfg.Graph.Configured() {
		slog.Warn("calendar: Graph not configured; events are logged, not created")
		return &calendar.Log{}
	}
	return &calendar.Graph{Tokens: tokens, Mailbox: cfg.Graph.CalendarMailbox, CalendarID: cfg.Graph.CalendarID}
}
