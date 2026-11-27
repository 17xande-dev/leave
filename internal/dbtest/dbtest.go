// Package dbtest gives each test its own real SQLite database. Not a mock: the
// schema's CHECKs, foreign keys and unique indexes are part of what is tested.
package dbtest

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/17xande-dev/leave/internal/db"
	"github.com/17xande-dev/leave/internal/outbox"
)

func New(t testing.TB) *sql.DB {
	t.Helper()
	d, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// Outbox attaches a queue with delivery disabled to a fixture database.
func Outbox(t testing.TB, d *sql.DB) *outbox.Queue {
	t.Helper()
	q, err := outbox.NewEnqueuer(d)
	if err != nil {
		t.Fatal(err)
	}
	return q
}
