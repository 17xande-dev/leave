// Package outbox holds side effects -- email, calendar events -- that must
// happen because of a committed change, and runs them afterwards with retries.
//
// A job is written in the same transaction as the change that causes it, so
// the two commit together or not at all. The network call happens later, on a
// worker, never inside a request or a transaction: a slow Graph API must not
// hold the database's write lock, and Graph being down must not stop anyone
// approving leave. The price is at-least-once delivery -- a crash between a
// send and marking the job done sends twice -- which for a notification email
// is the right way round.
package outbox

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

const (
	KindEmail          = "email"
	KindCalendarCreate = "calendar_create"
	KindCalendarDelete = "calendar_delete"
)

type execer interface {
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
}

// Enqueue writes a job. Pass the transaction making the change that causes it.
func Enqueue(ctx context.Context, tx execer, kind string, payload any) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("outbox: encode %s: %w", kind, err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err = tx.ExecContext(ctx, `INSERT INTO outbox (kind, payload, next_attempt_at, created_at)
		VALUES (?, ?, ?, ?)`, kind, string(b), now, now)
	return err
}

type Job struct {
	ID          int64
	Kind        string
	Payload     []byte
	Attempts    int
	LastError   string
	CreatedAt   string
	FailedAt    string
	NextAttempt string
}

// Handler runs one job. An error means try again later.
type Handler func(ctx context.Context, payload []byte) error

type Store struct{ db *sql.DB }

func NewStore(db *sql.DB) *Store { return &Store{db: db} }

// backoff is the wait after each failed attempt. Twelve attempts span a little
// over a day, which rides out a Graph outage or an expired client secret long
// enough for someone to notice on the admin page.
var backoff = []time.Duration{
	time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 30 * time.Minute,
	time.Hour, 2 * time.Hour, 3 * time.Hour, 4 * time.Hour, 6 * time.Hour,
}

// maxAttempts after which a job is marked failed and waits for a person.
var maxAttempts = len(backoff) + 1

func (s *Store) due(ctx context.Context, now time.Time, limit int) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, kind, payload, attempts FROM outbox
		WHERE done_at IS NULL AND failed_at IS NULL AND next_attempt_at <= ?
		ORDER BY id LIMIT ?`, now.UTC().Format(time.RFC3339), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		var j Job
		var p string
		if err := rows.Scan(&j.ID, &j.Kind, &p, &j.Attempts); err != nil {
			return nil, err
		}
		j.Payload = []byte(p)
		out = append(out, j)
	}
	return out, rows.Err()
}

func (s *Store) record(ctx context.Context, j Job, runErr error, now time.Time) error {
	stamp := now.UTC().Format(time.RFC3339)
	if runErr == nil {
		_, err := s.db.ExecContext(ctx, `UPDATE outbox SET done_at = ?, attempts = attempts + 1, last_error = ''
			WHERE id = ?`, stamp, j.ID)
		return err
	}
	attempts := j.Attempts + 1
	if attempts >= maxAttempts {
		_, err := s.db.ExecContext(ctx, `UPDATE outbox SET attempts = ?, last_error = ?, failed_at = ?
			WHERE id = ?`, attempts, runErr.Error(), stamp, j.ID)
		return err
	}
	next := now.Add(backoff[attempts-1]).UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `UPDATE outbox SET attempts = ?, last_error = ?, next_attempt_at = ?
		WHERE id = ?`, attempts, runErr.Error(), next, j.ID)
	return err
}

// Problems lists jobs that have failed for good or are failing now, newest
// first, for the admin page.
func (s *Store) Problems(ctx context.Context) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, kind, payload, attempts, last_error, created_at,
		COALESCE(failed_at, ''), next_attempt_at FROM outbox
		WHERE done_at IS NULL AND (failed_at IS NOT NULL OR attempts > 0)
		ORDER BY id DESC LIMIT 200`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		var j Job
		var p string
		if err := rows.Scan(&j.ID, &j.Kind, &p, &j.Attempts, &j.LastError, &j.CreatedAt, &j.FailedAt, &j.NextAttempt); err != nil {
			return nil, err
		}
		j.Payload = []byte(p)
		out = append(out, j)
	}
	return out, rows.Err()
}

// Summary describes a job for a person: who an email was for and what it was
// about, or which request a calendar job concerns. Never the whole payload --
// it is someone's leave.
func Summary(j Job) string {
	switch j.Kind {
	case KindEmail:
		var m struct {
			To      []string
			Subject string
		}
		if json.Unmarshal(j.Payload, &m) == nil {
			return fmt.Sprintf("Email to %s: %s", strings.Join(m.To, ", "), m.Subject)
		}
	case KindCalendarCreate, KindCalendarDelete:
		var c struct {
			RequestID int64 `json:"request_id"`
		}
		if json.Unmarshal(j.Payload, &c) == nil {
			verb := "Add"
			if j.Kind == KindCalendarDelete {
				verb = "Remove"
			}
			return fmt.Sprintf("%s request %d on the shared calendar", verb, c.RequestID)
		}
	}
	return j.Kind
}

// Retry puts a failed job back in the queue with a fresh set of attempts.
func (s *Store) Retry(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE outbox SET failed_at = NULL, attempts = 0, next_attempt_at = ?
		WHERE id = ? AND done_at IS NULL`, time.Now().UTC().Format(time.RFC3339), id)
	return err
}

// Worker runs due jobs one at a time, in the order they were written. One at a
// time is deliberate: a calendar delete must not overtake the create it undoes.
type Worker struct {
	store    *Store
	handlers map[string]Handler
	kick     chan struct{}
	now      func() time.Time
}

func NewWorker(s *Store, handlers map[string]Handler) *Worker {
	return &Worker{store: s, handlers: handlers, kick: make(chan struct{}, 1), now: time.Now}
}

// Kick wakes the worker now rather than at the next poll, so an approval's
// email goes out in a second instead of up to a poll interval later.
func (w *Worker) Kick() {
	select {
	case w.kick <- struct{}{}:
	default:
	}
}

// Run polls until ctx ends.
func (w *Worker) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		w.RunDue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-w.kick:
		}
	}
}

// RunDue runs every job that is due now. Exported for tests.
func (w *Worker) RunDue(ctx context.Context) {
	for {
		jobs, err := w.store.due(ctx, w.now(), 20)
		if err != nil {
			slog.Error("outbox: list due jobs", "err", err)
			return
		}
		if len(jobs) == 0 {
			return
		}
		for _, j := range jobs {
			if ctx.Err() != nil {
				return
			}
			h, ok := w.handlers[j.Kind]
			var runErr error
			if !ok {
				runErr = fmt.Errorf("no handler for %q", j.Kind)
			} else {
				jctx, cancel := context.WithTimeout(ctx, time.Minute)
				runErr = h(jctx, j.Payload)
				cancel()
			}
			if runErr != nil {
				slog.Warn("outbox: job failed", "id", j.ID, "kind", j.Kind, "attempt", j.Attempts+1, "err", runErr)
			}
			if err := w.store.record(ctx, j, runErr, w.now()); err != nil {
				slog.Error("outbox: record result", "id", j.ID, "err", err)
				return
			}
		}
	}
}
