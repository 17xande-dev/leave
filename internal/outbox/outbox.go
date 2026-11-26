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
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"runtime"
	"strings"
	"time"
	"unicode"
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

const (
	jobTimeout = time.Minute
	// Allow a bounded handler and its result write to finish without needing
	// lease renewal. A runner that ignores cancellation can still overlap.
	jobLease = 2 * time.Minute
)

type heldJob struct {
	ID       int64
	Kind     string
	Payload  []byte
	Attempts int
	token    []byte
}

func (s *Store) claim(ctx context.Context, now time.Time) (heldJob, error) {
	var j heldJob
	// Choosing and updating the row under the same SQLite write lock lets
	// separate processes compete without holding a transaction during delivery.
	// Charge the attempt now, because a crash cannot charge it afterwards.
	err := s.db.QueryRowContext(ctx, `UPDATE outbox
		SET lease_until = ?, claim_token = randomblob(16), attempts = min(attempts + 1, ?)
		WHERE id = (
			SELECT id FROM outbox
			WHERE done_at IS NULL AND failed_at IS NULL AND next_attempt_at <= ?
				AND (lease_until IS NULL OR lease_until <= ?)
			ORDER BY id LIMIT 1
		)
		RETURNING id, kind, payload, attempts, claim_token`,
		now.Add(jobLease).UnixNano(), maxAttempts+1, now.UTC().Format(time.RFC3339), now.UnixNano(),
	).Scan(&j.ID, &j.Kind, &j.Payload, &j.Attempts, &j.token)
	return j, err
}

