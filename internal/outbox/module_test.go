package outbox_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/17xande-dev/leave/internal/db"
	"github.com/17xande-dev/leave/internal/dbtest"
	leaveoutbox "github.com/17xande-dev/leave/internal/outbox"
	"github.com/17xande-dev/outbox"
	"github.com/17xande-dev/outbox/outboxtest"
	"github.com/17xande-dev/outbox/sqlitestore"
)

func TestLeaveOutboxConformance(t *testing.T) {
	var d *sql.DB
	var path string
	payload := func(t *testing.T, id int64) []byte {
		var bytes []byte
		if err := d.QueryRowContext(t.Context(), `SELECT payload FROM outbox_jobs WHERE id = ?`, id).Scan(&bytes); err != nil {
			t.Fatal(err)
		}
		return bytes
	}
	outboxtest.Run(t, outboxtest.Harness[*sql.Tx]{
		New: func(t *testing.T) (outbox.Store[*sql.Tx], func()) {
			path = filepath.Join(t.TempDir(), "outbox.db")
			var err error
			d, err = db.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			return sqlitestore.New(d), func() { d.Close() }
		},
		Payload: payload,
		Corrupt: func(t *testing.T, id int64) {
			bytes := payload(t, id)
			bytes[0] ^= 1
			if _, err := d.ExecContext(t.Context(), `UPDATE outbox_jobs SET payload = ? WHERE id = ?`, bytes, id); err != nil {
				t.Fatal(err)
			}
		},
		ConcurrentStores: func(t *testing.T, n int) []outbox.Store[*sql.Tx] {
			stores := make([]outbox.Store[*sql.Tx], n)
			for i := range stores {
				pool, err := db.Open(path)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { pool.Close() })
				stores[i] = sqlitestore.New(pool)
			}
			return stores
		},
	})
}

func TestLeaveOutboxBackoffAndAttemptLimit(t *testing.T) {
	delays := []time.Duration{
		time.Minute, 2 * time.Minute, 5 * time.Minute, 10 * time.Minute, 20 * time.Minute, 30 * time.Minute,
		time.Hour, 2 * time.Hour, 3 * time.Hour, 4 * time.Hour, 6 * time.Hour,
	}
	for _, kind := range []string{leaveoutbox.KindEmail, leaveoutbox.KindCalendarCreate, leaveoutbox.KindCalendarDelete} {
		t.Run(kind, func(t *testing.T) {
			d := dbtest.New(t)
			clock := outboxtest.NewClock(time.Time{})
			calls := 0
			deliveryErr := errors.New("fixture delivery error")
			log := outboxtest.NewLog()
			q, err := leaveoutbox.New(d, map[string]leaveoutbox.Handler{
				kind: func(context.Context, []byte) error {
					calls++
					return deliveryErr
				},
			}, outbox.Options{Now: clock.Now, Logger: log.Logger()})
			if err != nil {
				t.Fatal(err)
			}
			id, _, err := q.EnqueueNow(t.Context(), outbox.Message{Kind: kind})
			if err != nil {
				t.Fatal(err)
			}
			for attempt := 1; attempt <= 12; attempt++ {
				now := clock.Now()
				outboxtest.Drain(t, q)
				job, err := q.Get(t.Context(), id)
				if err != nil {
					t.Fatal(err)
				}
				if calls != attempt || job.Attempts != attempt {
					t.Fatalf("attempt %d: got %d deliveries and %d charged attempts", attempt, calls, job.Attempts)
				}
				if attempt == 12 {
					if job.State != outbox.StateDead || !job.FinishedAt.Equal(now) || !job.RunAt.Equal(now) ||
						job.LastError != "fixture delivery error" {
						t.Fatalf("final attempt should dead-letter without rescheduling: %+v", job)
					}
					break
				}
				delay := delays[attempt-1]
				if job.State != outbox.StatePending || job.RunAt.Sub(now) < delay-delay/10 || job.RunAt.Sub(now) > delay+delay/10 {
					t.Fatalf("attempt %d: state=%s delay=%s, want pending at %s ± 10%%", attempt, job.State, job.RunAt.Sub(now), delay)
				}
				clock.Set(job.RunAt.Add(-time.Microsecond))
				if ran := outboxtest.Drain(t, q); ran != 0 || calls != attempt {
					t.Fatalf("attempt %d delivered before its backoff expired", attempt+1)
				}
				clock.Set(job.RunAt)
			}
			clock.Advance(24 * time.Hour)
			if ran := outboxtest.Drain(t, q); ran != 0 || calls != 12 {
				t.Fatalf("dead letter delivered again: %d calls, want 12", calls)
			}
			log.RequireAbsent(t, "fixture delivery error")
			deliveryErr = nil
			if retried, err := q.Retry(t.Context(), id, "user:1"); !retried || err != nil {
				t.Fatalf("retry dead job: retried=%t err=%v", retried, err)
			}
			outboxtest.Drain(t, q)
			if probs, err := leaveoutbox.Problems(t.Context(), q); err != nil || len(probs) != 0 || calls != 13 {
				t.Fatalf("after successful retry: problems=%+v calls=%d err=%v", probs, calls, err)
			}
		})
	}
}

