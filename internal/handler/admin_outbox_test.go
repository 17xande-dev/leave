package handler

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/17xande-dev/outbox"
	"github.com/17xande-dev/websec/webtest"
)

func TestAdminOutboxRetryReportsStalePage(t *testing.T) {
	hs := newHarness(t)
	if _, err := hs.db.ExecContext(t.Context(), `INSERT INTO outbox_jobs
		(id,kind,payload,state,attempts,max_attempts,run_at,created_at,updated_at,finished_at,last_error)
		VALUES (42,'email',x'7b7d','dead',12,12,'2026-01-01T00:00:00.000000Z','2026-01-01T00:00:00.000000Z',
			'2026-01-02T00:00:00.000000Z','2026-01-02T00:00:00.000000Z','fixture failure')`); err != nil {
		t.Fatal(err)
	}
	hr, err := hs.us.ByUsername(t.Context(), "hr")
	if err != nil {
		t.Fatal(err)
	}
	c := webtest.NewServer(t, hs.srv)
	c.PostForm("/login", url.Values{"username": {"hr"}, "password": {"fixture"}}).WantStatus(http.StatusSeeOther)
	const path = "/admin/outbox/42/retry"
	if page := c.Get("/admin/outbox"); page.Status != http.StatusOK || !strings.Contains(page.Body, `action="`+path+`"`) ||
		!strings.Contains(page.Body, "fixture failure") || !strings.Contains(page.Body, "queued 2026-01-01T00:00:00Z") {
		t.Fatal("dead job is missing its retry form, timestamp or failure detail")
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
	job, err := hs.h.outbox.Get(t.Context(), 42)
	if err != nil || job.State != outbox.StatePending || job.Attempts != 0 || job.RetryCount != 1 ||
		job.RetriedBy != fmt.Sprintf("user:%d", hr.ID) {
		t.Fatalf("retry should requeue and audit exactly once: %+v (%v)", job, err)
	}
}
