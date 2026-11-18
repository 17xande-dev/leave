package leave

import (
	"context"
	"database/sql"
)

// CalendarRange carries the calendar bookkeeping separately from Request.
type CalendarRange struct {
	Range
	EventID string
}

// CalendarRanges reads the ordered ranges without taking a write lock.
func (s *Service) CalendarRanges(ctx context.Context, id int64) ([]CalendarRange, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT start_date, end_date, calendar_event_id FROM request_ranges WHERE request_id = ? ORDER BY start_date`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CalendarRange
	for rows.Next() {
		var r CalendarRange
		if err := rows.Scan(&r.Start, &r.End, &r.EventID); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, ErrNotFound
	}
	return out, nil
}

// RecordRangeEvent records the id of a just-created event on the range the
// worker read, but only if that range is still the one it read: no id yet, the
// same end date, and the request's correction count still equal to revision
// (read before the ranges). A correction that committed during the Graph call
// replaced the ranges, so the event is stale; false tells the caller to delete
// it, and the correction's own job makes the right one.
func (s *Service) RecordRangeEvent(ctx context.Context, requestID int64, startDate, endDate, id string, revision int) (bool, error) {
	var ok bool
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM request_corrections WHERE request_id = ?`, requestID).Scan(&n); err != nil {
			return err
		}
		if n != revision {
			return nil
		}
		res, err := tx.ExecContext(ctx, `UPDATE request_ranges SET calendar_event_id = ? WHERE request_id = ? AND start_date = ? AND end_date = ? AND calendar_event_id = ''`, id, requestID, startDate, endDate)
		if err != nil {
			return err
		}
		c, err := res.RowsAffected()
		if err != nil {
			return err
		}
		ok = c == 1
		return nil
	})
	return ok, err
}

// SetRangeEventID records each successful create/delete immediately.
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
		return nil
	})
}
