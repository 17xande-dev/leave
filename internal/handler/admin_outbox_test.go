package handler

import (
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/17xande-dev/websec/webtest"
)

func TestAdminOutboxRetryReportsStalePage(t *testing.T) {
	hs := newHarness(t)
	var kicks atomic.Int32
	hs.h.kick = func() { kicks.Add(1) }
	if _, err := hs.db.ExecContext(t.Context(), `INSERT INTO outbox
		(id,kind,payload,attempts,next_attempt_at,created_at,failed_at,last_error)
		VALUES (42,'email','{}',12,'2026-01-01T00:00:00Z','2026-01-01T00:00:00Z','2026-01-02T00:00:00Z','fixture failure')`); err != nil {
		t.Fatal(err)
	}
	c := webtest.NewServer(t, hs.srv)
	c.PostForm("/login", url.Values{"username": {"hr"}, "password": {"fixture"}}).WantStatus(http.StatusSeeOther)
	const path = "/admin/outbox/42/retry"
	if page := c.Get("/admin/outbox"); page.Status != http.StatusOK || !strings.Contains(page.Body, `action="`+path+`"`) {
		t.Fatal("dead job has no retry form")
	}
	first := c.PostForm(path, nil)
	first.WantRedirect(http.StatusSeeOther, "/admin/outbox?notice=retried")
	if page := c.Follow(first); page.Status != http.StatusOK || !strings.Contains(page.Body, "Queued for another attempt.") {
		t.Fatal("successful retry notice is missing")
	}
	// Submitting the previously loaded form again targets a now-active job.
	stale := c.PostForm(path, nil)
	stale.WantRedirect(http.StatusSeeOther, "/admin/outbox?notice=retry-stale")
	if page := c.Follow(stale); page.Status != http.StatusOK ||
		!strings.Contains(page.Body, "No job was retried. It may have already been retried or delivered.") {
		t.Fatal("stale retry did not render the no-op notice")
	}
	if got := kicks.Load(); got != 1 {
		t.Fatalf("worker kicked %d times, want one kick for the successful retry", got)
	}
}
