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
	sharedoutbox "github.com/17xande-dev/outbox"
)

func TestOutboxShutdownJoinsBeforeDatabaseClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d, w, id, started, finish := heldOutbox(t)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		closed := make(chan struct{})
		shutdown := startOutbox(ctx, w, func(context.Context) error { return nil }, func() error {
			defer close(closed)
			job, err := w.Get(t.Context(), id)
			if err == nil && job.State != sharedoutbox.StateDone {
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
		d, w, id, started, finish := heldOutbox(t)
		shutdown := startOutbox(t.Context(), w, func(context.Context) error { return nil }, d.Close)
		<-started
		begin := time.Now()
		err := shutdown()
		if err == nil || !strings.Contains(err.Error(), "stop outbox worker") {
			t.Fatalf("shutdown error = %v, want worker stop timeout", err)
		}
		if elapsed := time.Since(begin); elapsed != 5*time.Second {
			t.Fatalf("shutdown waited %s, want the module's 5s stop timeout", elapsed)
		}
		if err := d.PingContext(t.Context()); err != nil {
			t.Fatalf("database closed while the handler was still running: %v", err)
		}
		finish()
		synctest.Wait()
		if job, err := w.Get(t.Context(), id); err != nil || job.State != sharedoutbox.StateDone {
			t.Fatalf("late worker could not record success: state=%s err=%v", job.State, err)
		}
	})
}

func heldOutbox(t *testing.T) (*sql.DB, *outbox.Queue, int64, <-chan struct{}, func()) {
	t.Helper()
	d := dbtest.New(t)
	started, finish := make(chan struct{}), make(chan struct{})
	release := sync.OnceFunc(func() { close(finish) })
	t.Cleanup(release)
	w, err := outbox.New(d, map[string]outbox.Handler{
		outbox.KindEmail: func(ctx context.Context, _ []byte) error {
			close(started)
			<-ctx.Done()
			<-finish
			return nil
		},
	}, sharedoutbox.Options{})
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := w.EnqueueNow(t.Context(), sharedoutbox.Message{Kind: outbox.KindEmail})
	if err != nil {
		t.Fatal(err)
	}
	return d, w, id, started, release
}

func TestShutdownWaitBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		closeFinished := make(chan struct{})
		defer func() { <-closeFinished }()
		begin := time.Now()
		err := shutdownService(waitForShutdownDeadline, func() error { return nil }, func() error {
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
