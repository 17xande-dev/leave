package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// importLegacyOutbox runs on every start, not once. It is safe only because
// nothing deletes outbox_jobs rows: its dedup is the unique key of a row that
// still exists, so a purged job whose legacy row is unfinished would be
// imported and delivered again. Never call Purge or DeleteSubject while the
// legacy outbox table exists; drop the table and this import together.
func importLegacyOutbox(ctx context.Context, d *sql.DB) error {
	// An unreadable timestamp must stop startup naming the row. Skipping it
	// would drop a job silently, and a raw NOT NULL failure names nothing.
	var id int64
	var column string
	err := d.QueryRowContext(ctx, `
		SELECT id, CASE
				WHEN strftime('%Y-%m-%dT%H:%M:%S.000000Z', next_attempt_at) IS NULL THEN 'next_attempt_at'
				WHEN strftime('%Y-%m-%dT%H:%M:%S.000000Z', created_at) IS NULL THEN 'created_at'
				ELSE 'failed_at' END
		FROM outbox
		WHERE done_at IS NULL AND (
			strftime('%Y-%m-%dT%H:%M:%S.000000Z', next_attempt_at) IS NULL
			OR strftime('%Y-%m-%dT%H:%M:%S.000000Z', created_at) IS NULL
			OR (failed_at IS NOT NULL AND strftime('%Y-%m-%dT%H:%M:%S.000000Z', failed_at) IS NULL))
		ORDER BY id LIMIT 1`).Scan(&id, &column)
	if err == nil {
		return fmt.Errorf("import legacy outbox: row %d has an unreadable %s: fix it or mark the row done before upgrading", id, column)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("import legacy outbox: check timestamps: %w", err)
	}
	// Leave stored plaintext JSON and UTC timestamps to whole seconds. The
	// module needs BLOBs and fixed-width microsecond timestamps to order jobs.
	// Do not carry leases across the stopped-worker cutover: keep the charged
	// attempt, so an interrupted final attempt cannot deliver again.
	_, err = d.ExecContext(ctx, `
		INSERT INTO outbox_jobs (
			kind, subject, unique_key, payload, state, attempts, max_attempts,
			run_at, last_error, created_at, updated_at, finished_at
		)
		SELECT legacy.kind,
			CASE WHEN legacy.kind IN ('calendar_create', 'calendar_delete') AND json_valid(legacy.payload)
				THEN COALESCE('request:' || json_extract(legacy.payload, '$.request_id'), '')
				ELSE '' END,
			'legacy:outbox:' || legacy.id, CAST(legacy.payload AS BLOB),
			CASE WHEN legacy.failed_at IS NULL THEN 'pending' ELSE 'dead' END,
			legacy.attempts, 12,
			strftime('%Y-%m-%dT%H:%M:%S.000000Z', legacy.next_attempt_at),
			legacy.last_error,
			strftime('%Y-%m-%dT%H:%M:%S.000000Z', legacy.created_at),
			strftime('%Y-%m-%dT%H:%M:%S.000000Z', COALESCE(legacy.failed_at, legacy.created_at)),
			strftime('%Y-%m-%dT%H:%M:%S.000000Z', legacy.failed_at)
		FROM outbox AS legacy
		WHERE legacy.done_at IS NULL
			AND NOT EXISTS (SELECT 1 FROM outbox_jobs WHERE unique_key = 'legacy:outbox:' || legacy.id)
		ORDER BY legacy.id`)
	if err != nil {
		return fmt.Errorf("import legacy outbox: %w", err)
	}
	return nil
}
