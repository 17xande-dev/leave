package outbox

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/17xande-dev/leave/internal/dbtest"
	"github.com/17xande-dev/websec/webtest"
)

func TestRetryOnlyDeadJobs(t *testing.T) {
	ctx := context.Background()
	d := dbtest.New(t)
	s := NewStore(d)
	for i := range 2 {
		if err := Enqueue(ctx, d, KindEmail, i); err != nil {
			t.Fatal(err)
		}
	}
	later := time.Now().UTC().Add(time.Hour).Format(time.RFC3339)
	if _, err := d.ExecContext(ctx, `UPDATE outbox SET next_attempt_at = ? WHERE id = 2`, later); err != nil {
		t.Fatal(err)
	}
	NewWorker(s, map[string]Handler{KindEmail: func(context.Context, []byte) error {
		return errors.New("fixture failure")
	}}).RunDue(ctx)

	// Admin retry must not spend another attempt early on a job in backoff
	// or change the schedule of a job that has not failed.
	for id, wantAttempts := range map[int64]int{1: 1, 2: 0} {
		var before, after string
		var attempts int
		if err := d.QueryRowContext(ctx, `SELECT next_attempt_at FROM outbox WHERE id = ?`, id).Scan(&before); err != nil {
			t.Fatal(err)
		}
		if err := s.Retry(ctx, id); err != nil {
			t.Fatal(err)
		}
		if err := d.QueryRowContext(ctx, `SELECT attempts, next_attempt_at FROM outbox WHERE id = ?`, id).Scan(&attempts, &after); err != nil {
			t.Fatal(err)
		}
		if attempts != wantAttempts || after != before {
			t.Errorf("retry changed unresolved job %d: attempts = %d, want %d; schedule changed: %t",
				id, attempts, wantAttempts, after != before)
		}
	}
}

func TestExpiredFinalAttemptDeadLettersWithoutExtraDelivery(t *testing.T) {
	ctx, s, now := dueJob(t)
	if _, err := s.db.ExecContext(ctx, `UPDATE outbox SET attempts = ?`, maxAttempts-1); err != nil {
		t.Fatal(err)
	}
	final := mustClaim(t, ctx, s, now)
	if final.Attempts != maxAttempts {
		t.Fatal("fixture did not claim the final attempt")
	}
	calls := 0
	w := NewWorker(s, map[string]Handler{KindEmail: func(context.Context, []byte) error {
		calls++
		return nil
	}})
	w.now = func() time.Time { return now }
	for range 2 {
		now = now.Add(jobLease)
		w.RunDue(ctx)
	}
	if calls != 0 {
		t.Fatalf("delivered %d extra times after losing the final attempt", calls)
	}
	ps, err := s.Problems(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 1 || ps[0].FailedAt == "" || ps[0].Attempts != maxAttempts || ps[0].LastError != "final attempt lease expired" {
		t.Fatal("expired final attempt did not become a dead letter at the attempt limit")
	}
}

func TestFinalAllowedAttemptCanSucceed(t *testing.T) {
	ctx, s, now := dueJob(t)
	if _, err := s.db.ExecContext(ctx, `UPDATE outbox SET attempts = ?`, maxAttempts-1); err != nil {
		t.Fatal(err)
	}
	calls := 0
	w := NewWorker(s, map[string]Handler{KindEmail: func(context.Context, []byte) error {
		calls++
		return nil
	}})
	w.now = func() time.Time { return now }
	w.RunDue(ctx)
	if calls != 1 {
		t.Fatalf("final allowed attempt delivered %d times, want 1", calls)
	}
	var done bool
	if err := s.db.QueryRowContext(ctx, `SELECT done_at IS NOT NULL FROM outbox`).Scan(&done); err != nil || !done {
		t.Fatalf("final allowed success not recorded: done=%t err=%v", done, err)
	}
}

func TestHandlerPanicCountsAsFailedAttemptAndWorkerContinues(t *testing.T) {
	defer func() {
		if recover() != nil {
			t.Error("handler panic escaped the queue worker")
		}
	}()
	logger, logs := webtest.Logger(t)
	previous := slog.Default()
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(previous) })

	ctx := context.Background()
	d := dbtest.New(t)
	s := NewStore(d)
	const panicMarker = "fixture_panic_marker"
	if err := Enqueue(ctx, d, KindEmail, 1); err != nil {
		t.Fatal(err)
	}
	if err := Enqueue(ctx, d, KindCalendarCreate, 2); err != nil {
		t.Fatal(err)
	}
	followingRan := false
	w := NewWorker(s, map[string]Handler{
		KindEmail: func(context.Context, []byte) error {
			panic(panicMarker)
		},
		KindCalendarCreate: func(context.Context, []byte) error {
			followingRan = true
			return nil
		},
	})
	w.RunDue(ctx)
	if !followingRan {
		t.Fatal("panic prevented the following job from running")
	}
	ps, err := s.Problems(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(ps) != 1 || ps[0].Kind != KindEmail || ps[0].Attempts != 1 || ps[0].LastError != "handler panicked" {
		t.Fatal("panic was not saved as one failed attempt with a fixed reason")
	}
	webtest.NoSecrets(t, logs.String(), "outbox: job failed", panicMarker)
}
