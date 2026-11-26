package outbox

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

func TestFailureBackoffScheduleAndAttemptLimit(t *testing.T) {
	ctx, s, now := dueJob(t)
	now = now.UTC().Truncate(time.Second).Add(time.Second)
	calls := 0
	w := NewWorker(s, map[string]Handler{KindEmail: func(context.Context, []byte) error {
		calls++
		return errors.New("fixture failure")
	}})
	w.now = func() time.Time { return now }
	delays := []time.Duration{
		time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 30 * time.Minute,
		time.Hour, 2 * time.Hour, 3 * time.Hour, 4 * time.Hour, 6 * time.Hour,
	}
	for attempt := 1; attempt <= 12; attempt++ {
		w.RunDue(ctx)
		var attempts int
		var next string
		var failed sql.NullString
		if err := s.db.QueryRowContext(ctx, `SELECT attempts, next_attempt_at, failed_at FROM outbox`).
			Scan(&attempts, &next, &failed); err != nil {
			t.Fatal(err)
		}
		if calls != attempt || attempts != attempt {
			t.Fatalf("attempt %d: got %d deliveries and %d charged attempts", attempt, calls, attempts)
		}
		if attempt == 12 {
			if !failed.Valid || failed.String != now.Format(time.RFC3339) || next != now.Format(time.RFC3339) {
				t.Fatalf("final failure: failed_at = %v, next_attempt_at = %s; want dead letter without rescheduling", failed, next)
			}
			break
		}
		want := now.Add(delays[attempt-1])
		if failed.Valid || next != want.Format(time.RFC3339) {
			t.Fatalf("attempt %d: failed_at = %v, next_attempt_at = %s; want pending at %s", attempt, failed, next, want.Format(time.RFC3339))
		}
		now = want.Add(-time.Second)
		w.RunDue(ctx)
		if calls != attempt {
			t.Fatalf("attempt %d ran before its backoff expired", attempt+1)
		}
		now = want
	}
	now = now.Add(24 * time.Hour)
	w.RunDue(ctx)
	if calls != 12 {
		t.Fatalf("dead letter delivered again: %d total attempts, want 12", calls)
	}
}
