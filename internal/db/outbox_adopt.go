package db

import (
	"context"
	"database/sql"
	"fmt"
)

func importLegacyOutbox(ctx context.Context, d *sql.DB) error {
	// Leave stored plaintext JSON and UTC timestamps to whole seconds. The
	// module needs BLOBs and fixed-width microsecond timestamps to order jobs.
	// Do not carry leases across the stopped-worker cutover: keep the charged
	// attempt, so an interrupted final attempt cannot deliver again.
	_, err := d.ExecContext(ctx, `
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
