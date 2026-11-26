package main

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/17xande-dev/leave/internal/dbtest"
	"github.com/17xande-dev/leave/internal/outbox"
)

func TestOutboxShutdownJoinsBeforeDatabaseClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d, w, started, finish := heldOutbox(t)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		closed := make(chan struct{})
		shutdown := startOutbox(ctx, w, func(context.Context) error { return nil }, func() error {
			defer close(closed)
			var done bool
			err := d.QueryRow(`SELECT done_at IS NOT NULL FROM outbox`).Scan(&done)
			if err == nil && !done {
				err = errors.New("database close preceded the worker's result write")
			}
			return errors.Join(err, d.Close())
		})
		<-started
		cancel()
		done := make(chan error, 1)
		go func() { done <- shutdown() }()
		synctest.Wait()
		select {
		case <-closed:
			t.Error("database closed while the worker was still running")
		default:
		}
		finish()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		select {
		case <-closed:
		default:
			t.Fatal("database was not closed after the worker joined")
		}
	})
}

func TestShutdownLeavesDatabaseOpenAfterWorkerStopTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d, w, started, finish := heldOutbox(t)
		shutdown := startOutbox(t.Context(), w, waitForShutdownDeadline, d.Close)
		<-started
		begin := time.Now()
		err := shutdown()
		if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "stop outbox worker") {
			t.Fatalf("shutdown error = %v, want worker deadline exceeded", err)
		}
		if elapsed := time.Since(begin); elapsed != 10*time.Second {
			t.Fatalf("shutdown waited %s, want the shared 10s deadline", elapsed)
		}
		if err := d.PingContext(t.Context()); err != nil {
			t.Fatalf("database closed while the handler was still running: %v", err)
		}
		finish()
		synctest.Wait()
		var done bool
		if err := d.QueryRow(`SELECT done_at IS NOT NULL FROM outbox`).Scan(&done); err != nil || !done {
			t.Fatalf("late worker could not record success: done=%t err=%v", done, err)
		}
	})
}

func heldOutbox(t *testing.T) (*sql.DB, *outbox.Worker, <-chan struct{}, func()) {
	t.Helper()
	d := dbtest.New(t)
	if err := outbox.Enqueue(t.Context(), d, outbox.KindEmail, "fixture"); err != nil {
		t.Fatal(err)
	}
	started, finish := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(finish) })
	t.Cleanup(release)
	w := outbox.NewWorker(outbox.NewStore(d), map[string]outbox.Handler{
		outbox.KindEmail: func(ctx context.Context, _ []byte) error {
			close(started)
			<-ctx.Done()
			<-finish
			return nil
		},
	})
	return d, w, started, release
}

func TestShutdownWaitBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		closeFinished := make(chan struct{})
		defer func() { <-closeFinished }()
		begin := time.Now()
		err := shutdownService(waitForShutdownDeadline, func(ctx context.Context) error {
			<-ctx.Done()
			return nil
		}, func() error {
			defer close(closeFinished)
			time.Sleep(time.Minute) // Close itself does not accept a context.
			return nil
		})
		if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "close database") {
			t.Fatalf("shutdown error = %v, want close deadline exceeded", err)
		}
		if elapsed := time.Since(begin); elapsed != 13*time.Second {
			t.Fatalf("shutdown waited %s, want 10s drain and join plus 3s close", elapsed)
		}
	})
}

func waitForShutdownDeadline(ctx context.Context) error {
	<-ctx.Done()
	return ctx.Err()
}
