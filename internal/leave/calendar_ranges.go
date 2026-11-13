package leave

import (
	"context"
	"database/sql"
)

// CalendarRange carries the calendar bookkeeping separately from Request.
type CalendarRange struct {
	Range
	EventID string
	// LegacyEventID is transitional: a distinct event written by the old
	// container must also be deleted, even if this range already has an id.
	LegacyEventID string
}

// CalendarRanges adopts an old container's event atomically before returning
// the ordered ranges. Remove this compatibility shim with migration 0005.
func (s *Service) CalendarRanges(ctx context.Context, id int64) ([]CalendarRange, error) {
	var out []CalendarRange
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var legacy string
		if err := tx.QueryRowContext(ctx, `SELECT calendar_event_id FROM requests WHERE id = ?`, id).Scan(&legacy); err != nil {
			return err
		}
		rows, err := tx.QueryContext(ctx, `SELECT start_date, end_date, calendar_event_id FROM request_ranges WHERE request_id = ? ORDER BY start_date`, id)
		if err != nil {
			return err
		}
		for rows.Next() {
			var r CalendarRange
			if err := rows.Scan(&r.Start, &r.End, &r.EventID); err != nil {
				rows.Close()
				return err
			}
			out = append(out, r)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(out) == 0 {
			return ErrNotFound
		}
		if legacy != "" {
			first := &out[0]
			if first.EventID == "" || first.EventID == legacy {
				if _, err := tx.ExecContext(ctx, `UPDATE request_ranges SET calendar_event_id = ? WHERE request_id = ? AND start_date = ?`, legacy, id, first.Start); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `UPDATE requests SET calendar_event_id = '' WHERE id = ?`, id); err != nil {
					return err
				}
				first.EventID = legacy
			} else {
				first.LegacyEventID = legacy
			}
		}
		return nil
	})
	return out, err
}

// SetRangeEventID records each successful create/delete immediately. Clearing
// the first range also clears the legacy id, after the handler deletes both.
func (s *Service) SetRangeEventID(ctx context.Context, requestID int64, startDate, id string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE request_ranges SET calendar_event_id = ? WHERE request_id = ? AND start_date = ?`, id, requestID, startDate)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n != 1 {
			return ErrNotFound
		}
		if id == "" {
			_, err = tx.ExecContext(ctx, `UPDATE requests SET calendar_event_id = '' WHERE id = ? AND ? = (SELECT min(start_date) FROM request_ranges WHERE request_id = ?)`, requestID, startDate, requestID)
		}
		return err
	})
}