func TestLeaveOutboxLogsClassAndKeepsAdminError(t *testing.T) {
	const errorMarker = "fixture_error_marker"
	const payloadMarker = "fixture_payload_marker"
	log := outboxtest.NewLog()
	q, err := leaveoutbox.New(dbtest.New(t), map[string]leaveoutbox.Handler{
		leaveoutbox.KindEmail: func(context.Context, []byte) error {
			return errors.New(errorMarker + "\r\n\tremote\x00 failed " + strings.Repeat("界", 300))
		},
	}, outbox.Options{Logger: log.Logger()})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := q.EnqueueNow(t.Context(), outbox.Message{Kind: leaveoutbox.KindEmail, Payload: []byte(payloadMarker)}); err != nil {
		t.Fatal(err)
	}
	outboxtest.Drain(t, q)
	if !strings.Contains(log.String(), "error_class=") || !strings.Contains(log.String(), "outcome=retry") {
		t.Fatal("outbox did not log a classified retry")
	}
	log.RequireAbsent(t, errorMarker, payloadMarker)
	probs, err := leaveoutbox.Problems(t.Context(), q)
	prefix := errorMarker + "  remote failed "
	want := prefix + strings.Repeat("界", 300-len(prefix))
	if err != nil || len(probs) != 1 || probs[0].LastError != want {
		t.Fatal("admin error is not the expected control-free text capped at 300 runes")
	}
}

func TestLeaveOutboxWakeRunsPromptly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ran := make(chan struct{}, 1)
		d := dbtest.New(t)
		q, err := leaveoutbox.New(d, map[string]leaveoutbox.Handler{
			leaveoutbox.KindEmail: func(context.Context, []byte) error {
				ran <- struct{}{}
				return nil
			},
		}, outbox.Options{Poll: time.Hour})
		if err != nil {
			t.Fatal(err)
		}
		q.Start(t.Context())
		defer func() {
			if err := q.Stop(); err != nil {
				t.Error(err)
			}
		}()
		synctest.Wait() // the start-up drain has found the queue empty
		tx, err := d.BeginTx(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if _, _, err := q.Enqueue(t.Context(), tx, outbox.Message{Kind: leaveoutbox.KindEmail}); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
		q.Wake()
		select {
		case <-ran:
		case <-time.After(time.Second):
			t.Fatal("woken worker waited for its next poll")
		}
	})
}

func TestLeaveOutboxEnqueueRollsBackWithTheCaller(t *testing.T) {
	d := dbtest.New(t)
	d.SetMaxOpenConns(2)
	q := dbtest.Outbox(t, d)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := leaveoutbox.Enqueue(ctx, q, tx, leaveoutbox.KindEmail, "", struct{}{}); err != nil {
		t.Fatal(err)
	}
	var inTx int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM outbox_jobs`).Scan(&inTx); err != nil || inTx != 1 {
		t.Fatalf("caller transaction has %d jobs (%v), want 1", inTx, err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if jobs := outboxtest.Enqueued(t, q, outbox.Filter{}); len(jobs) != 0 {
		t.Fatalf("rolled-back transaction left %d jobs", len(jobs))
	}
}

func TestLeaveOutboxLastAttemptCanSucceed(t *testing.T) {
	d := dbtest.New(t)
	q, err := leaveoutbox.New(d, map[string]leaveoutbox.Handler{leaveoutbox.KindEmail: func(context.Context, []byte) error {
		return nil
	}}, outbox.Options{Logger: outboxtest.NewLog().Logger()})
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := q.EnqueueNow(t.Context(), outbox.Message{Kind: leaveoutbox.KindEmail})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.ExecContext(t.Context(), `UPDATE outbox_jobs SET attempts = 11 WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if ran := outboxtest.Drain(t, q); ran != 1 {
		t.Fatalf("last attempt claims = %d, want 1", ran)
	}
	job, err := q.Get(t.Context(), id)
	if err != nil || job.State != outbox.StateDone || job.Attempts != 12 {
		t.Fatalf("last allowed attempt should succeed: %+v (%v)", job, err)
	}
}
