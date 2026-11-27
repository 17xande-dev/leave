package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"

	"github.com/17xande-dev/sqlitekit/goosestrict"
	"github.com/pressly/goose/v3/database"
)

const (
	outboxHistory = "outbox_goose_version"
	// outbox v0.1.0 ships only migration 0001 and has no status API. A bump
	// that adds one must raise this; TestOutboxStatusMatchesTheLibrary fails
	// until it does.
	outboxLatest int64 = 1
)

// writeOutboxStatus prints the module's own migration history, which the
// app's goose table does not hold, so "-migrate" shows more than "11 -> 11".
// It reads without creating the history table.
func writeOutboxStatus(ctx context.Context, d *sql.DB, w io.Writer) error {
	store, err := goosestrict.NewStoreTable(outboxHistory)
	if err != nil {
		return fmt.Errorf("outbox migration history: %w", err)
	}
	state := "pending"
	exists, err := store.(database.StoreExtender).TableExists(ctx, d)
	if err != nil {
		return fmt.Errorf("inspect outbox migration history: %w", err)
	}
	if exists {
		current, err := store.GetLatestVersion(ctx, d)
		if err != nil && !errors.Is(err, database.ErrVersionNotFound) {
			return fmt.Errorf("read outbox migration version: %w", err)
		}
		if current > outboxLatest {
			return fmt.Errorf("outbox migration %d is newer than this build's latest outbox migration %d", current, outboxLatest)
		}
		if current == outboxLatest {
			v, err := store.GetMigration(ctx, d, outboxLatest)
			if err != nil {
				return fmt.Errorf("read outbox migration status: %w", err)
			}
			if v.IsApplied {
				state = "applied"
			}
		}
	}
	_, err = fmt.Fprintf(w, "outbox %04d %s\n", outboxLatest, state)
	return err
}
