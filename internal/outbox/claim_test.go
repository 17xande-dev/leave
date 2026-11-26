package outbox

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/17xande-dev/leave/internal/dbtest"
)

func TestConcurrentWorkersRunOneDueJobOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	d := dbtest.New(t)
	d.SetMaxOpenConns(8)
	d.SetMaxIdleConns(8)
	var conns []*sql.Conn
	for range 8 {
		c, err := d.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
		t.Cleanup(func() { c.Close() })
	}
	for _, c := range conns {
		c.Close()
	}
	if err := Enqueue(ctx, d, KindEmail, 1); err != nil {
		t.Fatal(err)
	}

	// A fast completion could hide a missing claim, so hold the first delivery
	// open until the other nine workers finish their pass.
	var calls atomic.Int32
	start, release := make(chan struct{}), make(chan struct{})
	var workers sync.WaitGroup
	stopped := make(chan struct{}, 10)
	var unblocked bool
	defer func() {
		if !unblocked {
			close(release)
		}
		workers.Wait()
	}()
	for range 10 {
		workers.Go(func() {
			<-start
			NewWorker(NewStore(d), map[string]Handler{KindEmail: func(context.Context, []byte) error {
				if calls.Add(1) == 1 {
					<-release
				}
				return nil
			}}).RunDue(ctx)
			stopped <- struct{}{}
		})
	}
	close(start)
	for range 9 {
		select {
		case <-stopped:
		case <-ctx.Done():
			t.Fatal("workers did not finish while the job was in flight")
		}
	}
	close(release)
	unblocked = true
	workers.Wait()
	if got := calls.Load(); got != 1 {
		t.Fatalf("delivery attempts = %d, want 1", got)
	}
	var done bool
	if err := d.QueryRowContext(ctx, `SELECT done_at IS NOT NULL FROM outbox`).Scan(&done); err != nil || !done {
		t.Fatalf("winning worker did not settle the job: done=%t, err=%v", done, err)
	}
}

func TestClaimReclaimsAtLeaseBoundary(t *testing.T) {
	ctx, s, now := dueJob(t)
	old := mustClaim(t, ctx, s, now)
	if p, err := s.Problems(ctx); err != nil || len(p) != 0 {
		t.Fatalf("first in-flight attempt must not appear as a problem: count=%d err=%v", len(p), err)
	}
	if _, err := s.claim(ctx, now.Add(jobLease-time.Nanosecond)); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("claim before lease expiry: %v, want no rows", err)
	}
	current := mustClaim(t, ctx, s, now.Add(jobLease))
	if current.ID != old.ID || current.Attempts != 2 || bytes.Equal(current.token, old.token) {
		t.Fatal("expired job was not reclaimed under a fresh fence")
	}
}

func TestStaleOwnerCannotSettle(t *testing.T) {
	failure := errors.New("fixture failure")
	for _, tc := range []struct {
		name        string
		runErr      error
		interrupted bool
		final       bool
	}{
		{name: "done"},
		{name: "retry", runErr: failure},
		{name: "failed", runErr: failure, final: true},
		{name: "release", interrupted: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, s, now := dueJob(t)
			if tc.final {
				if _, err := s.db.ExecContext(ctx, `UPDATE outbox SET attempts = ?`, maxAttempts-1); err != nil {
					t.Fatal(err)
				}
			}
			old := mustClaim(t, ctx, s, now)
			now = now.Add(jobLease)
			current := mustClaim(t, ctx, s, now)
			if written, err := s.record(ctx, old, tc.runErr, tc.interrupted, now); err != nil || written {
				t.Fatalf("stale owner could settle: written=%t err=%v", written, err)
			}
			if written, err := s.record(ctx, current, nil, false, now); err != nil || !written {
				t.Fatalf("current owner could not settle: written=%t err=%v", written, err)
			}
		})
	}
}

func TestAdminRetryCannotReuseAnOldFence(t *testing.T) {
	ctx, s, now := dueJob(t)
	first := mustClaim(t, ctx, s, now)
	final := first
	for range maxAttempts - 1 {
		now = now.Add(jobLease)
		final = mustClaim(t, ctx, s, now)
	}
	if written, err := s.record(ctx, final, errors.New("fixture failure"), false, now); err != nil || !written {
		t.Fatalf("dead-letter final attempt: written=%t err=%v", written, err)
	}
	if retried, err := s.Retry(ctx, first.ID); err != nil || !retried {
		t.Fatalf("retry dead job: retried=%t err=%v", retried, err)
	}
	retried := mustClaim(t, ctx, s, now)
	if retried.Attempts != first.Attempts || bytes.Equal(retried.token, first.token) {
		t.Fatal("manual retry did not reset attempts under a fresh token")
	}
	if written, err := s.record(ctx, first, nil, false, now); err != nil || written {
		t.Fatalf("old owner matched after admin reset: written=%t err=%v", written, err)
	}
}

func dueJob(t *testing.T) (context.Context, *Store, time.Time) {
	t.Helper()
	ctx := context.Background()
	d := dbtest.New(t)
	if err := Enqueue(ctx, d, KindEmail, 1); err != nil {
		t.Fatal(err)
	}
	return ctx, NewStore(d), time.Now()
}

func mustClaim(t *testing.T, ctx context.Context, s *Store, now time.Time) heldJob {
	t.Helper()
	j, err := s.claim(ctx, now)
	if err != nil {
		t.Fatalf("claim due job: %v", err)
	}
	return j
}
