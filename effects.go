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
		if r.Status != leave.StatusApproved {
			return nil
		}
		ranges, err := svc.CalendarRanges(ctx, r.ID)
		if err != nil {
			return err
		}
		approved := "Approved automatically."
		if r.DecidedBy != "" {
			approved = "Approved by " + r.DecidedBy + "."
			if r.IsRecorded() {
				approved = "Recorded by " + r.DecidedBy + "."
			}
		}
		for i, rr := range ranges {
			if rr.EventID != "" {
				continue
			}
			key := fmt.Sprintf("leave-request-%d", r.ID)
			if i > 0 {
				key += "-" + rr.Start
			}
			dates := notify.Dates(r.Start, r.End)
			if len(ranges) > 1 {
				dates = notify.Dates(rr.Start, rr.End)
			}
			eventBody := fmt.Sprintf("%s, %s.\n%s", r.TypeName, dates, approved)
			if r.Department != "" {
				eventBody += "\nDepartment: " + r.Department
			}
			if len(ranges) > 1 {
				eventBody += fmt.Sprintf("\nPart %d of %d.", i+1, len(ranges))
			}
			id, err := cal.Create(ctx, calendar.Event{
				Key: key, Subject: r.UserName + " — " + r.TypeName,
				Body: eventBody, Start: rr.Start, End: rr.End,
			})
			if err != nil {
				return err
			}
			if err := svc.SetRangeEventID(ctx, r.ID, rr.Start, id); err != nil {
				return err
			}
		}
		return nil
	}
	remove = func(ctx context.Context, payload []byte) error {
		r, err := load(ctx, payload)
		if err != nil {
			return err
		}
		ranges, err := svc.CalendarRanges(ctx, r.ID)
		if err != nil {
			return err
		}
		for _, rr := range ranges {
			if rr.LegacyEventID != "" {
				if err := cal.Delete(ctx, rr.LegacyEventID); err != nil {
					return err
				}
			}
			if rr.EventID == "" {
				continue
			}
			if err := cal.Delete(ctx, rr.EventID); err != nil {
				return err
			}
			if err := svc.SetRangeEventID(ctx, r.ID, rr.Start, ""); err != nil {
				return err
			}
		}
		return nil
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
