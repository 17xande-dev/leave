// Package outbox registers Leave's notification and calendar jobs.
package outbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/17xande-dev/outbox"
	"github.com/17xande-dev/outbox/sqlitestore"
)

const (
	KindEmail          = "email"
	KindCalendarCreate = "calendar_create"
	KindCalendarDelete = "calendar_delete"
)

// Handler delivers one Leave payload; an error means retry later.
type Handler func(ctx context.Context, payload []byte) error

type Queue = outbox.Queue[*sql.Tx]

// New registers the application's handlers on the migrated database.
func New(d *sql.DB, handlers map[string]Handler, opts outbox.Options) (*Queue, error) {
	// A day-long Graph outage should not need immediate intervention.
	backoff := outbox.Table(
		time.Minute, 2*time.Minute, 5*time.Minute, 10*time.Minute, 20*time.Minute, 30*time.Minute,
		time.Hour, 2*time.Hour, 3*time.Hour, 4*time.Hour, 6*time.Hour,
	)
	kinds := make(map[string]outbox.Kind[*sql.Tx], len(handlers))
	for name, handler := range handlers {
		kinds[name] = outbox.Kind[*sql.Tx]{
			MaxAttempts: 12,
			Backoff:     backoff,
			Run: func(ctx context.Context, job outbox.Job) error {
				return handler(ctx, job.Payload)
			},
		}
	}
	if opts.Poll == 0 {
		opts.Poll = 30 * time.Second
	}
	return outbox.New(sqlitestore.New(d), kinds, opts)
}

// NewEnqueuer is for commands that commit jobs but never run a worker.
func NewEnqueuer(d *sql.DB) (*Queue, error) {
	disabled := func(context.Context, []byte) error {
		return errors.New("outbox: delivery is disabled in this command")
	}
	return New(d, map[string]Handler{
		KindEmail:          disabled,
		KindCalendarCreate: disabled,
		KindCalendarDelete: disabled,
	}, outbox.Options{})
}

// Enqueue preserves Leave's JSON payload format for both handlers and rollback.
func Enqueue(ctx context.Context, q *Queue, tx *sql.Tx, kind, subject string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("outbox: encode %s: %w", kind, err)
	}
	_, _, err = q.Enqueue(ctx, tx, outbox.Message{Kind: kind, Subject: subject, Payload: b})
	return err
}
