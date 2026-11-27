package outbox

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/17xande-dev/outbox"
)

// Problems returns the most recent jobs with a past failure. A first attempt
// currently running is not a problem, and completed jobs are never fetched.
func Problems(ctx context.Context, q *Queue) ([]outbox.Info, error) {
	const limit = 200
	var problems []outbox.Info
	for _, state := range []string{outbox.StateDead, outbox.StatePending, outbox.StateRunning} {
		found := 0
		for offset := 0; found < limit; offset += limit {
			page, err := q.List(ctx, outbox.Filter{State: state, Limit: limit, Offset: offset})
			if err != nil {
				return nil, err
			}
			for _, job := range page {
				if state == outbox.StateRunning {
					job.Attempts-- // The current delivery has not failed.
				}
				if state == outbox.StateDead || job.Attempts > 0 {
					problems = append(problems, job)
					found++
				}
			}
			if len(page) < limit {
				break
			}
		}
	}
	slices.SortFunc(problems, func(a, b outbox.Info) int {
		if c := b.CreatedAt.Compare(a.CreatedAt); c != 0 {
			return c
		}
		return cmp.Compare(b.ID, a.ID)
	})
	return problems[:min(len(problems), limit)], nil
}

// Summary describes what can be identified from the queue's metadata.
// The delivery payload is private to the runner.
func Summary(job outbox.Info) string {
	switch job.Kind {
	case KindEmail:
		return "Email notification"
	case KindCalendarCreate, KindCalendarDelete:
		number, ok := strings.CutPrefix(job.Subject, "request:")
		id, err := strconv.ParseInt(number, 10, 64)
		if !ok || err != nil || id <= 0 {
			return job.Kind
		}
		if job.Kind == KindCalendarDelete {
			return fmt.Sprintf("Remove request %d on the shared calendar", id)
		}
		return fmt.Sprintf("Add request %d on the shared calendar", id)
	default:
		return job.Kind
	}
}
