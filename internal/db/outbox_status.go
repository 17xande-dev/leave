package db

import (
	"context"
	"database/sql"
	"fmt"
	"io"

	"github.com/17xande-dev/outbox/sqlitestore"
)

// writeOutboxStatus prints the module's own migration history, which the
// app's goose table does not hold, so "-migrate" shows more than "11 -> 11".
// The module's status call reads without creating the history table.
func writeOutboxStatus(ctx context.Context, d *sql.DB, w io.Writer) error {
	status, err := sqlitestore.MigrationStatus(ctx, d)
	if err != nil {
		return err
	}
	for _, migration := range status.Migrations {
		state := "pending"
		if migration.Applied {
			state = "applied"
		}
		if _, err := fmt.Fprintf(w, "outbox %04d %s\n", migration.Version, state); err != nil {
			return err
		}
	}
	return nil
}
