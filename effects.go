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
		var j notify.CalendarJob
		if err := json.Unmarshal(payload, &j); err != nil {
			return fmt.Errorf("decode calendar job: %w", err)
		}
		// An admin's correction of approved leave replaced its ranges, so the
		// events they held are listed here. They are deleted first, before the
		// status check below: a correction followed quickly by a cancellation
		// must still remove them, because the cancellation's delete job finds
		// the ranges' ids already blank.
		//
		// Retry safety: a retry deletes again (Graph 404 counts as done) and
		// creates only the ranges still without an id. If a second correction
		// lands before this job runs, it captured no ids (the first blanked
		// them) and this job creates events for the newest ranges, so the
		// second job finds every range done. If this job ran in between, the
		// second correction captured and deletes those events and recreates.
		for _, id := range j.DeleteEventIDs {
			if err := cal.Delete(ctx, id); err != nil {
				return err
			}
		}
		// The correction count is read before the request and its ranges, so
		// RecordRangeEvent can tell if a correction landed after this point.
		revision, err := svc.CorrectionCount(ctx, j.RequestID)
		if err != nil {
			return err
		}
		r, err := svc.Get(ctx, j.RequestID)
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
			approved = "Approved by " + r.DecidedByFor() + "."
			if r.IsRecorded() {
				approved = "Recorded by " + r.DecidedBy + "."
			}
		}
		// Each correction of approved leave deletes its events and makes new
		// ones, so the key carries the correction count: otherwise Graph's
		// transactionId dedupe could hand back the event just deleted.
		for i, rr := range ranges {
			if rr.EventID != "" {
				continue
			}
			key := fmt.Sprintf("leave-request-%d", r.ID)
			if revision > 0 {
				key += fmt.Sprintf("-c%d", revision)
			}
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
			ok, err := svc.RecordRangeEvent(ctx, r.ID, rr.Start, rr.End, id, revision)
			if err != nil {
				return err
			}
			if !ok {
				// A correction committed during the Graph call and replaced
				// the ranges: this event is stale. Remove it; the
				// correction's own job creates the right one.
				if err := cal.Delete(ctx, id); err != nil {
					return err
				}
				return nil
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
