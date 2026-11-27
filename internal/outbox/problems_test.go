package outbox_test

import (
	"reflect"
	"testing"

	"github.com/17xande-dev/leave/internal/dbtest"
	leaveoutbox "github.com/17xande-dev/leave/internal/outbox"
	"github.com/17xande-dev/outbox"
)

func TestProblemsIncludesPastAttemptsAndDeadJobs(t *testing.T) {
	d := dbtest.New(t)
	q := dbtest.Outbox(t, d)
	const stamp = "2026-01-01T00:00:00.000000Z"
	for _, row := range []struct {
		state    string
		attempts int
	}{
		{outbox.StatePending, 2}, // ID 1; older than 200 new pending jobs
		{outbox.StateRunning, 1}, // ID 2; first in-flight attempt
		{outbox.StateRunning, 3}, // ID 3; two past attempts
		{outbox.StateDead, 12},   // ID 4; still shown and can be retried
		{outbox.StateDone, 3},    // ID 5; never a problem
	} {
		_, err := d.ExecContext(t.Context(), `INSERT INTO outbox_jobs
			(kind, payload, state, attempts, max_attempts, run_at, created_at, updated_at, lease_until, claim_token)
			VALUES ('email', x'', ?1, ?2, 12, ?3, ?3, ?3,
				CASE WHEN ?1 = 'running' THEN ?3 END, CASE WHEN ?1 = 'running' THEN randomblob(16) END)`,
			row.state, row.attempts, stamp)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.ExecContext(t.Context(), `WITH RECURSIVE rows(n) AS (
		VALUES (1) UNION ALL SELECT n+1 FROM rows WHERE n < 200
	)
	INSERT INTO outbox_jobs (kind, payload, state, max_attempts, run_at, created_at, updated_at)
		SELECT 'email', x'', 'pending', 12, ?1, ?1, ?1 FROM rows`, stamp); err != nil {
		t.Fatal(err)
	}
	probs, err := leaveoutbox.Problems(t.Context(), q)
	if err != nil {
		t.Fatal(err)
	}
	type past struct {
		id       int64
		attempts int
	}
	var got []past
	for _, job := range probs {
		got = append(got, past{job.ID, job.Attempts})
	}
	if want := []past{{4, 12}, {3, 2}, {1, 2}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("problems: %+v, want %+v", got, want)
	}
}

func TestSummaryUsesCalendarReference(t *testing.T) {
	for _, tc := range []struct {
		kind, subject, want string
	}{
		{leaveoutbox.KindCalendarCreate, "request:7", "Add request 7 on the shared calendar"},
		{leaveoutbox.KindCalendarDelete, "request:7", "Remove request 7 on the shared calendar"},
		{leaveoutbox.KindCalendarCreate, "7", "calendar_create"},
		{leaveoutbox.KindEmail, "", "Email notification"},
		{leaveoutbox.KindEmail, "request:7:email", "Email about request 7"},
		{leaveoutbox.KindEmail, "request:7", "Email notification"},
		{leaveoutbox.KindEmail, "request:x:email", "Email notification"},
	} {
		if got := leaveoutbox.Summary(outbox.Info{Kind: tc.kind, Subject: tc.subject}); got != tc.want {
			t.Errorf("Summary(%s, %s) = %q, want %q", tc.kind, tc.subject, got, tc.want)
		}
	}
}
