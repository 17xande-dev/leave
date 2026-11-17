package leave

import (
	"database/sql"
	"slices"
	"strings"
)

// Range is one inclusive, contiguous stretch of leave.
type Range struct{ Start, End string }

func (in Input) ranges() ([]Range, error) {
	if len(in.Ranges) != 0 {
		if in.Start != "" || in.End != "" {
			return nil, FieldErrors{"start": "Use either ranges or first and last day, not both."}
		}
		return in.Ranges, nil
	}
	return []Range{{in.Start, in.End}}, nil
}

func normaliseRanges(ranges []Range) ([]Range, error) {
	if len(ranges) == 0 {
		return nil, FieldErrors{"start": "Enter the first day of leave."}
	}
	out := slices.Clone(ranges)
	for _, r := range out {
		if _, err := ParseDate(r.Start); err != nil {
			return nil, FieldErrors{"start": "Enter the first day of leave."}
		}
		if _, err := ParseDate(r.End); err != nil {
			return nil, FieldErrors{"end": "Enter the last day of leave."}
		}
		if r.End < r.Start {
			return nil, FieldErrors{"end": "The last day cannot be before the first day."}
		}
	}
	slices.SortFunc(out, func(a, b Range) int {
		if a.Start < b.Start {
			return -1
		}
		if a.Start > b.Start {
			return 1
		}
		return 0
	})
	merged := out[:0]
	for _, r := range out {
		if len(merged) > 0 {
			last := &merged[len(merged)-1]
			end, _ := ParseDate(last.End)
			if r.Start <= fmtDate(end.AddDate(0, 0, 1)) {
				if r.End > last.End {
					last.End = r.End
				}
				continue
			}
		}
		merged = append(merged, r)
	}
	if len(merged) > 3 {
		return nil, FieldErrors{"start": "A request can have at most 3 separate ranges of days."}
	}
	return merged, nil
}

func rangesString(ranges []Range) string {
	parts := make([]string, len(ranges))
	for i, r := range ranges {
		parts[i] = r.Start + "/" + r.End
	}
	return strings.Join(parts, " ")
}

// insertRanges is the sole application writer of range dates. The request's
// span and its ranges are committed together. The leading DELETE supports
// Edit's replacement and SubmitDraft's rewrite of existing ranges.
func insertRanges(tx *sql.Tx, id int64, ranges []Range) error {
	ranges, err := normaliseRanges(ranges)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM request_ranges WHERE request_id = ?`, id); err != nil {
		return err
	}
	for _, r := range ranges {
		if _, err := tx.Exec(`INSERT INTO request_ranges (request_id, start_date, end_date) VALUES (?, ?, ?)`, id, r.Start, r.End); err != nil {
			return err
		}
	}
	_, err = tx.Exec(`UPDATE requests SET start_date = ?, end_date = ? WHERE id = ?`, ranges[0].Start, ranges[len(ranges)-1].End, id)
	return err
}
