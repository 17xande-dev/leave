package outbox

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/17xande-dev/leave/internal/dbtest"
)

func TestShutdownWaitsForInFlightJob(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	d := dbtest.New(t)
	if err := Enqueue(ctx, d, KindEmail, 1); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	canceled := make(chan struct{})
	finish := make(chan struct{})
	w := NewWorker(NewStore(d), map[string]Handler{KindEmail: func(jobCtx context.Context, _ []byte) error {
		close(started)
		<-jobCtx.Done()
		close(canceled)
		// A delivery already accepted by the remote service is a success
		// even if cancellation arrives before its handler returns.
		select {
		case <-finish:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}})
	stop := w.Start(ctx, time.Hour)
	defer func() {
		cancel()
		cleanup, end := context.WithTimeout(context.Background(), time.Second)
		defer end()
		_ = stop(cleanup)
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("job did not start")
	}

	// The wait has a bound, but cancellation alone is not a join.
	wait, end := context.WithTimeout(ctx, 25*time.Millisecond)
	defer end()
	if err := stop(wait); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stop with handler still in flight = %v, want deadline exceeded", err)
	}
	select {
	case <-canceled:
	case <-ctx.Done():
		t.Fatal("stop did not cancel the running handler")
	}
	close(finish)
	if err := stop(ctx); err != nil {
		t.Fatal(err)
	}
	var done bool
	if err := d.QueryRowContext(ctx, `SELECT done_at IS NOT NULL FROM outbox`).Scan(&done); err != nil || !done {
		t.Fatalf("shutdown returned before recording success: done=%t, err=%v", done, err)
	}
}

func TestShutdownRefundsOnlyCancellation(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"canceled", context.Canceled, 0},
		{"unrelated failure", errors.New("fixture failure"), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, s, now := dueJob(t)
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			started := make(chan struct{})
			w := NewWorker(s, map[string]Handler{KindEmail: func(ctx context.Context, _ []byte) error {
				close(started)
				<-ctx.Done()
				return tc.err
			}})
			w.now = func() time.Time { return now }
			stop := w.Start(ctx, time.Hour)
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("job did not start")
			}
			if err := stop(ctx); err != nil {
				t.Fatal(err)
			}
			var attempts int
			var unlocked bool
			err := s.db.QueryRowContext(ctx, `SELECT attempts, claim_token IS NULL FROM outbox`).Scan(&attempts, &unlocked)
			if err != nil || attempts != tc.want || !unlocked {
				t.Fatalf("after shutdown: attempts=%d, want %d; lease cleared=%t; err=%v", attempts, tc.want, unlocked, err)
			}
		})
	}
}
