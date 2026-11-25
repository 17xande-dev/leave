package outbox

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/17xande-dev/leave/internal/dbtest"
	"github.com/17xande-dev/websec/webtest"
)

func TestOutboxLogsClassButKeepsOneLineErrorForAdmins(t *testing.T) {
	logger, logs := webtest.Logger(t)
	previous := slog.Default()
	slog.SetDefault(logger)
	t.Cleanup(func() { slog.SetDefault(previous) })

	const errMarker = "fixture_error_marker"
	const payloadMarker = "fixture_payload_marker"
	ctx := context.Background()
	d := dbtest.New(t)
	s := NewStore(d)
	if err := Enqueue(ctx, d, KindEmail, payloadMarker); err != nil {
		t.Fatal(err)
	}
	raw := errMarker + "\r\n\tremote\x00 failed " + strings.Repeat("界", 300)
	NewWorker(s, map[string]Handler{KindEmail: func(context.Context, []byte) error {
		return errors.New(raw)
	}}).RunDue(ctx)

	webtest.NoSecrets(t, logs.String(), "outbox: job failed", errMarker, payloadMarker)
	if !strings.Contains(logs.String(), `"error_class":`) {
		t.Fatal("outbox warning has no error class")
	}
	ps, err := s.Problems(ctx)
	if err != nil {
		t.Fatal(err)
	}
	prefix := errMarker + " remote failed "
	want := prefix + strings.Repeat("界", 300-len(prefix))
	if len(ps) != 1 || ps[0].LastError != want {
		t.Fatal("admin error is not one line, control-free and capped at 300 runes")
	}
}
