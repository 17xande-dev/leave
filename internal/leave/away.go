package leave

import (
	"context"

	"github.com/17xande-dev/leave/internal/users"
)

// Away is one stretch of leave a viewer may see on someone else.
type Away struct {
	UserID     int64
	Name       string
	Start, End string
	// Status is "pending" or "approved".
	Status string
	// LeaveType is filled only for the person's approver (and for admins).
	// Everyone else gets name and dates: sick leave stays private.
	LeaveType string
}

// Away lists who is away between start and end (YYYY-MM-DD, inclusive) among
// the viewer's department, the people who share a live volunteer team with
// them, and their reports (as in Team: by directory manager, or by approver
// override). Pending and approved leave only: a draft or a request awaiting HR
// is not yet a plan. The viewer and disabled people are left out.
//
// The type of leave is returned only where the viewer is an admin, is the
// frozen approver_id of that request, or the person is the viewer's report
// (by directory manager or approver override); it never reaches a colleague.
// The test is on the request's own approver_id, not a fresh ResolveApprover,
// so it follows the request. Two consequences: an admin gets the type only for
// people already in their own department, teams or reports, and someone who
// approved an escalated request at an earlier step (approver_id has moved up
// since) loses the type unless the person is their report. Phase 6 (the
// approvals queue) will use this view, so that is the rule to keep. One query: the report test is a column so the type can
// be blanked in SQL, before it reaches any caller that might render it.
func (s *Service) Away(ctx context.Context, viewer users.User, start, end string) ([]Away, error) {
	// A report: managed in the directory (unless an override sends them
	// elsewhere), or overridden to the viewer.
	const report = `((u.manager_dn = ?1 AND ?1 <> '' AND u.id NOT IN (SELECT user_id FROM approver_overrides))
		OR u.id IN (SELECT user_id FROM approver_overrides WHERE approver_id = ?2))`
	rows, err := s.db.QueryContext(ctx, `SELECT u.id, u.name, r.start_date, r.end_date, r.status,
			CASE WHEN ?3 = 1 OR r.approver_id = ?2 OR `+report+` THEN t.name ELSE '' END
		FROM requests r
		JOIN users u ON u.id = r.user_id
		JOIN leave_types t ON t.id = r.type_id
		WHERE r.kind = 'leave' AND r.status IN ('pending', 'approved')
		AND r.start_date <= ?5 AND r.end_date >= ?4
		AND u.disabled = 0 AND u.id <> ?2
		AND ((?6 <> '' AND u.department = ?6)
			OR u.id IN (SELECT m2.user_id FROM team_members m1
				JOIN teams tm ON tm.id = m1.team_id AND tm.disabled = 0
				JOIN team_members m2 ON m2.team_id = m1.team_id
				WHERE m1.user_id = ?2)
			OR `+report+`)
		ORDER BY r.start_date, u.name, r.id`,
		viewer.DN, viewer.ID, boolInt(viewer.Admin), start, end, viewer.Department)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Away
	for rows.Next() {
		var a Away
		if err := rows.Scan(&a.UserID, &a.Name, &a.Start, &a.End, &a.Status, &a.LeaveType); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
