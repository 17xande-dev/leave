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
	if retried, err := s.Retry(ctx, ps[0].ID); err != nil || !retried {
		t.Fatalf("retry dead job: retried=%t err=%v", retried, err)
	}
	w.handlers[KindEmail] = func(context.Context, []byte) error { calls++; return nil }
	w.RunDue(ctx)
	if ps, _ := s.Problems(ctx); len(ps) != 0 {
		t.Fatalf("still a problem after a successful retry: %+v", ps)
	}
}

// A single worker prefers the oldest eligible job when both are due.
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
	stop := w.Start(ctx, time.Hour)
	defer func() {
		wait, end := context.WithTimeout(context.Background(), time.Second)
		defer end()
		if err := stop(wait); err != nil {
			t.Error(err)
		}
	}()
	time.Sleep(50 * time.Millisecond) // let the first, empty pass finish
	Enqueue(ctx, d, KindEmail, 1)
	w.Kick()
	select {
	case <-ran:
	case <-time.After(3 * time.Second):
		t.Fatal("kicked job did not run")
	}
}

// A correction's calendar job also removes the events it replaces, and the
// admin queue page must say so: otherwise it reads as a plain add.
func TestSummaryMentionsOldEventsACorrectionRemoves(t *testing.T) {
	cases := []struct{ payload, want string }{
		{`{"request_id":7}`, "Add request 7 on the shared calendar"},
		{`{"request_id":7,"delete_event_ids":["a"]}`, "Add request 7 on the shared calendar (and removes 1 old event)"},
		{`{"request_id":7,"delete_event_ids":["a","b"]}`, "Add request 7 on the shared calendar (and removes 2 old events)"},
	}
	for _, c := range cases {
		if got := Summary(Job{Kind: KindCalendarCreate, Payload: []byte(c.payload)}); got != c.want {
			t.Errorf("Summary(%s) = %q, want %q", c.payload, got, c.want)
		}
	}
}
