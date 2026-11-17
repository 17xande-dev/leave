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
