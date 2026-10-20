package outbox

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/17xande-dev/leave/internal/dbtest"
)

func TestRetriesThenFails(t *testing.T) {
	ctx := context.Background()
	d := dbtest.New(t)
	s := NewStore(d)
	if err := Enqueue(ctx, d, KindEmail, map[string]string{"to": "a"}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	w := NewWorker(s, map[string]Handler{KindEmail: func(context.Context, []byte) error {
		calls++
		return errors.New("graph down")
	}})
	now := time.Now()
	w.now = func() time.Time { return now }

	w.RunDue(ctx)
	if calls != 1 {
		t.Fatalf("calls = %d after first run", calls)
	}
	// Not due again until its backoff has passed.
	w.RunDue(ctx)
	if calls != 1 {
		t.Fatalf("retried before its backoff: %d calls", calls)
	}
	for range maxAttempts * 2 {
		now = now.Add(7 * time.Hour)
		w.RunDue(ctx)
	}
	if calls != maxAttempts {
		t.Fatalf("calls = %d, want exactly %d before giving up", calls, maxAttempts)
	}
	ps, _ := s.Problems(ctx)
	if len(ps) != 1 || ps[0].FailedAt == "" || ps[0].LastError != "graph down" {
		t.Fatalf("problems = %+v", ps)
	}

	// A person retries it, and this time it works.
	s.Retry(ctx, ps[0].ID)
	w.handlers[KindEmail] = func(context.Context, []byte) error { calls++; return nil }
	w.RunDue(ctx)
	if ps, _ := s.Problems(ctx); len(ps) != 0 {
		t.Fatalf("still a problem after a successful retry: %+v", ps)
	}
}

// Jobs run in the order written: a calendar delete must not overtake the
// create it undoes.
func TestRunsInOrder(t *testing.T) {
	ctx := context.Background()
	d := dbtest.New(t)
	Enqueue(ctx, d, KindCalendarCreate, 1)
	Enqueue(ctx, d, KindCalendarDelete, 1)
	var got []string
	rec := func(k string) Handler {
		return func(context.Context, []byte) error { got = append(got, k); return nil }
	}
	NewWorker(NewStore(d), map[string]Handler{
		KindCalendarCreate: rec("create"), KindCalendarDelete: rec("delete"),
	}).RunDue(ctx)
	if len(got) != 2 || got[0] != "create" || got[1] != "delete" {
		t.Fatalf("order = %v", got)
	}
}

// A kick must run a freshly written job straight away, not at the next poll:
// an approval email should not wait half a minute.
func TestKickRunsPromptly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := dbtest.New(t)
	ran := make(chan struct{}, 1)
	w := NewWorker(NewStore(d), map[string]Handler{KindEmail: func(context.Context, []byte) error {
		ran <- struct{}{}
		return nil
	}})
	go w.Run(ctx, time.Hour)
	time.Sleep(50 * time.Millisecond) // let the first, empty pass finish
	Enqueue(ctx, d, KindEmail, 1)
	w.Kick()
	select {
	case <-ran:
	case <-time.After(3 * time.Second):
		t.Fatal("kicked job did not run")
	}
}