func (s *Store) record(ctx context.Context, j heldJob, runErr error, interrupted bool, now time.Time) (bool, error) {
	stamp := now.UTC().Format(time.RFC3339)
	// Clearing the lease and settling must be one fenced write: a resumed
	// worker must not change the result of a later owner, even after admin retry.
	const held = `, lease_until = NULL, claim_token = NULL
		WHERE id = ? AND claim_token = ? AND done_at IS NULL AND failed_at IS NULL`
	var res sql.Result
	var err error
	switch {
	case interrupted:
		res, err = s.db.ExecContext(ctx, `UPDATE outbox SET attempts = attempts - 1`+held, j.ID, j.token)
	case runErr == nil:
		res, err = s.db.ExecContext(ctx, `UPDATE outbox SET done_at = ?, last_error = ''`+held, stamp, j.ID, j.token)
	case j.Attempts >= maxAttempts:
		res, err = s.db.ExecContext(ctx, `UPDATE outbox SET attempts = ?, last_error = ?, failed_at = ?`+held,
			maxAttempts, describeError(runErr), stamp, j.ID, j.token)
	default:
		next := now.Add(backoff[j.Attempts-1]).UTC().Format(time.RFC3339)
		res, err = s.db.ExecContext(ctx, `UPDATE outbox SET last_error = ?, next_attempt_at = ?`+held,
			describeError(runErr), next, j.ID, j.token)
	}
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// Keep the detail available to admins without letting a remote service supply
// a multiline or unbounded message. The queue logs only the error class.
func describeError(err error) string {
	detail := strings.Map(func(r rune) rune {
		switch {
		case unicode.IsSpace(r):
			return ' '
		case unicode.IsControl(r):
			return -1
		default:
			return r
		}
	}, err.Error())
	runes := []rune(strings.Join(strings.Fields(detail), " "))
	return string(runes[:min(len(runes), 300)])
}

// Problems lists jobs that have failed for good or are failing now, newest
// first, for the admin page.
func (s *Store) Problems(ctx context.Context) ([]Job, error) {
	// The claim charges an attempt before delivery. Keep the admin count and
	// problem list about past attempts, not the handler currently running.
	rows, err := s.db.QueryContext(ctx, `SELECT id, kind, payload, attempts - (claim_token IS NOT NULL), last_error, created_at,
		COALESCE(failed_at, ''), next_attempt_at FROM outbox
		WHERE done_at IS NULL AND (failed_at IS NOT NULL OR attempts > (claim_token IS NOT NULL))
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
			RequestID      int64    `json:"request_id"`
			DeleteEventIDs []string `json:"delete_event_ids"`
		}
		if json.Unmarshal(j.Payload, &c) == nil {
			verb := "Add"
			if j.Kind == KindCalendarDelete {
				verb = "Remove"
			}
			out := fmt.Sprintf("%s request %d on the shared calendar", verb, c.RequestID)
			// A correction's job removes the events it replaces first.
			if n := len(c.DeleteEventIDs); n > 0 {
				out += fmt.Sprintf(" (and removes %d old %s)", n, map[bool]string{true: "event", false: "events"}[n == 1])
			}
			return out
		}
	}
	return j.Kind
}

// Retry puts a dead job back in the queue with a fresh set of attempts.
// It reports whether a job was requeued, leaving active schedules alone.
func (s *Store) Retry(ctx context.Context, id int64) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE outbox SET failed_at = NULL, attempts = 0, next_attempt_at = ?
		WHERE id = ? AND done_at IS NULL AND failed_at IS NOT NULL AND claim_token IS NULL`,
		time.Now().UTC().Format(time.RFC3339), id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// Worker runs due jobs one at a time, preferring the oldest eligible job.
// Backoff and other workers can reorder effects, so handlers cannot rely on FIFO.
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

// Start starts polling and returns a function that cancels the worker and
// waits for it to finish. On a wait error the worker may still be using its DB.
func (w *Worker) Start(ctx context.Context, every time.Duration) func(context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		w.Run(ctx, every)
	}()
	return func(wait context.Context) error {
		cancel()
		select {
		case <-done:
			return nil
		default:
		}
		select {
		case <-done:
			return nil
		case <-wait.Done():
			return wait.Err()
		}
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
	for ctx.Err() == nil {
		j, err := w.store.claim(ctx, w.now())
		if errors.Is(err, sql.ErrNoRows) || (err != nil && ctx.Err() != nil) {
			return
		}
		if err != nil {
			slog.Error("outbox: claim due job", "error_class", fmt.Sprintf("%T", err))
			return
		}
		var runErr error
		if j.Attempts > maxAttempts {
			// A process may have died after claiming the final attempt.
			// Reclaim to settle, not to perform another delivery.
			runErr = errors.New("final attempt lease expired")
		} else if runErr = ctx.Err(); runErr == nil {
			h, ok := w.handlers[j.Kind]
			if !ok {
				runErr = fmt.Errorf("no handler for %q", j.Kind)
			} else {
				jctx, cancel := context.WithTimeout(ctx, jobTimeout)
				runErr = runHandler(jctx, h, j)
				cancel()
			}
		}
		// Shutdown itself is not a delivery failure. Settle with a fresh
		// context so a racing success or unrelated error can still be saved.
		interrupted := ctx.Err() != nil && errors.Is(runErr, ctx.Err())
		recordCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		written, err := w.store.record(recordCtx, j, runErr, interrupted, w.now())
		cancel()
		switch {
		case err != nil:
			slog.Error("outbox: record result", "id", j.ID, "error_class", fmt.Sprintf("%T", err))
			return
		case !written:
			slog.Warn("outbox: lease lost", "id", j.ID, "kind", j.Kind, "attempt", min(j.Attempts, maxAttempts))
		case runErr != nil && !interrupted:
			slog.Warn("outbox: job failed", "id", j.ID, "kind", j.Kind, "attempt", min(j.Attempts, maxAttempts),
				"error_class", fmt.Sprintf("%T", runErr))
		}
	}
}

func runHandler(ctx context.Context, h Handler, j heldJob) (err error) {
	returned := false
	defer func() {
		if !returned {
			// A poison job must use the normal attempt budget, and panic
			// values can contain details that must not reach logs or admins.
			recover()
			var pcs [16]uintptr
			n := runtime.Callers(2, pcs[:]) // skip Callers and this deferred function
			frames := runtime.CallersFrames(pcs[:n])
			for frame, more := frames.Next(); ; frame, more = frames.Next() {
				if frame.Function != "" && !strings.HasPrefix(frame.Function, "runtime.") {
					// Only symbols and source locations, never stack arguments.
					slog.Error("outbox: handler panicked", "id", j.ID, "kind", j.Kind,
						"function", frame.Function, "source", fmt.Sprintf("%s:%d", filepath.Base(frame.File), frame.Line))
					break
				}
				if !more {
					break
				}
			}
			err = errors.New("handler panicked")
		}
	}()
	err = h(ctx, j.Payload)
	returned = true
	return err
}
